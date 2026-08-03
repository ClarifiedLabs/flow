package historyarchive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type countingWriter struct {
	w io.Writer
	n int64
	h hash.Hash
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, e := w.w.Write(p)
	w.n += int64(n)
	_, _ = w.h.Write(p[:n])
	return n, e
}

type countingReader struct {
	r io.Reader
	n int64
	h hash.Hash
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, e := r.r.Read(p)
	r.n += int64(n)
	_, _ = r.h.Write(p[:n])
	return n, e
}

type blobSource struct {
	size int64
	open func() (io.ReadCloser, error)
}

func canonicalJSON(value any) ([]byte, error) {
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeArchive(ctx context.Context, dst io.Writer, limits Limits, manifestName string, manifest []byte, blobs map[string]blobSource, logicalEntries int, logicalBytes int64) (Artifact, error) {
	if err := limits.validate(); err != nil {
		return Artifact{}, err
	}
	if int64(len(manifest)) > limits.MaxFileBytes {
		return Artifact{}, fmt.Errorf("%w: manifest", ErrLimitExceeded)
	}
	if logicalBytes < int64(len(manifest)) || logicalBytes > limits.MaxLogicalBytes {
		return Artifact{}, fmt.Errorf("%w: logical bytes", ErrLimitExceeded)
	}
	cw := &countingWriter{w: dst, h: sha256.New()}
	limited := &limitWrite{w: cw, remain: limits.MaxStoredBytes}
	gz, err := gzip.NewWriterLevel(limited, gzip.BestCompression)
	if err != nil {
		return Artifact{}, err
	}
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	writeOne := func(name string, size int64, source io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		h := &tar.Header{Name: name, Mode: 0600, Size: size, Typeflag: tar.TypeReg, Uid: 0, Gid: 0, ModTime: time.Unix(0, 0).UTC(), AccessTime: time.Time{}, ChangeTime: time.Time{}, Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		n, err := io.CopyN(tw, source, size)
		if err != nil {
			return err
		}
		if n != size {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	if err := writeOne(manifestName, int64(len(manifest)), strings.NewReader(string(manifest))); err != nil {
		return Artifact{}, closeWriters(tw, gz, err)
	}
	names := make([]string, 0, len(blobs))
	for name := range blobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := blobs[name]
		if s.size < 0 || s.size > limits.MaxFileBytes {
			return Artifact{}, fmt.Errorf("%w: blob %s", ErrLimitExceeded, name)
		}
		r, err := s.open()
		if err != nil {
			return Artifact{}, closeWriters(tw, gz, err)
		}
		err = writeOne(name, s.size, r)
		closeErr := r.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return Artifact{}, closeWriters(tw, gz, err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return Artifact{}, err
	}
	if err := gz.Close(); err != nil {
		return Artifact{}, err
	}
	kind := ArchiveHarness
	if manifestName == WorkspaceManifestName {
		kind = ArchiveWorkspace
	}
	return Artifact{Kind: kind, SHA256: hex.EncodeToString(cw.h.Sum(nil)), StoredBytes: cw.n, LogicalBytes: logicalBytes, EntryCount: logicalEntries}, nil
}

func closeWriters(tw *tar.Writer, gz *gzip.Writer, cause error) error {
	_ = tw.Close()
	_ = gz.Close()
	return cause
}

type limitWrite struct {
	w      io.Writer
	remain int64
}

func (w *limitWrite) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remain {
		return 0, fmt.Errorf("%w: stored bytes", ErrLimitExceeded)
	}
	n, e := w.w.Write(p)
	w.remain -= int64(n)
	return n, e
}

type scanResult struct {
	inspection Inspection
	blobs      map[string]blobExpectation
}
type blobExpectation struct {
	size   int64
	digest string
}

func Inspect(ctx context.Context, src io.Reader, limits Limits) (Inspection, error) {
	result, err := scanArchive(ctx, src, limits, nil)
	return result.inspection, err
}

type blobSink func(name string, src io.Reader, size int64) error

func scanArchive(ctx context.Context, src io.Reader, limits Limits, sink blobSink) (scanResult, error) {
	if err := limits.validate(); err != nil {
		return scanResult{}, err
	}
	cr := &countingReader{r: io.LimitReader(src, limits.MaxStoredBytes+1), h: sha256.New()}
	buffered := bufio.NewReader(cr)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return scanResult{}, fmt.Errorf("%w: gzip: %v", ErrInvalidArchive, err)
	}
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	expected := map[string]blobExpectation{}
	harnessStateBlobs := map[string]HarnessMember{}
	seen := map[string]bool{}
	var result scanResult
	var physicalBytes int64
	var logicalBytes int64
	physical := 0
	previous := ""
	for {
		if err := ctx.Err(); err != nil {
			return scanResult{}, err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return scanResult{}, fmt.Errorf("%w: tar: %v", ErrInvalidArchive, err)
		}
		physical++
		if physical > limits.MaxEntries+1 {
			return scanResult{}, fmt.Errorf("%w: tar entries", ErrLimitExceeded)
		}
		if header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Mode != 0600 || header.Uid != 0 || header.Gid != 0 || header.Linkname != "" || !header.ModTime.Equal(time.Unix(0, 0).UTC()) {
			return scanResult{}, fmt.Errorf("%w: non-canonical header %q", ErrInvalidArchive, header.Name)
		}
		if err := ValidatePath(header.Name, limits.MaxPathBytes); err != nil {
			return scanResult{}, err
		}
		if header.Size < 0 || header.Size > limits.MaxFileBytes {
			return scanResult{}, fmt.Errorf("%w: file %q", ErrLimitExceeded, header.Name)
		}
		physicalBytes += header.Size
		if physicalBytes > limits.MaxLogicalBytes {
			return scanResult{}, fmt.Errorf("%w: physical logical bytes", ErrLimitExceeded)
		}
		if seen[header.Name] || physical > 2 && header.Name <= previous {
			return scanResult{}, fmt.Errorf("%w: duplicate/unsorted member %q", ErrInvalidArchive, header.Name)
		}
		seen[header.Name] = true
		previous = header.Name
		if physical == 1 {
			data, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
			if err != nil || int64(len(data)) != header.Size {
				return scanResult{}, fmt.Errorf("%w: manifest read", ErrInvalidArchive)
			}
			switch header.Name {
			case HarnessManifestName:
				var m HarnessManifest
				if err := strictJSON(data, &m); err != nil {
					return scanResult{}, err
				}
				if err := validateHarnessManifest(&m, limits); err != nil {
					return scanResult{}, err
				}
				result.inspection.Harness = &m
				result.inspection.Kind = ArchiveHarness
				fileByPath := make(map[string]File, len(m.Files))
				for _, f := range m.Files {
					fileByPath[f.Path] = f
					if f.Type == FileRegular {
						expected[f.Blob] = blobExpectation{f.Size, f.SHA256}
					}
				}
				for _, member := range m.Members {
					file := fileByPath[member.RelativeMemberPath]
					if file.Type != FileRegular || file.Size > 1<<20 {
						return scanResult{}, fmt.Errorf("%w: Harness member state file", ErrInvalidArchive)
					}
					harnessStateBlobs[file.Blob] = member
				}
				logicalBytes = int64(len(data))
				for _, file := range m.Files {
					logicalBytes += file.Size
				}
				result.inspection.EntryCount = len(m.Files)
			case WorkspaceManifestName:
				var m WorkspaceManifest
				if err := strictJSON(data, &m); err != nil {
					return scanResult{}, err
				}
				if err := validateWorkspaceManifest(&m, limits); err != nil {
					return scanResult{}, err
				}
				result.inspection.Workspace = &m
				result.inspection.Kind = ArchiveWorkspace
				expected[m.Staged.Blob] = blobExpectation{m.Staged.Size, m.Staged.SHA256}
				expected[m.Unstaged.Blob] = blobExpectation{m.Unstaged.Size, m.Unstaged.SHA256}
				for _, f := range m.Untracked {
					if f.Type == FileRegular {
						expected[f.Blob] = blobExpectation{f.Size, f.SHA256}
					}
				}
				logicalBytes = int64(len(data)) + m.Staged.Size + m.Unstaged.Size
				for _, file := range m.Untracked {
					logicalBytes += file.Size
				}
				result.inspection.EntryCount = len(m.Untracked) + 2
			default:
				return scanResult{}, fmt.Errorf("%w: manifest must be first", ErrInvalidArchive)
			}
			if logicalBytes > limits.MaxLogicalBytes {
				return scanResult{}, fmt.Errorf("%w: logical bytes", ErrLimitExceeded)
			}
			continue
		}
		expect, ok := expected[header.Name]
		if !ok {
			return scanResult{}, fmt.Errorf("%w: unexpected member %q", ErrInvalidArchive, header.Name)
		}
		if expect.size != header.Size {
			return scanResult{}, fmt.Errorf("%w: size for %q", ErrDigestMismatch, header.Name)
		}
		h := sha256.New()
		reader := io.Reader(io.TeeReader(tr, h))
		var state bytes.Buffer
		if _, captureState := harnessStateBlobs[header.Name]; captureState {
			reader = io.TeeReader(reader, &state)
		}
		if sink != nil {
			err = sink(header.Name, reader, header.Size)
		} else {
			_, err = io.Copy(io.Discard, reader)
		}
		if err != nil {
			return scanResult{}, err
		}
		if hex.EncodeToString(h.Sum(nil)) != expect.digest {
			return scanResult{}, fmt.Errorf("%w: %q", ErrDigestMismatch, header.Name)
		}
		if member, ok := harnessStateBlobs[header.Name]; ok {
			parsed, parseErr := ParseHarnessStateV5(state.Bytes())
			if parseErr != nil || parsed.NativeSessionID != member.NativeSessionID || parsed.AgentName != member.AgentName || parsed.Model != member.Model || parsed.HarnessBuild != member.HarnessBuild || parsed.NativeSchemaVersion != member.NativeSchemaVersion {
				return scanResult{}, fmt.Errorf("%w: Harness member state does not match manifest", ErrInvalidArchive)
			}
			delete(harnessStateBlobs, header.Name)
		}
		delete(expected, header.Name)
	}
	if physical == 0 || len(expected) != 0 || len(harnessStateBlobs) != 0 {
		return scanResult{}, fmt.Errorf("%w: missing archive members", ErrInvalidArchive)
	}
	trailing, copyErr := io.Copy(io.Discard, gz)
	if copyErr != nil {
		return scanResult{}, fmt.Errorf("%w: gzip checksum: %v", ErrInvalidArchive, copyErr)
	}
	if trailing != 0 {
		return scanResult{}, fmt.Errorf("%w: data after tar end", ErrInvalidArchive)
	}
	if err := gz.Close(); err != nil {
		return scanResult{}, fmt.Errorf("%w: gzip close: %v", ErrInvalidArchive, err)
	}
	if _, err := buffered.ReadByte(); err == nil {
		return scanResult{}, fmt.Errorf("%w: trailing gzip data", ErrInvalidArchive)
	} else if !errors.Is(err, io.EOF) {
		return scanResult{}, err
	}
	if cr.n > limits.MaxStoredBytes {
		return scanResult{}, fmt.Errorf("%w: stored bytes", ErrLimitExceeded)
	}
	result.inspection.SHA256 = hex.EncodeToString(cr.h.Sum(nil))
	result.inspection.StoredBytes = cr.n
	result.inspection.LogicalBytes = logicalBytes
	result.blobs = expected
	return result, nil
}

func strictJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrInvalidArchive, err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing manifest JSON", ErrInvalidArchive)
	}
	canonical, err := canonicalJSON(dst)
	if err != nil || !bytes.Equal(data, canonical) {
		return fmt.Errorf("%w: non-canonical or duplicate manifest fields", ErrInvalidArchive)
	}
	return nil
}

func validateHarnessManifest(m *HarnessManifest, l Limits) error {
	if m.Format != "flow-harness-archive" || m.FormatVersion != HarnessFormatVersion || m.SchemaVersion != HarnessSchemaVersion {
		return fmt.Errorf("%w: Harness format/schema", ErrUnsupported)
	}
	if m.HarnessBuild == "" || m.RootSessionID == "" || len(m.Members) == 0 {
		return fmt.Errorf("%w: Harness identity", ErrInvalidArchive)
	}
	if err := validateFiles(m.Files, l); err != nil {
		return err
	}
	roots := 0
	ids := map[string]bool{}
	membersByPath := map[string]HarnessMember{}
	for _, member := range m.Members {
		if member.NativeSchemaVersion != SupportedHarnessNativeSchema || member.ParseStatus != "parsed" || member.NativeSessionID == "" || member.HarnessBuild != m.HarnessBuild {
			return fmt.Errorf("%w: Harness member", ErrInvalidArchive)
		}
		if ids[member.NativeSessionID] {
			return fmt.Errorf("%w: duplicate native session", ErrInvalidArchive)
		}
		ids[member.NativeSessionID] = true
		if member.MemberKind == "root" {
			roots++
			if member.NativeSessionID != m.RootSessionID || member.NativeParentSessionID != "" {
				return fmt.Errorf("%w: Harness root", ErrInvalidArchive)
			}
		} else if member.MemberKind != "delegated_child" || member.NativeParentSessionID == "" {
			return fmt.Errorf("%w: Harness child", ErrInvalidArchive)
		}
		if err := ValidatePath(member.RelativeMemberPath, l.MaxPathBytes); err != nil {
			return err
		}
		if path.Base(member.RelativeMemberPath) != "state.json" {
			return fmt.Errorf("%w: Harness member path", ErrInvalidArchive)
		}
		if _, exists := membersByPath[member.RelativeMemberPath]; exists {
			return fmt.Errorf("%w: duplicate Harness member path", ErrInvalidArchive)
		}
		membersByPath[member.RelativeMemberPath] = member
	}
	if roots != 1 {
		return fmt.Errorf("%w: expected one Harness root", ErrInvalidArchive)
	}
	for _, member := range m.Members {
		dir := path.Dir(member.RelativeMemberPath)
		if member.MemberKind == "root" {
			if member.RelativeMemberPath != "state.json" {
				return fmt.Errorf("%w: Harness root path", ErrInvalidArchive)
			}
			continue
		}
		if !isChildSessionDir(dir) {
			return fmt.Errorf("%w: Harness child path", ErrInvalidArchive)
		}
		parentPath := "state.json"
		if parentDir := parentChildSessionDir(dir); parentDir != "." {
			parentPath = path.Join(parentDir, "state.json")
		}
		parent, ok := membersByPath[parentPath]
		if !ok || parent.NativeSessionID != member.NativeParentSessionID || !ids[member.NativeParentSessionID] {
			return fmt.Errorf("%w: missing or mismatched Harness parent", ErrInvalidArchive)
		}
	}
	return nil
}

func validatePatch(p Patch, l Limits) error {
	if p.Size < 0 || p.Size > l.MaxFileBytes || !validDigest(p.SHA256) || p.Blob != blobName(p.SHA256) {
		return fmt.Errorf("%w: patch", ErrInvalidArchive)
	}
	return nil
}
func validateWorkspaceManifest(m *WorkspaceManifest, l Limits) error {
	if m.Format != "flow-workspace-archive" || m.FormatVersion != WorkspaceFormatVersion || m.SchemaVersion != WorkspaceSchemaVersion {
		return fmt.Errorf("%w: workspace format/schema", ErrUnsupported)
	}
	if m.HeadCommit == "" || m.BaseCommit == "" || m.ObjectFormat == "" || m.RepositoryFormat == "" || !validDigest(m.InventoryDigest) {
		return fmt.Errorf("%w: workspace identity", ErrInvalidArchive)
	}
	if err := validatePatch(m.Staged, l); err != nil {
		return err
	}
	if err := validatePatch(m.Unstaged, l); err != nil {
		return err
	}
	if err := validateFiles(m.Untracked, l); err != nil {
		return err
	}
	for _, p := range append(append([]string{}, m.StagedPaths...), m.UnstagedPaths...) {
		if err := ValidatePath(p, l.MaxPathBytes); err != nil {
			return err
		}
		if isGitMetadataPath(p) {
			return fmt.Errorf("%w: .git path", ErrUnsafePath)
		}
	}
	for _, f := range m.Untracked {
		if isGitMetadataPath(f.Path) || deniedFlowPath(f.Path) {
			return fmt.Errorf("%w: denied workspace path", ErrUnsafePath)
		}
	}
	return nil
}

// Extract validates src completely into a private sibling staging directory and
// atomically publishes destination. Archive tar links are always rejected;
// validated logical links are created only after all regular files.
func Extract(ctx context.Context, src io.Reader, destination string, limits Limits) (Inspection, error) {
	parent := filepath.Dir(destination)
	if _, err := os.Lstat(destination); err == nil {
		return Inspection{}, ErrDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return Inspection{}, err
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return Inspection{}, err
	}
	archive, err := os.CreateTemp(parent, ".history-archive-*")
	if err != nil {
		return Inspection{}, err
	}
	archiveName := archive.Name()
	defer os.Remove(archiveName)
	_ = archive.Chmod(0600)
	n, err := io.Copy(archive, io.LimitReader(src, limits.MaxStoredBytes+1))
	if err == nil && n > limits.MaxStoredBytes {
		err = fmt.Errorf("%w: stored bytes", ErrLimitExceeded)
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Inspection{}, err
	}
	f, err := os.Open(archiveName)
	if err != nil {
		return Inspection{}, err
	}
	inspection, err := Inspect(ctx, f, limits)
	_ = f.Close()
	if err != nil {
		return Inspection{}, err
	}
	stage, err := os.MkdirTemp(parent, ".history-stage-*")
	if err != nil {
		return Inspection{}, err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0700); err != nil {
		return Inspection{}, err
	}
	root, err := os.OpenRoot(stage)
	if err != nil {
		return Inspection{}, err
	}
	defer root.Close()
	if err := root.Mkdir(".payload", 0700); err != nil {
		return Inspection{}, err
	}
	f, err = os.Open(archiveName)
	if err != nil {
		return Inspection{}, err
	}
	_, err = scanArchive(ctx, f, limits, func(name string, r io.Reader, size int64) error {
		out, err := root.OpenFile(filepath.FromSlash(".payload/"+strings.TrimPrefix(name, "blobs/")), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, r, size)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	_ = f.Close()
	if err != nil {
		return Inspection{}, err
	}
	copyPayload := func(blob, dest string, mode uint32) error {
		if err := mkdirParents(root, dest); err != nil {
			return err
		}
		in, err := root.Open(filepath.FromSlash(".payload/" + strings.TrimPrefix(blob, "blobs/")))
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := root.OpenFile(filepath.FromSlash(dest), os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(mode))
		if err != nil {
			return err
		}
		_, e := io.Copy(out, in)
		ce := out.Close()
		if e != nil {
			return e
		}
		return ce
	}
	var files []File
	if inspection.Harness != nil {
		files = inspection.Harness.Files
	} else {
		if err := copyPayload(inspection.Workspace.Staged.Blob, "patches/staged.patch", 0600); err != nil {
			return Inspection{}, err
		}
		if err := copyPayload(inspection.Workspace.Unstaged.Blob, "patches/unstaged.patch", 0600); err != nil {
			return Inspection{}, err
		}
		files = inspection.Workspace.Untracked
	}
	for _, file := range files {
		if file.Type != FileRegular {
			continue
		}
		dest := file.Path
		if inspection.Workspace != nil {
			dest = "untracked/" + dest
		}
		if err := copyPayload(file.Blob, dest, file.Mode); err != nil {
			return Inspection{}, err
		}
	}
	for _, file := range files {
		if file.Type != FileSymlink {
			continue
		}
		dest := file.Path
		if inspection.Workspace != nil {
			dest = "untracked/" + dest
		}
		if err := mkdirParents(root, dest); err != nil {
			return Inspection{}, err
		}
		target := file.LinkTarget
		if err := root.Symlink(target, filepath.FromSlash(dest)); err != nil {
			return Inspection{}, err
		}
	}
	if err := root.RemoveAll(".payload"); err != nil {
		return Inspection{}, err
	}
	if err := root.Close(); err != nil {
		return Inspection{}, err
	}
	if err := os.Rename(stage, destination); err != nil {
		return Inspection{}, err
	}
	return inspection, nil
}
func mkdirParents(root *os.Root, name string) error {
	dir := filepath.Dir(filepath.FromSlash(name))
	if dir == "." {
		return nil
	}
	return root.MkdirAll(dir, 0700)
}
