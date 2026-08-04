package historyarchive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestWorkspaceArchiveReconstructsStagedUnstagedBinaryAndUntracked(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, git, source, "init", "--initial-branch=main")
	gitTest(t, git, source, "config", "user.name", "History Test")
	gitTest(t, git, source, "config", "user.email", "history@example.invalid")
	writeTestFile(t, filepath.Join(source, "tracked.bin"), []byte{0, 1, 2, 3, 'a'}, 0600)
	writeTestFile(t, filepath.Join(source, "script.sh"), []byte("#!/bin/sh\necho base\n"), 0700)
	gitTest(t, git, source, "add", "--", "tracked.bin", "script.sh")
	gitTest(t, git, source, "commit", "-m", "base")
	writeTestFile(t, filepath.Join(source, "tracked.bin"), []byte{0, 1, 9, 3, 's'}, 0600)
	writeTestFile(t, filepath.Join(source, "script.sh"), []byte("#!/bin/sh\necho staged\n"), 0700)
	gitTest(t, git, source, "add", "--", "tracked.bin", "script.sh")
	writeTestFile(t, filepath.Join(source, "tracked.bin"), []byte{0, 7, 9, 3, 'w'}, 0600)
	writeTestFile(t, filepath.Join(source, "new", "payload.bin"), []byte{0xff, 0, 0xfe, 'u'}, 0700)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("payload.bin", filepath.Join(source, "new", "latest")); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(source, ".git", "must-not-archive"), []byte("private git metadata"), 0600)
	options := WorkspaceOptions{Limits: testLimits(), GitPath: git}
	var first, second bytes.Buffer
	artifact, manifest, err := WriteWorkspace(context.Background(), &first, source, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteWorkspace(context.Background(), &second, source, options); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("workspace archive is not deterministic")
	}
	if artifact.EntryCount != len(manifest.Untracked)+2 || len(manifest.StagedPaths) != 2 || len(manifest.UnstagedPaths) != 1 {
		t.Fatalf("artifact=%+v manifest=%+v", artifact, manifest)
	}
	for _, f := range manifest.Untracked {
		if strings.Contains(strings.ToLower(f.Path), ".git") {
			t.Fatalf("archived .git path %q", f.Path)
		}
	}
	inspection, err := Inspect(context.Background(), bytes.NewReader(first.Bytes()), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Workspace == nil || inspection.Workspace.InventoryDigest != manifest.InventoryDigest {
		t.Fatalf("inspection=%+v", inspection)
	}
	restore := filepath.Join(t.TempDir(), "restore")
	gitTest(t, git, "", "clone", "--no-local", source, restore)
	gitTest(t, git, restore, "checkout", "--detach", manifest.HeadCommit)
	if _, err := RestoreWorkspace(context.Background(), bytes.NewReader(first.Bytes()), restore, testLimits(), git); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tracked.bin", "script.sh", "new/payload.bin"} {
		want, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(restore, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("restored %s differs", name)
		}
	}
	wantStaged := gitOutput(t, git, source, "diff", "--cached", "--binary", "--full-index", "HEAD", "--")
	gotStaged := gitOutput(t, git, restore, "diff", "--cached", "--binary", "--full-index", "HEAD", "--")
	if !bytes.Equal(gotStaged, wantStaged) {
		t.Fatalf("staged patch differs\nwant %q\ngot  %q", wantStaged, gotStaged)
	}
	wantUnstaged := gitOutput(t, git, source, "diff", "--binary", "--full-index", "--")
	gotUnstaged := gitOutput(t, git, restore, "diff", "--binary", "--full-index", "--")
	if !bytes.Equal(gotUnstaged, wantUnstaged) {
		t.Fatal("unstaged patch differs")
	}

	failedRestore := filepath.Join(t.TempDir(), "failed-restore")
	gitTest(t, git, "", "clone", "--no-local", source, failedRestore)
	gitTest(t, git, failedRestore, "checkout", "--detach", manifest.HeadCommit)
	failingGit := filepath.Join(t.TempDir(), "git-fail-final-inventory")
	script := "#!/bin/sh\ncase \"$*\" in *\"ls-files --stage -z\"*) exit 17;; esac\nexec " + strconv.Quote(git) + " \"$@\"\n"
	if err := os.WriteFile(failingGit, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreWorkspace(context.Background(), bytes.NewReader(first.Bytes()), failedRestore, testLimits(), failingGit); err == nil {
		t.Fatal("restore unexpectedly succeeded after final inventory failure")
	}
	if status := gitOutput(t, git, failedRestore, "status", "--porcelain=v1", "--untracked-files=all"); len(status) != 0 {
		t.Fatalf("failed restore left workspace mutations: %q", status)
	}
	if _, err := os.Lstat(filepath.Join(failedRestore, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore left untracked directory: %v", err)
	}
}

func TestWorkspaceArchiveRejectsSensitiveUntrackedPayload(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, git, repo, "init")
	gitTest(t, git, repo, "config", "user.name", "T")
	gitTest(t, git, repo, "config", "user.email", "t@example.invalid")
	writeTestFile(t, filepath.Join(repo, "base"), []byte("base"), 0600)
	gitTest(t, git, repo, "add", "base")
	gitTest(t, git, repo, "commit", "-m", "base")
	writeTestFile(t, filepath.Join(repo, "secret"), []byte("prefix TOKEN-123 suffix"), 0600)
	_, _, err = WriteWorkspace(context.Background(), io.Discard, repo, WorkspaceOptions{Limits: testLimits(), GitPath: git, SensitiveValues: [][]byte{[]byte("TOKEN-123")}})
	if !errors.Is(err, ErrSensitiveContent) {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkspaceRestorePreflightsCombinedPatchesBeforeMutation(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, git, source, "init", "--initial-branch=main")
	gitTest(t, git, source, "config", "user.name", "History Test")
	gitTest(t, git, source, "config", "user.email", "history@example.invalid")
	writeTestFile(t, filepath.Join(source, "tracked.txt"), []byte("base\n"), 0600)
	gitTest(t, git, source, "add", "--", "tracked.txt")
	gitTest(t, git, source, "commit", "-m", "base")
	writeTestFile(t, filepath.Join(source, "tracked.txt"), []byte("staged\n"), 0600)
	gitTest(t, git, source, "add", "--", "tracked.txt")
	writeTestFile(t, filepath.Join(source, "tracked.txt"), []byte("unstaged\n"), 0600)

	var captured bytes.Buffer
	_, manifest, err := WriteWorkspace(context.Background(), &captured, source, WorkspaceOptions{Limits: testLimits(), GitPath: git})
	if err != nil {
		t.Fatal(err)
	}
	staged := gitOutput(t, git, source, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	invalidUnstaged := []byte("not a valid Git patch\n")
	manifest.Unstaged = patchFor(invalidUnstaged)
	encoded, err := canonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	blobs := make(map[string]blobSource)
	addBytesBlob(blobs, manifest.Staged.Blob, staged)
	addBytesBlob(blobs, manifest.Unstaged.Blob, invalidUnstaged)
	var malformed bytes.Buffer
	if _, err := writeArchive(context.Background(), &malformed, testLimits(), WorkspaceManifestName, encoded, blobs, 2, int64(len(encoded)+len(staged)+len(invalidUnstaged))); err != nil {
		t.Fatal(err)
	}

	restore := filepath.Join(t.TempDir(), "restore")
	gitTest(t, git, "", "clone", "--no-local", source, restore)
	gitTest(t, git, restore, "checkout", "--detach", manifest.HeadCommit)
	marker := filepath.Join(t.TempDir(), "destination-mutated")
	wrapper := filepath.Join(t.TempDir(), "git-wrapper.sh")
	writeTestFile(t, wrapper, []byte("#!/bin/sh\ncached=\ncheck=\nfor arg do\n  [ \"$arg\" = \"--cached\" ] && cached=1\n  [ \"$arg\" = \"--check\" ] && check=1\ndone\nif [ -n \"$cached\" ] && [ -z \"$check\" ] && [ -z \"${GIT_INDEX_FILE:-}\" ]; then\n  : > "+strconv.Quote(marker)+"\nfi\nexec "+strconv.Quote(git)+" \"$@\"\n"), 0700)

	if _, err := RestoreWorkspace(context.Background(), bytes.NewReader(malformed.Bytes()), restore, testLimits(), wrapper); err == nil {
		t.Fatal("restore unexpectedly accepted an invalid unstaged patch")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination mutation marker error = %v, want not exist", err)
	}
	if status := gitOutput(t, git, restore, "status", "--porcelain=v1", "-z", "--untracked-files=all"); len(status) != 0 {
		t.Fatalf("destination changed during failed preflight: %q", status)
	}
}

func TestWorkspaceArchiveRejectsSensitiveModifiedTrackedBinaryVersions(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	for _, test := range []struct {
		name     string
		populate func(*testing.T, string)
	}{
		{
			name: "staged index version",
			populate: func(t *testing.T, repo string) {
				writeTestFile(t, filepath.Join(repo, "tracked.bin"), append([]byte{0, 1}, []byte("BINARY-CREDENTIAL")...), 0600)
				gitTest(t, git, repo, "add", "--", "tracked.bin")
				writeTestFile(t, filepath.Join(repo, "tracked.bin"), []byte{0, 2, 3, 4}, 0600)
			},
		},
		{
			name: "unstaged worktree version",
			populate: func(t *testing.T, repo string) {
				writeTestFile(t, filepath.Join(repo, "tracked.bin"), []byte{0, 2, 3, 4}, 0600)
				gitTest(t, git, repo, "add", "--", "tracked.bin")
				writeTestFile(t, filepath.Join(repo, "tracked.bin"), append([]byte{0, 1}, []byte("BINARY-CREDENTIAL")...), 0600)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			gitTest(t, git, repo, "init", "--initial-branch=main")
			gitTest(t, git, repo, "config", "user.name", "History Test")
			gitTest(t, git, repo, "config", "user.email", "history@example.invalid")
			writeTestFile(t, filepath.Join(repo, "tracked.bin"), []byte{0, 1, 2, 3}, 0600)
			gitTest(t, git, repo, "add", "--", "tracked.bin")
			gitTest(t, git, repo, "commit", "-m", "base")
			test.populate(t, repo)

			var archive bytes.Buffer
			_, _, err := WriteWorkspace(context.Background(), &archive, repo, WorkspaceOptions{
				Limits: testLimits(), GitPath: git, SensitiveValues: [][]byte{[]byte("BINARY-CREDENTIAL")},
			})
			if !errors.Is(err, ErrSensitiveContent) {
				t.Fatalf("error = %v, want sensitive content", err)
			}
			if archive.Len() != 0 {
				t.Fatalf("wrote %d archive bytes before rejecting binary credential", archive.Len())
			}
		})
	}
}

func TestWorkspaceArchiveRejectsSensitiveUntrackedMetadata(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	tests := []struct {
		name      string
		sensitive string
		populate  func(*testing.T, string)
	}{
		{
			name:      "filename",
			sensitive: "WORKSPACE-CREDENTIAL",
			populate: func(t *testing.T, repo string) {
				writeTestFile(t, filepath.Join(repo, "nested", "prefix-WORKSPACE-CREDENTIAL.txt"), nil, 0600)
			},
		},
		{
			name:      "symlink target",
			sensitive: "targets/WORKSPACE-CREDENTIAL",
			populate: func(t *testing.T, repo string) {
				if err := os.Symlink("targets/WORKSPACE-CREDENTIAL", filepath.Join(repo, "latest")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			gitTest(t, git, repo, "init")
			gitTest(t, git, repo, "config", "user.name", "T")
			gitTest(t, git, repo, "config", "user.email", "t@example.invalid")
			writeTestFile(t, filepath.Join(repo, "base"), []byte("base"), 0600)
			gitTest(t, git, repo, "add", "base")
			gitTest(t, git, repo, "commit", "-m", "base")
			test.populate(t, repo)

			var archive bytes.Buffer
			_, _, err := WriteWorkspace(context.Background(), &archive, repo, WorkspaceOptions{
				Limits:          testLimits(),
				GitPath:         git,
				SensitiveValues: [][]byte{[]byte(test.sensitive)},
			})
			if !errors.Is(err, ErrSensitiveContent) {
				t.Fatalf("error=%v", err)
			}
			if archive.Len() != 0 {
				t.Fatalf("wrote %d archive bytes before rejecting sensitive metadata", archive.Len())
			}
		})
	}
}

func gitTest(t *testing.T, git, repo string, args ...string) {
	t.Helper()
	cmdArgs := args
	if repo != "" {
		cmdArgs = append([]string{"-C", repo}, args...)
	}
	cmd := exec.Command(git, cmdArgs...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
func gitOutput(t *testing.T, git, repo string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(git, append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}
