package historyarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type WorkspaceOptions struct {
	Limits          Limits
	BaseRef         string
	GitPath         string
	SensitiveValues [][]byte
}

type gitRunner struct {
	ctx       context.Context
	repo, git string
	max       int64
}

func (g gitRunner) run(args ...string) ([]byte, error) { return g.runInput(nil, false, args...) }
func (g gitRunner) optional(args ...string) ([]byte, bool, error) {
	b, err := g.runInput(nil, true, args...)
	if err == nil {
		return b, true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, false, nil
	}
	return nil, false, err
}
func (g gitRunner) runInput(input []byte, allowExit bool, args ...string) ([]byte, error) {
	argv := append([]string{"-C", g.repo}, args...)
	cmd := exec.CommandContext(g.ctx, g.git, argv...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = bytes.NewReader(input)
	stdout := &boundedBuffer{remain: g.max}
	stderr := &boundedBuffer{remain: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		if errors.Is(stdout.err, ErrLimitExceeded) {
			return nil, stdout.err
		}
		if allowExit {
			return nil, err
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.buf.String()))
	}
	return stdout.buf.Bytes(), nil
}

type boundedBuffer struct {
	buf    bytes.Buffer
	remain int64
	err    error
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remain {
		b.err = fmt.Errorf("%w: Git output", ErrLimitExceeded)
		return 0, b.err
	}
	n, e := b.buf.Write(p)
	b.remain -= int64(n)
	return n, e
}

// WriteWorkspace captures reconstructive Git state. Repository discovery and
// inventory use Git argv only; no shell command or repository walk is used.
func WriteWorkspace(ctx context.Context, dst io.Writer, repo string, options WorkspaceOptions) (Artifact, WorkspaceManifest, error) {
	limits := options.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := limits.validate(); err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	git := options.GitPath
	if git == "" {
		git = "git"
	}
	runner := gitRunner{ctx: ctx, repo: repo, git: git, max: limits.MaxLogicalBytes}
	if sparse, ok, err := runner.optional("config", "--bool", "core.sparseCheckout"); err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	} else if ok && strings.TrimSpace(string(sparse)) == "true" {
		return Artifact{}, WorkspaceManifest{}, fmt.Errorf("%w: sparse checkout", ErrUnsupported)
	}
	headBytes, err := runner.run("rev-parse", "--verify", "HEAD")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	head := strings.TrimSpace(string(headBytes))
	branchBytes, onBranch, err := runner.optional("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	branch := strings.TrimSpace(string(branchBytes))
	base := head
	if options.BaseRef != "" {
		baseBytes, err := runner.run("rev-parse", "--verify", options.BaseRef+"^{commit}")
		if err != nil {
			return Artifact{}, WorkspaceManifest{}, err
		}
		base = strings.TrimSpace(string(baseBytes))
	}
	objectBytes, err := runner.run("rev-parse", "--show-object-format")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	repoFormatBytes, ok, err := runner.optional("config", "--get", "core.repositoryformatversion")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	repoFormat := "0"
	if ok {
		repoFormat = strings.TrimSpace(string(repoFormatBytes))
	}
	staged, err := runner.run("diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	unstaged, err := runner.run("diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	if err := scanSensitive(staged, options.SensitiveValues); err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	if err := scanSensitive(unstaged, options.SensitiveValues); err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	stagedNames, err := runner.run("diff", "--cached", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	unstagedNames, err := runner.run("diff", "--name-only", "-z", "--")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	stagedPaths, err := parseNULPaths(stagedNames, limits)
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	unstagedPaths, err := parseNULPaths(unstagedNames, limits)
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	indexRaw, err := runner.run("ls-files", "--stage", "-z")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	index, err := parseIndex(indexRaw, limits)
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	untrackedRaw, err := runner.run("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	untrackedPaths, err := parseNULPaths(untrackedRaw, limits)
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	rootHandle, err := os.OpenRoot(repo)
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	defer rootHandle.Close()
	var tempPaths []string
	defer func() {
		for _, tempPath := range tempPaths {
			_ = os.Remove(tempPath)
		}
	}()
	files := make([]File, 0, len(untrackedPaths))
	blobs := map[string]blobSource{}
	for _, name := range untrackedPaths {
		if isGitMetadataPath(name) || deniedFlowPath(name) {
			continue
		}
		if err := noSymlinkParents(repo, name); err != nil {
			return Artifact{}, WorkspaceManifest{}, err
		}
		info, err := rootHandle.Lstat(filepath.FromSlash(name))
		if err != nil {
			return Artifact{}, WorkspaceManifest{}, err
		}
		if info.IsDir() {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := rootHandle.Readlink(filepath.FromSlash(name))
			if err != nil {
				return Artifact{}, WorkspaceManifest{}, err
			}
			target = filepath.ToSlash(target)
			if err := ValidateLink(name, target, limits.MaxPathBytes); err != nil {
				return Artifact{}, WorkspaceManifest{}, err
			}
			files = append(files, File{Path: name, Type: FileSymlink, Mode: 0600, LinkTarget: target})
			continue
		}
		if !info.Mode().IsRegular() {
			return Artifact{}, WorkspaceManifest{}, fmt.Errorf("%w: special untracked file %q", ErrUnsafePath, name)
		}
		tempPath, digest, err := spoolRegular(rootHandle, name, info, limits.MaxFileBytes, options.SensitiveValues)
		if err != nil {
			return Artifact{}, WorkspaceManifest{}, err
		}
		tempPaths = append(tempPaths, tempPath)
		mode := uint32(0600)
		if info.Mode()&0111 != 0 {
			mode = 0700
		}
		f := File{Path: name, Type: FileRegular, Mode: mode, Size: info.Size(), SHA256: digest, Blob: blobName(digest)}
		files = append(files, f)
		if _, ok := blobs[f.Blob]; !ok {
			sourcePath, size, expected := tempPath, info.Size(), digest
			blobs[f.Blob] = blobSource{size: size, open: func() (io.ReadCloser, error) {
				r, err := os.Open(sourcePath)
				if err != nil {
					return nil, err
				}
				return &verifyingReadCloser{ReadCloser: r, expected: expected, remaining: size, h: sha256.New()}, nil
			}}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	stagedPatch := patchFor(staged)
	unstagedPatch := patchFor(unstaged)
	addBytesBlob(blobs, stagedPatch.Blob, staged)
	addBytesBlob(blobs, unstagedPatch.Blob, unstaged)
	inventory := inventoryDigest(head, indexRaw, stagedNames, unstagedNames, files)
	manifest := WorkspaceManifest{Format: "flow-workspace-archive", FormatVersion: WorkspaceFormatVersion, SchemaVersion: WorkspaceSchemaVersion, HeadCommit: head, Branch: branch, Detached: !onBranch, BaseRef: options.BaseRef, BaseCommit: base, ObjectFormat: strings.TrimSpace(string(objectBytes)), RepositoryFormat: repoFormat, Staged: stagedPatch, Unstaged: unstagedPatch, StagedPaths: stagedPaths, UnstagedPaths: unstagedPaths, Index: index, Untracked: files, InventoryDigest: inventory}
	if err := validateWorkspaceManifest(&manifest, limits); err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	encoded, err := canonicalJSON(manifest)
	if err != nil {
		return Artifact{}, WorkspaceManifest{}, err
	}
	logicalBytes := int64(len(encoded)) + stagedPatch.Size + unstagedPatch.Size
	for _, file := range files {
		logicalBytes += file.Size
	}
	artifact, err := writeArchive(ctx, dst, limits, WorkspaceManifestName, encoded, blobs, len(files)+2, logicalBytes)
	return artifact, manifest, err
}
func patchFor(data []byte) Patch {
	d := digestBytes(data)
	return Patch{Size: int64(len(data)), SHA256: d, Blob: blobName(d)}
}
func addBytesBlob(blobs map[string]blobSource, name string, data []byte) {
	if _, ok := blobs[name]; ok {
		return
	}
	copyData := append([]byte(nil), data...)
	blobs[name] = blobSource{size: int64(len(copyData)), open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(copyData)), nil }}
}
func scanSensitive(data []byte, values [][]byte) error {
	for _, v := range values {
		if len(v) > 0 && bytes.Contains(data, v) {
			return ErrSensitiveContent
		}
	}
	return nil
}
func parseNULPaths(data []byte, l Limits) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("%w: unterminated Git path output", ErrInvalidArchive)
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	paths := make([]string, 0, len(parts))
	seen := map[string]string{}
	for _, p := range parts {
		name := string(p)
		if err := ValidatePath(name, l.MaxPathBytes); err != nil {
			return nil, err
		}
		fold := foldedPath(name)
		if prior, ok := seen[fold]; ok {
			return nil, fmt.Errorf("%w: paths %q and %q", ErrInvalidArchive, prior, name)
		}
		seen[fold] = name
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths, nil
}
func parseIndex(data []byte, l Limits) ([]IndexEntry, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("%w: index output", ErrInvalidArchive)
	}
	records := bytes.Split(data[:len(data)-1], []byte{0})
	out := make([]IndexEntry, 0, len(records))
	for _, record := range records {
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("%w: index record", ErrInvalidArchive)
		}
		fields := strings.Fields(string(record[:tab]))
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: index fields", ErrInvalidArchive)
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 0 || stage > 3 {
			return nil, fmt.Errorf("%w: index stage", ErrInvalidArchive)
		}
		name := string(record[tab+1:])
		if err := ValidatePath(name, l.MaxPathBytes); err != nil {
			return nil, err
		}
		if isGitMetadataPath(name) {
			return nil, fmt.Errorf("%w: .git index path", ErrUnsafePath)
		}
		out = append(out, IndexEntry{Path: name, Mode: fields[0], Object: fields[1], Stage: stage})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Stage < out[j].Stage
	})
	return out, nil
}
func inventoryDigest(head string, index, staged, unstaged []byte, files []File) string {
	h := sha256.New()
	for _, piece := range [][]byte{[]byte(head), {0}, index, {0}, staged, {0}, unstaged, {0}} {
		_, _ = h.Write(piece)
	}
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%o\x00%d\x00%s\x00%s\x00", f.Path, f.Type, f.Mode, f.Size, f.SHA256, f.LinkTarget)
	}
	return hex.EncodeToString(h.Sum(nil))
}
