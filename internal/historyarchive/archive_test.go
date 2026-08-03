package historyarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHarnessArchiveDeterministicInspectAndExtract(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "state.json"), stateJSON("root-native", "author"), 0600)
	writeTestFile(t, filepath.Join(root, "tree.ndjson"), []byte("tree\n"), 0600)
	writeTestFile(t, filepath.Join(root, "children", "child-op", "state.json"), stateJSON("child-native", "explore"), 0600)
	writeTestFile(t, filepath.Join(root, "children", "child-op", "meta.json"), []byte(`{"status":"completed"}`), 0600)
	writeTestFile(t, filepath.Join(root, "artifacts", "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	if err := os.Symlink("run.sh", filepath.Join(root, "artifacts", "latest")); err != nil {
		t.Fatal(err)
	}

	options := HarnessOptions{Limits: testLimits(), HarnessBuild: "v0.4.5", RootSessionID: "root-native"}
	var first, second bytes.Buffer
	artifact, manifest, err := WriteHarness(context.Background(), &first, root, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteHarness(context.Background(), &second, root, options); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("Harness archives are not deterministic")
	}
	if artifact.StoredBytes != int64(first.Len()) || artifact.EntryCount != len(manifest.Files) || len(manifest.Members) != 2 {
		t.Fatalf("artifact=%+v members=%+v", artifact, manifest.Members)
	}
	if manifest.Members[0].MemberKind != "root" || manifest.Members[1].NativeParentSessionID != "root-native" || manifest.Members[1].Status != "completed" {
		t.Fatalf("members=%+v", manifest.Members)
	}
	inspection, err := Inspect(context.Background(), bytes.NewReader(first.Bytes()), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Harness == nil || inspection.SHA256 != artifact.SHA256 || inspection.Harness.RootSessionID != "root-native" {
		t.Fatalf("inspection=%+v", inspection)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if _, err := Extract(context.Background(), bytes.NewReader(first.Bytes()), destination, testLimits()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "children", "child-op", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, stateJSON("child-native", "explore")) {
		t.Fatal("restored state differs")
	}
	link, err := os.Readlink(filepath.Join(destination, "artifacts", "latest"))
	if err != nil || link != "run.sh" {
		t.Fatalf("link=%q err=%v", link, err)
	}
	if _, err := Extract(context.Background(), bytes.NewReader(first.Bytes()), destination, testLimits()); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("existing destination error=%v", err)
	}
}

func TestHarnessCaptureRejectsUnsupportedUnsafeAndSensitiveInputs(t *testing.T) {
	t.Run("schema", func(t *testing.T) {
		if _, err := ParseHarnessStateV5([]byte(`{"version":4,"id":"old","build":{"version":"v0"}}`)); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("escaping link", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "state.json"), stateJSON("root", "author"), 0600)
		if err := os.Symlink("../../secret", filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		_, _, err := WriteHarness(context.Background(), io.Discard, root, HarnessOptions{Limits: testLimits(), HarnessBuild: "v0.4.5"})
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("portable collision", func(t *testing.T) {
		digest := strings.Repeat("0", 64)
		err := validateFiles([]File{
			{Path: "A", Type: FileRegular, Mode: 0600, SHA256: digest, Blob: blobName(digest)},
			{Path: "a", Type: FileRegular, Mode: 0600, SHA256: digest, Blob: blobName(digest)},
		}, testLimits())
		if !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("secret across scanner writes", func(t *testing.T) {
		s := newSecretScanner([][]byte{[]byte("top-secret")})
		_, _ = s.Write([]byte("top-"))
		_, _ = s.Write([]byte("secret"))
		if !s.found {
			t.Fatal("secret was not found across writes")
		}
	})
}

func TestValidatePathAndLinksAdversarial(t *testing.T) {
	for _, name := range []string{"../x", "a/../x", "/abs", `a\\b`, "C:/x", "a//b", "a/./b", ".git/config", "nul\x00x"} {
		err := ValidatePath(name, 100)
		if name == ".git/config" {
			if err != nil {
				t.Fatalf("generic .git path unexpectedly rejected: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("ValidatePath(%q) succeeded", name)
		}
	}
	for _, target := range []string{"../../escape", "/abs", `..\\escape`, "C:/escape"} {
		if err := ValidateLink("dir/link", target, 100); err == nil {
			t.Errorf("ValidateLink target %q succeeded", target)
		}
	}
	if err := ValidateLink("dir/link", "../safe", 100); err != nil {
		t.Fatal(err)
	}
}

func TestInspectRejectsManifestSpoofingAndLogicalFanout(t *testing.T) {
	t.Run("duplicate JSON member", func(t *testing.T) {
		var manifest HarnessManifest
		if err := strictJSON([]byte("{\"format\":\"one\",\"format\":\"two\"}\n"), &manifest); !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("duplicate manifest field error=%v", err)
		}
	})

	t.Run("Harness state disagrees with manifest", func(t *testing.T) {
		state := stateJSON("native", "author")
		digest := digestBytes(state)
		manifest := HarnessManifest{
			Format: "flow-harness-archive", FormatVersion: HarnessFormatVersion, SchemaVersion: HarnessSchemaVersion,
			HarnessBuild: "v0.4.5", RootSessionID: "native",
			Members: []HarnessMember{{NativeSessionID: "native", RelativeMemberPath: "state.json", MemberKind: "root", AgentName: "spoofed", Model: "p:m", HarnessBuild: "v0.4.5", NativeSchemaVersion: 5, ParseStatus: "parsed"}},
			Files:   []File{{Path: "state.json", Type: FileRegular, Mode: 0600, Size: int64(len(state)), SHA256: digest, Blob: blobName(digest)}},
		}
		encoded, err := canonicalJSON(manifest)
		if err != nil {
			t.Fatal(err)
		}
		var archive bytes.Buffer
		blobs := map[string]blobSource{blobName(digest): {size: int64(len(state)), open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(state)), nil
		}}}
		if _, err := writeArchive(context.Background(), &archive, testLimits(), HarnessManifestName, encoded, blobs, 1, int64(len(encoded)+len(state))); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(context.Background(), bytes.NewReader(archive.Bytes()), testLimits()); !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("spoofed member error=%v", err)
		}
	})

	t.Run("deduplicated blob logical fanout", func(t *testing.T) {
		payload := []byte("shared-payload")
		digest := digestBytes(payload)
		emptyDigest := digestBytes(nil)
		manifest := WorkspaceManifest{
			Format: "flow-workspace-archive", FormatVersion: WorkspaceFormatVersion, SchemaVersion: WorkspaceSchemaVersion,
			HeadCommit: strings.Repeat("1", 40), BaseCommit: strings.Repeat("1", 40), ObjectFormat: "sha1", RepositoryFormat: "0",
			Staged: Patch{SHA256: emptyDigest, Blob: blobName(emptyDigest)}, Unstaged: Patch{SHA256: emptyDigest, Blob: blobName(emptyDigest)},
			Untracked: []File{
				{Path: "one", Type: FileRegular, Mode: 0600, Size: int64(len(payload)), SHA256: digest, Blob: blobName(digest)},
				{Path: "two", Type: FileRegular, Mode: 0600, Size: int64(len(payload)), SHA256: digest, Blob: blobName(digest)},
			},
			InventoryDigest: strings.Repeat("2", 64),
		}
		encoded, err := canonicalJSON(manifest)
		if err != nil {
			t.Fatal(err)
		}
		blobs := map[string]blobSource{
			blobName(emptyDigest): {open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(nil)), nil }},
			blobName(digest): {size: int64(len(payload)), open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(payload)), nil
			}},
		}
		var archive bytes.Buffer
		if _, err := writeArchive(context.Background(), &archive, testLimits(), WorkspaceManifestName, encoded, blobs, 4, int64(len(encoded)+len(payload))); err != nil {
			t.Fatal(err)
		}
		limits := testLimits()
		limits.MaxLogicalBytes = int64(len(encoded) + len(payload) + 1)
		if _, err := Inspect(context.Background(), bytes.NewReader(archive.Bytes()), limits); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("logical fanout error=%v", err)
		}
	})
}

func TestInspectRejectsTarLinksTrailingStreamsAndBounds(t *testing.T) {
	malicious := rawArchive(t, tar.Header{Name: HarnessManifestName, Mode: 0600, Size: 0, Typeflag: tar.TypeSymlink, Linkname: "../../x", ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}, nil)
	if _, err := Inspect(context.Background(), bytes.NewReader(malicious), testLimits()); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("link error=%v", err)
	}
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "state.json"), stateJSON("root", "author"), 0600)
	var valid bytes.Buffer
	if _, _, err := WriteHarness(context.Background(), &valid, root, HarnessOptions{Limits: testLimits(), HarnessBuild: "v0.4.5"}); err != nil {
		t.Fatal(err)
	}
	trailing := append(append([]byte{}, valid.Bytes()...), valid.Bytes()...)
	if _, err := Inspect(context.Background(), bytes.NewReader(trailing), testLimits()); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("trailing stream error=%v", err)
	}
	limits := testLimits()
	limits.MaxStoredBytes = int64(valid.Len() - 1)
	if _, err := Inspect(context.Background(), bytes.NewReader(valid.Bytes()), limits); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("stored bound error=%v", err)
	}
	corrupt := append([]byte{}, valid.Bytes()...)
	corrupt[len(corrupt)/2] ^= 0xff
	if _, err := Inspect(context.Background(), bytes.NewReader(corrupt), testLimits()); err == nil {
		t.Fatal("corrupt archive accepted")
	}
}

func stateJSON(id, agent string) []byte {
	return []byte(`{"version":5,"id":"` + id + `","provider":"p:m","model":"p:m","agent":"` + agent + `","build":{"version":"v0.4.5"}}`)
}
func writeTestFile(t *testing.T, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}
func testLimits() Limits {
	return Limits{MaxStoredBytes: 16 << 20, MaxLogicalBytes: 32 << 20, MaxFileBytes: 8 << 20, MaxEntries: 1000, MaxPathBytes: 1024}
}
func rawArchive(t *testing.T, h tar.Header, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&h); err != nil {
		t.Fatal(err)
	}
	if len(data) > 0 {
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

var _ = strings.Contains
