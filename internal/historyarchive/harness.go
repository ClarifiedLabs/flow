package historyarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type HarnessOptions struct {
	Limits          Limits
	HarnessBuild    string
	RootSessionID   string
	SensitiveValues [][]byte
}

type harnessStateV5 struct {
	Version  int    `json:"version"`
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Agent    string `json:"agent"`
	Build    struct {
		Version string `json:"version"`
	} `json:"build"`
}
type harnessMeta struct {
	Status string `json:"status"`
}

// ParseHarnessStateV5 validates the supported Harness native state envelope and
// returns the fields used for immutable archive indexing.
func ParseHarnessStateV5(data []byte) (HarnessMember, error) {
	var state harnessStateV5
	if err := json.Unmarshal(data, &state); err != nil {
		return HarnessMember{}, fmt.Errorf("%w: Harness state: %v", ErrInvalidArchive, err)
	}
	if state.Version != SupportedHarnessNativeSchema {
		return HarnessMember{}, fmt.Errorf("%w: Harness native schema %d", ErrUnsupported, state.Version)
	}
	if strings.TrimSpace(state.ID) == "" || len(state.ID) > 255 || strings.TrimSpace(state.Build.Version) == "" || len(state.Build.Version) > 255 {
		return HarnessMember{}, fmt.Errorf("%w: Harness state identity", ErrInvalidArchive)
	}
	model := state.Model
	if model == "" {
		model = state.Provider
	}
	return HarnessMember{NativeSessionID: state.ID, AgentName: state.Agent, Model: model, HarnessBuild: state.Build.Version, NativeSchemaVersion: state.Version, ParseStatus: "parsed"}, nil
}

// WriteHarness archives exactly root (the Flow-managed output session directory).
// It never follows filesystem links or reads sibling session directories.
func WriteHarness(ctx context.Context, dst io.Writer, root string, options HarnessOptions) (Artifact, HarnessManifest, error) {
	limits := options.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := limits.validate(); err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	info, err := os.Lstat(rootAbs)
	if err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Artifact{}, HarnessManifest{}, fmt.Errorf("%w: Harness root is not a directory", ErrUnsafePath)
	}
	rootHandle, err := os.OpenRoot(rootAbs)
	if err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	defer rootHandle.Close()
	var tempPaths []string
	defer func() {
		for _, tempPath := range tempPaths {
			_ = os.Remove(tempPath)
		}
	}()
	spooled := map[string]string{}
	var files []File
	blobs := map[string]blobSource{}
	var total int64
	err = filepath.WalkDir(rootAbs, func(hostPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if hostPath == rootAbs {
			return nil
		}
		rel, err := filepath.Rel(rootAbs, hostPath)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if err := ValidatePath(name, limits.MaxPathBytes); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if err := scanSensitiveStrings(options.SensitiveValues, name); err != nil {
			return err
		}
		if len(files) >= limits.MaxEntries {
			return fmt.Errorf("%w: Harness entries", ErrLimitExceeded)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := rootHandle.Readlink(filepath.FromSlash(name))
			if err != nil {
				return err
			}
			target = filepath.ToSlash(target)
			if err := ValidateLink(name, target, limits.MaxPathBytes); err != nil {
				return err
			}
			if err := scanSensitiveStrings(options.SensitiveValues, target); err != nil {
				return err
			}
			files = append(files, File{Path: name, Type: FileSymlink, Mode: 0600, LinkTarget: target})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: special Harness file %q", ErrUnsafePath, name)
		}
		if info.Size() > limits.MaxFileBytes {
			return fmt.Errorf("%w: Harness file %q", ErrLimitExceeded, name)
		}
		total += info.Size()
		if total > limits.MaxLogicalBytes {
			return fmt.Errorf("%w: Harness logical bytes", ErrLimitExceeded)
		}
		tempPath, digest, err := spoolRegular(rootHandle, name, info, limits.MaxFileBytes, options.SensitiveValues)
		if err != nil {
			return err
		}
		tempPaths = append(tempPaths, tempPath)
		spooled[name] = tempPath
		mode := uint32(0600)
		if info.Mode()&0111 != 0 {
			mode = 0700
		}
		file := File{Path: name, Type: FileRegular, Mode: mode, Size: info.Size(), SHA256: digest, Blob: blobName(digest)}
		files = append(files, file)
		if _, ok := blobs[file.Blob]; !ok {
			expected := digest
			size := info.Size()
			sourcePath := tempPath
			blobs[file.Blob] = blobSource{size: size, open: func() (io.ReadCloser, error) {
				f, err := os.Open(sourcePath)
				if err != nil {
					return nil, err
				}
				return &verifyingReadCloser{ReadCloser: f, expected: expected, remaining: size, h: sha256.New()}, nil
			}}
		}
		return nil
	})
	if err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	members, err := parseHarnessMembers(spooled, files, limits, options.SensitiveValues)
	if err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	if len(members) == 0 {
		return Artifact{}, HarnessManifest{}, fmt.Errorf("%w: missing root state.json", ErrInvalidArchive)
	}
	rootID := members[0].NativeSessionID
	if options.RootSessionID != "" && options.RootSessionID != rootID {
		return Artifact{}, HarnessManifest{}, fmt.Errorf("%w: root session ID mismatch", ErrInvalidArchive)
	}
	build := options.HarnessBuild
	if build == "" {
		build = members[0].HarnessBuild
	}
	for _, member := range members {
		if member.HarnessBuild != build {
			return Artifact{}, HarnessManifest{}, fmt.Errorf("%w: mixed Harness builds", ErrUnsupported)
		}
	}
	manifest := HarnessManifest{Format: "flow-harness-archive", FormatVersion: HarnessFormatVersion, SchemaVersion: HarnessSchemaVersion, HarnessBuild: build, RootSessionID: rootID, Members: members, Files: files}
	if err := validateHarnessManifest(&manifest, limits); err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	encoded, err := canonicalJSON(manifest)
	if err != nil {
		return Artifact{}, HarnessManifest{}, err
	}
	logicalBytes := int64(len(encoded))
	for _, file := range files {
		logicalBytes += file.Size
	}
	artifact, err := writeArchive(ctx, dst, limits, HarnessManifestName, encoded, blobs, len(files), logicalBytes)
	return artifact, manifest, err
}

func parseHarnessMembers(spooled map[string]string, files []File, limits Limits, sensitiveValues [][]byte) ([]HarnessMember, error) {
	regular := map[string]bool{}
	for _, f := range files {
		if f.Type == FileRegular {
			regular[f.Path] = true
		}
	}
	var statePaths []string
	for name := range regular {
		if name == "state.json" || strings.HasSuffix(name, "/state.json") {
			dir := path.Dir(name)
			if dir == "." || isChildSessionDir(dir) {
				statePaths = append(statePaths, name)
			}
		}
	}
	sort.Slice(statePaths, func(i, j int) bool {
		di, dj := strings.Count(statePaths[i], "/"), strings.Count(statePaths[j], "/")
		if di != dj {
			return di < dj
		}
		return statePaths[i] < statePaths[j]
	})
	if len(statePaths) == 0 || statePaths[0] != "state.json" {
		return nil, fmt.Errorf("%w: root state.json", ErrInvalidArchive)
	}
	idsByDir := map[string]string{}
	ids := map[string]bool{}
	members := make([]HarnessMember, 0, len(statePaths))
	for _, name := range statePaths {
		data, err := os.ReadFile(spooled[name])
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > limits.MaxFileBytes {
			return nil, fmt.Errorf("%w: state.json", ErrLimitExceeded)
		}
		member, err := ParseHarnessStateV5(data)
		if err != nil {
			return nil, err
		}
		if err := scanSensitiveStrings(sensitiveValues, member.NativeSessionID, member.AgentName, member.Model, member.HarnessBuild); err != nil {
			return nil, err
		}
		if ids[member.NativeSessionID] {
			return nil, fmt.Errorf("%w: duplicate native session ID", ErrInvalidArchive)
		}
		ids[member.NativeSessionID] = true
		dir := path.Dir(name)
		member.RelativeMemberPath = name
		if dir == "." {
			member.MemberKind = "root"
		} else {
			member.MemberKind = "delegated_child"
			parentDir := parentChildSessionDir(dir)
			parentID, ok := idsByDir[parentDir]
			if !ok {
				return nil, fmt.Errorf("%w: missing parent for %q", ErrInvalidArchive, name)
			}
			member.NativeParentSessionID = parentID
		}
		metaPath := path.Join(dir, "meta.json")
		if regular[metaPath] {
			var meta harnessMeta
			if data, err := os.ReadFile(spooled[metaPath]); err == nil && json.Unmarshal(data, &meta) == nil {
				member.Status = meta.Status
			}
		}
		if err := scanSensitiveStrings(sensitiveValues, member.RelativeMemberPath, member.NativeParentSessionID, member.Status); err != nil {
			return nil, err
		}
		idsByDir[dir] = member.NativeSessionID
		members = append(members, member)
	}
	return members, nil
}
func isChildSessionDir(dir string) bool {
	parts := strings.Split(dir, "/")
	if len(parts)%2 != 0 {
		return false
	}
	for i := 0; i < len(parts); i += 2 {
		if parts[i] != "children" || parts[i+1] == "" {
			return false
		}
	}
	return true
}
func parentChildSessionDir(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) <= 2 {
		return "."
	}
	return strings.Join(parts[:len(parts)-2], "/")
}

func spoolRegular(root *os.Root, name string, expected os.FileInfo, max int64, secrets [][]byte) (string, string, error) {
	input, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return "", "", err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil {
		return "", "", err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) || opened.Size() < 0 || opened.Size() > max {
		return "", "", fmt.Errorf("%w: source changed or is not regular: %q", ErrUnsafePath, name)
	}
	temp, err := os.CreateTemp("", ".flow-history-source-*")
	if err != nil {
		return "", "", err
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0600); err != nil {
		return "", "", err
	}
	h := sha256.New()
	scanner := newSecretScanner(secrets)
	n, err := io.Copy(io.MultiWriter(temp, h, scanner), io.LimitReader(input, max+1))
	if err != nil {
		return "", "", err
	}
	if n != opened.Size() || n > max {
		return "", "", fmt.Errorf("%w: source size changed: %q", ErrInvalidArchive, name)
	}
	if scanner.found {
		return "", "", ErrSensitiveContent
	}
	if err := temp.Close(); err != nil {
		return "", "", err
	}
	keep = true
	return tempPath, hex.EncodeToString(h.Sum(nil)), nil
}

type secretScanner struct {
	values [][]byte
	tail   []byte
	found  bool
}

func newSecretScanner(v [][]byte) *secretScanner { return &secretScanner{values: v} }
func (s *secretScanner) Write(p []byte) (int, error) {
	combined := append(append([]byte{}, s.tail...), p...)
	max := 0
	for _, v := range s.values {
		if len(v) == 0 {
			continue
		}
		if len(v) > max {
			max = len(v)
		}
		if strings.Contains(string(combined), string(v)) {
			s.found = true
		}
	}
	if max > 1 && len(combined) >= max-1 {
		s.tail = append([]byte{}, combined[len(combined)-(max-1):]...)
	} else {
		s.tail = combined
	}
	return len(p), nil
}

type verifyingReadCloser struct {
	io.ReadCloser
	expected  string
	remaining int64
	h         hash.Hash
	done      bool
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	if v.remaining <= 0 {
		var one [1]byte
		n, e := v.ReadCloser.Read(one[:])
		if n != 0 {
			return 0, fmt.Errorf("%w: source grew", ErrInvalidArchive)
		}
		if !v.done {
			v.done = true
			if hex.EncodeToString(v.h.Sum(nil)) != v.expected {
				return 0, ErrDigestMismatch
			}
		}
		if e == nil {
			e = io.EOF
		}
		return 0, e
	}
	if int64(len(p)) > v.remaining {
		p = p[:v.remaining]
	}
	n, e := v.ReadCloser.Read(p)
	if n > 0 {
		_, _ = v.h.Write(p[:n])
		v.remaining -= int64(n)
	}
	if errors.Is(e, io.EOF) && v.remaining != 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, e
}
