package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChangedFileDiffExcludesChangesMadeOnlyOnAdvancedBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")

	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, "shared.txt", "initial\n")
	runRefsGit(t, repoPath, "add", "shared.txt")
	runRefsGit(t, repoPath, "commit", "-m", "initial")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, "main:main")

	runRefsGit(t, repoPath, "checkout", "-b", "task/t-test-0001")
	writeRefsFile(t, repoPath, "task.txt", "task change\n")
	runRefsGit(t, repoPath, "add", "task.txt")
	runRefsGit(t, repoPath, "commit", "-m", "task change")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "task/t-test-0001:task/t-test-0001")

	runRefsGit(t, repoPath, "checkout", "main")
	writeRefsFile(t, repoPath, "shared.txt", "updated only on main\n")
	writeRefsFile(t, repoPath, "main-only.txt", "main change\n")
	runRefsGit(t, repoPath, "add", "shared.txt", "main-only.txt")
	runRefsGit(t, repoPath, "commit", "-m", "advance main")
	runRefsGit(t, repoPath, "push", exchangePath, "main:main")

	stats, err := ChangedFileStats(ctx, exchangePath, "refs/heads/main", head)
	if err != nil {
		t.Fatalf("changed file stats: %v", err)
	}
	if stats.Additions != 1 || stats.Deletions != 0 || len(stats.Files) != 1 {
		t.Fatalf("stats = %+v, want only the task-side addition", stats)
	}
	if stats.Files[0].Path != "task.txt" {
		t.Fatalf("files = %+v, want task.txt only", stats.Files)
	}

	diff, err := ChangedFileDiff(ctx, exchangePath, "refs/heads/main", head)
	if err != nil {
		t.Fatalf("changed file diff: %v", err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "task.txt" || len(diff.Files[0].Hunks) != 1 {
		t.Fatalf("diff files = %+v, want only the task.txt hunk", diff.Files)
	}
	lines := diff.Files[0].Hunks[0].Lines
	if len(lines) != 1 || lines[0].Kind != "add" || lines[0].Text != "task change" {
		t.Fatalf("diff lines = %+v, want the task-side addition", lines)
	}
}

func TestChangedFileStatsExcludesRestoredPathsButKeepsSourceDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")

	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, "app.go", "package app\n")
	runRefsGit(t, repoPath, "add", "app.go")
	runRefsGit(t, repoPath, "commit", "-m", "initial")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, "main:main")

	runRefsGit(t, repoPath, "checkout", "-b", "task/t-test-0001")
	if err := os.MkdirAll(filepath.Join(repoPath, ".flow/session"), 0o755); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	runRefsGit(t, repoPath, "mv", "app.go", ".flow/session/app.go")
	runRefsGit(t, repoPath, "commit", "-m", "move app into session")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "task/t-test-0001:task/t-test-0001")

	stats, err := ChangedFileStats(ctx, exchangePath, "refs/heads/main", head)
	if err != nil {
		t.Fatalf("changed file stats: %v", err)
	}
	if stats.Additions != 0 || stats.Deletions != 1 || len(stats.Files) != 1 {
		t.Fatalf("stats = %+v, want one mergeable app.go deletion", stats)
	}
	if stats.Files[0].Path != "app.go" || stats.Files[0].Deletions != 1 {
		t.Fatalf("files = %+v, want app.go deletion", stats.Files)
	}

	diff, err := ChangedFileDiff(ctx, exchangePath, "refs/heads/main", head)
	if err != nil {
		t.Fatalf("changed file diff: %v", err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "app.go" || len(diff.Files[0].Hunks) != 1 {
		t.Fatalf("diff files = %+v, want one app.go hunk", diff.Files)
	}
	if len(diff.Files[0].Hunks[0].Lines) != 1 || diff.Files[0].Hunks[0].Lines[0].Kind != "delete" || diff.Files[0].Hunks[0].Lines[0].Text != "package app" {
		t.Fatalf("diff hunk = %+v, want deleted app.go line", diff.Files[0].Hunks[0])
	}
}

func TestChangedFileDiffParsesHeaderLikeContentLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")

	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, "notes.txt", "alpha\n-- old comment\nkeep\n")
	runRefsGit(t, repoPath, "add", "notes.txt")
	runRefsGit(t, repoPath, "commit", "-m", "initial")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, "main:main")

	runRefsGit(t, repoPath, "checkout", "-b", "task/t-test-0001")
	writeRefsFile(t, repoPath, "notes.txt", "alpha\n++ new comment\nkeep\n")
	runRefsGit(t, repoPath, "add", "notes.txt")
	runRefsGit(t, repoPath, "commit", "-m", "update notes")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "task/t-test-0001:task/t-test-0001")

	diff, err := ChangedFileDiff(ctx, exchangePath, "refs/heads/main", head)
	if err != nil {
		t.Fatalf("changed file diff: %v", err)
	}
	if len(diff.Files) != 1 || len(diff.Files[0].Hunks) != 1 {
		t.Fatalf("diff = %+v, want one file hunk", diff)
	}
	var sawDelete bool
	var sawAdd bool
	for _, line := range diff.Files[0].Hunks[0].Lines {
		if line.Kind == "delete" && line.Text == "-- old comment" {
			sawDelete = true
		}
		if line.Kind == "add" && line.Text == "++ new comment" {
			sawAdd = true
		}
	}
	if !sawDelete || !sawAdd {
		t.Fatalf("hunk lines = %+v, want header-like delete and add content", diff.Files[0].Hunks[0].Lines)
	}
}

func TestChangedFileDiffPreservesTrailingSpaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")

	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, "README.md", "initial\n")
	runRefsGit(t, repoPath, "add", "README.md")
	runRefsGit(t, repoPath, "commit", "-m", "initial")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, "main:main")

	runRefsGit(t, repoPath, "checkout", "-b", "task/t-test-0001")
	writeRefsFile(t, repoPath, "trailing.txt", "value   \n")
	runRefsGit(t, repoPath, "add", "trailing.txt")
	runRefsGit(t, repoPath, "commit", "-m", "add trailing spaces")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "task/t-test-0001:task/t-test-0001")

	diff, err := ChangedFileDiff(ctx, exchangePath, "refs/heads/main", head)
	if err != nil {
		t.Fatalf("changed file diff: %v", err)
	}
	if len(diff.Files) != 1 || len(diff.Files[0].Hunks) != 1 || len(diff.Files[0].Hunks[0].Lines) != 1 {
		t.Fatalf("diff = %+v, want one added line", diff)
	}
	if diff.Files[0].Hunks[0].Lines[0].Text != "value   " {
		t.Fatalf("line text = %q, want trailing spaces preserved", diff.Files[0].Hunks[0].Lines[0].Text)
	}
}

func TestChangedFileDiffMatchesQuotedPatchPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	relativePath := "dir/tab\tname.txt"

	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, relativePath, "old\n")
	runRefsGit(t, repoPath, "add", ".")
	runRefsGit(t, repoPath, "commit", "-m", "initial")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, "main:main")

	runRefsGit(t, repoPath, "checkout", "-b", "task/t-test-0001")
	writeRefsFile(t, repoPath, relativePath, "new\n")
	runRefsGit(t, repoPath, "add", ".")
	runRefsGit(t, repoPath, "commit", "-m", "update quoted path")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "task/t-test-0001:task/t-test-0001")

	diff, err := ChangedFileDiff(ctx, exchangePath, "refs/heads/main", head)
	if err != nil {
		t.Fatalf("changed file diff: %v", err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != relativePath || len(diff.Files[0].Hunks) != 1 {
		t.Fatalf("diff files = %+v, want quoted path hunks attached", diff.Files)
	}
	var sawNew bool
	for _, line := range diff.Files[0].Hunks[0].Lines {
		if line.Kind == "add" && line.Text == "new" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatalf("hunk lines = %+v, want added new line", diff.Files[0].Hunks[0].Lines)
	}
}

func TestImmutableEvidenceGitHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")

	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, "README.md", "base\n")
	runRefsGit(t, repoPath, "add", "README.md")
	runRefsGit(t, repoPath, "commit", "-m", "base")
	baseSHA := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	baseBlobSHA := refsGitOutput(t, repoPath, "rev-parse", "HEAD:README.md")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, "main:main")

	runRefsGit(t, repoPath, "checkout", "-b", "task/t-evidence-0001")
	writeRefsFile(t, repoPath, "feature.txt", "source evidence\n")
	runRefsGit(t, repoPath, "add", "feature.txt")
	runRefsGit(t, repoPath, "commit", "-m", "source change")
	headSHA := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "task/t-evidence-0001:task/t-evidence-0001")

	exists, err := CommitExists(ctx, exchangePath, headSHA)
	if err != nil || !exists {
		t.Fatalf("source commit exists = %t, err = %v", exists, err)
	}
	exists, err = CommitExists(ctx, exchangePath, strings.Repeat("f", 40))
	if err != nil || exists {
		t.Fatalf("missing commit exists = %t, err = %v", exists, err)
	}
	exists, err = CommitExists(ctx, exchangePath, baseBlobSHA)
	if err != nil || exists {
		t.Fatalf("blob exists as commit = %t, err = %v", exists, err)
	}
	mergeBase, err := MergeBase(ctx, exchangePath, "refs/heads/main", headSHA)
	if err != nil {
		t.Fatalf("merge base: %v", err)
	}
	if mergeBase != baseSHA {
		t.Fatalf("merge base = %q, want %q", mergeBase, baseSHA)
	}
	digest, err := CanonicalDiffDigest(ctx, exchangePath, mergeBase, headSHA)
	if err != nil {
		t.Fatalf("canonical diff digest: %v", err)
	}
	replayedDigest, err := CanonicalDiffDigest(ctx, exchangePath, mergeBase, headSHA)
	if err != nil || replayedDigest != digest || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("replayed digest = %q, err = %v, want %q", replayedDigest, err, digest)
	}
	if err := os.MkdirAll(filepath.Join(repoPath, ".flow", "session"), 0o755); err != nil {
		t.Fatalf("mkdir session evidence: %v", err)
	}
	writeRefsFile(t, repoPath, ".flow/session/transcript", "mutable session data\n")
	runRefsGit(t, repoPath, "add", ".flow/session/transcript")
	runRefsGit(t, repoPath, "commit", "-m", "session-only data")
	sessionHeadSHA := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "task/t-evidence-0001:task/t-evidence-0001")
	sessionDigest, err := CanonicalDiffDigest(ctx, exchangePath, mergeBase, sessionHeadSHA)
	if err != nil || sessionDigest != digest {
		t.Fatalf("session-only digest = %q, err = %v, want %q", sessionDigest, err, digest)
	}

	ref := "refs/flow/promotions/pr-test/source"
	created, err := CreateOrVerifyRefOwned(ctx, exchangePath, ref, headSHA)
	if err != nil || !created {
		t.Fatalf("create immutable ref = created %v, err %v; want true, nil", created, err)
	}
	created, err = CreateOrVerifyRefOwned(ctx, exchangePath, ref, headSHA)
	if err != nil || created {
		t.Fatalf("verify immutable ref replay = created %v, err %v; want false, nil", created, err)
	}
	if err := CreateOrVerifyRef(ctx, exchangePath, ref, baseSHA); err == nil || !strings.Contains(err.Error(), "already points to") {
		t.Fatalf("repoint immutable ref error = %v, want mismatch", err)
	}

	symbolicRef := "refs/flow/promotions/pr-test/symbolic-source"
	runRefsGit(t, "", "--git-dir", exchangePath, "symbolic-ref", symbolicRef, "refs/heads/main")
	if err := CreateOrVerifyRef(ctx, exchangePath, symbolicRef, baseSHA); err == nil {
		t.Fatal("symbolic immutable ref unexpectedly verified")
	}
}

func TestDeleteRefIfMatchesUsesExpectedTipCAS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, "file.txt", "first\n")
	runRefsGit(t, repoPath, "add", "file.txt")
	runRefsGit(t, repoPath, "commit", "-m", "first")
	first := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	writeRefsFile(t, repoPath, "file.txt", "second\n")
	runRefsGit(t, repoPath, "commit", "-am", "second")
	second := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, first+":refs/heads/cleanup")
	runRefsGit(t, repoPath, "push", exchangePath, second+":refs/heads/other")

	if err := DeleteRefIfMatches(ctx, exchangePath, "refs/heads/cleanup", second); err == nil {
		t.Fatal("delete with unexpected tip succeeded")
	}
	if tip, exists, err := BranchTip(ctx, exchangePath, "cleanup"); err != nil || !exists || tip != first {
		t.Fatalf("ref after rejected delete = %q exists=%v err=%v, want %q", tip, exists, err, first)
	}
	if err := DeleteRefIfMatches(ctx, exchangePath, "refs/heads/cleanup", first); err != nil {
		t.Fatalf("delete expected ref: %v", err)
	}
	if tip, exists, err := BranchTip(ctx, exchangePath, "cleanup"); err != nil || exists {
		t.Fatalf("ref after expected delete = %q exists=%v err=%v", tip, exists, err)
	}
}

func TestWithLockedRefsFencesConcurrentUpdatesAndRejectsStaleTips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")

	runRefsGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runRefsGit(t, repoPath, "config", "user.name", "Flow Test")
	runRefsGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeRefsFile(t, repoPath, "file.txt", "first\n")
	runRefsGit(t, repoPath, "add", "file.txt")
	runRefsGit(t, repoPath, "commit", "-m", "first")
	first := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, "", "init", "--bare", exchangePath)
	runRefsGit(t, repoPath, "push", exchangePath, "HEAD:refs/heads/main")

	writeRefsFile(t, repoPath, "file.txt", "second\n")
	runRefsGit(t, repoPath, "commit", "-am", "second")
	second := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "HEAD:refs/heads/next")

	var updateErr error
	if err := WithLockedRefs(ctx, exchangePath, map[string]string{"refs/heads/main": first}, func(context.Context) error {
		cmd := exec.Command("git", "--git-dir", exchangePath, "update-ref", "refs/heads/main", second, first)
		output, err := cmd.CombinedOutput()
		if err == nil {
			return fmt.Errorf("competing ref update unexpectedly succeeded")
		}
		updateErr = fmt.Errorf("%w: %s", err, output)
		return nil
	}); err != nil {
		t.Fatalf("lock expected ref: %v", err)
	}
	if updateErr == nil || !strings.Contains(updateErr.Error(), "cannot lock ref") {
		t.Fatalf("competing update err = %v, want lock rejection", updateErr)
	}
	if got := refsGitOutput(t, "", "--git-dir", exchangePath, "rev-parse", "refs/heads/main"); got != first {
		t.Fatalf("main after locked update = %q, want %q", got, first)
	}

	called := false
	if err := WithLockedRefs(ctx, exchangePath, map[string]string{"refs/heads/main": second}, func(context.Context) error {
		called = true
		return nil
	}); err == nil {
		t.Fatal("stale expected ref unexpectedly locked")
	}
	if called {
		t.Fatal("stale expected ref invoked callback")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	lockedAfterTimeout := false
	timeoutErr := WithLockedRefs(timeoutCtx, exchangePath, map[string]string{"refs/heads/main": first}, func(lockedCtx context.Context) error {
		<-lockedCtx.Done()
		cmd := exec.Command("git", "--git-dir", exchangePath, "update-ref", "refs/heads/main", second, first)
		if output, updateErr := cmd.CombinedOutput(); updateErr != nil && strings.Contains(string(output), "cannot lock ref") {
			lockedAfterTimeout = true
		}
		return lockedCtx.Err()
	})
	if timeoutErr == nil {
		t.Fatal("timed out ref-lock callback returned nil")
	}
	if !lockedAfterTimeout {
		t.Fatal("ref lock was released before the timed-out callback returned")
	}
}

func runRefsGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func refsGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}

	return string(bytesTrimSpace(output))
}

func writeRefsFile(t *testing.T, repoPath string, relativePath string, contents string) {
	t.Helper()
	path := filepath.Join(repoPath, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relativePath, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", relativePath, err)
	}
}

func bytesTrimSpace(value []byte) []byte {
	start := 0
	for start < len(value) && (value[start] == '\n' || value[start] == '\r' || value[start] == '\t' || value[start] == ' ') {
		start++
	}
	end := len(value)
	for end > start && (value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t' || value[end-1] == ' ') {
		end--
	}

	return value[start:end]
}
