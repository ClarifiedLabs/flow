package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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

	runRefsGit(t, repoPath, "checkout", "-b", "issue/i-0001")
	if err := os.MkdirAll(filepath.Join(repoPath, ".flow/session"), 0o755); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	runRefsGit(t, repoPath, "mv", "app.go", ".flow/session/app.go")
	runRefsGit(t, repoPath, "commit", "-m", "move app into session")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "issue/i-0001:issue/i-0001")

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

	runRefsGit(t, repoPath, "checkout", "-b", "issue/i-0001")
	writeRefsFile(t, repoPath, "notes.txt", "alpha\n++ new comment\nkeep\n")
	runRefsGit(t, repoPath, "add", "notes.txt")
	runRefsGit(t, repoPath, "commit", "-m", "update notes")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "issue/i-0001:issue/i-0001")

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

	runRefsGit(t, repoPath, "checkout", "-b", "issue/i-0001")
	writeRefsFile(t, repoPath, "trailing.txt", "value   \n")
	runRefsGit(t, repoPath, "add", "trailing.txt")
	runRefsGit(t, repoPath, "commit", "-m", "add trailing spaces")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "issue/i-0001:issue/i-0001")

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

	runRefsGit(t, repoPath, "checkout", "-b", "issue/i-0001")
	writeRefsFile(t, repoPath, relativePath, "new\n")
	runRefsGit(t, repoPath, "add", ".")
	runRefsGit(t, repoPath, "commit", "-m", "update quoted path")
	head := refsGitOutput(t, repoPath, "rev-parse", "HEAD")
	runRefsGit(t, repoPath, "push", exchangePath, "issue/i-0001:issue/i-0001")

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
