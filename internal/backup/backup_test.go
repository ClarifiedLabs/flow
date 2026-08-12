package backup

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/version"
)

// TestBackupRestoreProjectRoundTrip backs up a project with tasks, an exchange
// ref, and an attachment, restores it into a fresh data dir, and verifies
// every component survives.
func TestBackupRestoreProjectRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	projectID := "p-test"

	registerProject(t, ctx, dataDir, projectID, "Test Project")

	store, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(dataDir, projectID))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	tasks := coordinator.NewTaskService(store.DB(), projectID)
	first, err := tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "First task"},
	})
	if err != nil {
		t.Fatalf("create first task: %v", err)
	}
	second, err := tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Second task"},
	})
	if err != nil {
		t.Fatalf("create second task: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close project database: %v", err)
	}

	projectDir := flowgit.ProjectDir(dataDir, projectID)
	exchangeDir := filepath.Join(projectDir, "exchange.git")
	testGit(t, "", "init", "--bare", exchangeDir)
	work := t.TempDir()
	testGit(t, work, "init")
	testGit(t, work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "seed")
	seedSHA := testGit(t, work, "rev-parse", "HEAD")
	testGit(t, work, "push", exchangeDir, "HEAD:refs/heads/main")

	attachmentDir := filepath.Join(projectDir, "attachments", first.ID)
	if err := os.MkdirAll(attachmentDir, 0o755); err != nil {
		t.Fatalf("create attachment dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(attachmentDir, "hello.txt"), []byte("hello attachment"), 0o644); err != nil {
		t.Fatalf("write attachment: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "backup")
	result, err := BackupProject(ctx, dataDir, projectID, outDir)
	if err != nil {
		t.Fatalf("backup project: %v", err)
	}
	if result.Dir != outDir {
		t.Fatalf("backup dir = %q, want %q", result.Dir, outDir)
	}
	manifest := result.Manifest
	if manifest.FlowFormat != FormatVersion || manifest.Kind != KindProject {
		t.Fatalf("manifest format/kind = %d/%q", manifest.FlowFormat, manifest.Kind)
	}
	if manifest.ProjectID != projectID || manifest.ProjectName != "Test Project" {
		t.Fatalf("manifest project = %q/%q", manifest.ProjectID, manifest.ProjectName)
	}
	if manifest.SchemaVersion != "7" {
		t.Fatalf("manifest schema_version = %q, want 7", manifest.SchemaVersion)
	}
	if manifest.FlowVersion != version.Current().String() {
		t.Fatalf("manifest flow_version = %q, want %q", manifest.FlowVersion, version.Current().String())
	}
	if manifest.CreatedAt.IsZero() {
		t.Fatal("manifest created_at is zero")
	}
	for _, artifact := range []string{"flow.db", "exchange.bundle", "attachments.tar", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(outDir, artifact)); err != nil {
			t.Fatalf("backup artifact %s: %v", artifact, err)
		}
	}

	freshDir := t.TempDir()
	restored, err := RestoreProject(ctx, outDir, freshDir, "", false)
	if err != nil {
		t.Fatalf("restore project: %v", err)
	}
	if restored.ProjectID != projectID {
		t.Fatalf("restored project id = %q, want %q", restored.ProjectID, projectID)
	}

	reopened, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(freshDir, projectID))
	if err != nil {
		t.Fatalf("reopen restored database: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	restoredTasks := coordinator.NewTaskService(reopened.DB(), projectID)
	for _, want := range []coordinator.Task{first, second} {
		got, err := restoredTasks.GetTask(ctx, want.ID)
		if err != nil {
			t.Fatalf("get restored task %s: %v", want.ID, err)
		}
		if got.Title != want.Title {
			t.Fatalf("restored task %s title = %q, want %q", want.ID, got.Title, want.Title)
		}
	}

	restoredExchange := filepath.Join(flowgit.ProjectDir(freshDir, projectID), "exchange.git")
	if got := testGit(t, "", "--git-dir", restoredExchange, "rev-parse", "refs/heads/main"); got != seedSHA {
		t.Fatalf("restored exchange main = %q, want %q", got, seedSHA)
	}
	contents, err := os.ReadFile(filepath.Join(flowgit.ProjectDir(freshDir, projectID), "attachments", first.ID, "hello.txt"))
	if err != nil {
		t.Fatalf("read restored attachment: %v", err)
	}
	if string(contents) != "hello attachment" {
		t.Fatalf("restored attachment = %q", contents)
	}
}

// TestRestoreRefusesNonEmptyProjectDirWithoutForce covers the overwrite
// refusal and the --force escape hatch, plus the matching backup-side refusal
// of a non-empty output directory.
func TestRestoreRefusesNonEmptyProjectDirWithoutForce(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	projectID := "p-refusal"

	store, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(dataDir, projectID))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	tasks := coordinator.NewTaskService(store.DB(), projectID)
	if _, err := tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Refusal task"},
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close project database: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "backup")
	if _, err := BackupProject(ctx, dataDir, projectID, outDir); err != nil {
		t.Fatalf("backup project: %v", err)
	}
	if _, err := BackupProject(ctx, dataDir, projectID, outDir); err == nil {
		t.Fatal("backup over a non-empty output directory succeeded")
	}

	freshDir := t.TempDir()
	if _, err := RestoreProject(ctx, outDir, freshDir, "", false); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	if _, err := RestoreProject(ctx, outDir, freshDir, "", false); err == nil {
		t.Fatal("restore over a non-empty project directory succeeded without force")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("restore refusal error = %q, want it to mention --force", err)
	}
	if _, err := RestoreProject(ctx, outDir, freshDir, "", true); err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	reopened, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(freshDir, projectID))
	if err != nil {
		t.Fatalf("reopen forced-restore database: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	tasks = coordinator.NewTaskService(reopened.DB(), projectID)
	list, err := tasks.ListTasks(ctx, coordinator.TaskFilter{})
	if err != nil {
		t.Fatalf("list forced-restore tasks: %v", err)
	}
	if len(list) != 1 || list[0].Title != "Refusal task" {
		t.Fatalf("forced-restore tasks = %+v", list)
	}
}

// TestRestoreRejectsUnknownSchemaVersion pins the actionable error for backups
// written by an incompatible flow build.
func TestRestoreRejectsUnknownSchemaVersion(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	projectID := "p-schema"

	store, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(dataDir, projectID))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close project database: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "backup")
	if _, err := BackupProject(ctx, dataDir, projectID, outDir); err != nil {
		t.Fatalf("backup project: %v", err)
	}
	manifestPath := filepath.Join(outDir, "manifest.json")
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	manifest.SchemaVersion = "999"
	rewritten, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, rewritten, 0o644); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}

	_, err = RestoreProject(ctx, outDir, t.TempDir(), "", false)
	if err == nil {
		t.Fatal("restore accepted an unknown schema_version")
	}
	if !strings.Contains(err.Error(), `schema_version "999"`) || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("schema refusal error = %q", err)
	}
}

// TestBackupAllRoundTrip covers the full-data-dir backup: the coordinator
// global database plus every project, with a top-level manifest.
func TestBackupAllRoundTrip(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	projectID := "p-all"

	registerProject(t, ctx, dataDir, projectID, "All Project")

	store, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(dataDir, projectID))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	tasks := coordinator.NewTaskService(store.DB(), projectID)
	created, err := tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "All task"},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close project database: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "full-backup")
	result, err := BackupAll(ctx, dataDir, outDir)
	if err != nil {
		t.Fatalf("backup all: %v", err)
	}
	if !result.GlobalDatabase {
		t.Fatal("full backup did not capture the global database")
	}
	if len(result.Projects) != 1 || result.Projects[0].Manifest.ProjectID != projectID {
		t.Fatalf("full backup projects = %+v", result.Projects)
	}
	if result.Manifest.Kind != KindFull || len(result.Manifest.Projects) != 1 {
		t.Fatalf("top-level manifest = %+v", result.Manifest)
	}
	if _, err := os.Stat(filepath.Join(outDir, "global.db")); err != nil {
		t.Fatalf("global.db artifact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "projects", projectID, "flow.db")); err != nil {
		t.Fatalf("project artifact: %v", err)
	}

	freshDir := t.TempDir()
	restored, err := RestoreAll(ctx, outDir, freshDir, false)
	if err != nil {
		t.Fatalf("restore all: %v", err)
	}
	if restored.GlobalDatabase == "" || len(restored.Projects) != 1 {
		t.Fatalf("restore all result = %+v", restored)
	}

	global, err := flowdb.OpenGlobal(ctx, filepath.Join(freshDir, "global.db"))
	if err != nil {
		t.Fatalf("reopen restored global database: %v", err)
	}
	var name string
	if err := global.DB().QueryRowContext(ctx, "SELECT name FROM projects WHERE id = ?", projectID).Scan(&name); err != nil {
		t.Fatalf("query restored global database: %v", err)
	}
	if err := global.Close(); err != nil {
		t.Fatalf("close restored global database: %v", err)
	}
	if name != "All Project" {
		t.Fatalf("restored project name = %q", name)
	}

	reopened, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(freshDir, projectID))
	if err != nil {
		t.Fatalf("reopen restored project database: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := coordinator.NewTaskService(reopened.DB(), projectID).GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("get restored task: %v", err)
	}
	if got.Title != "All task" {
		t.Fatalf("restored task title = %q", got.Title)
	}
}

// registerProject creates the coordinator global database with one project row
// so the manifest name lookup has something to find.
func registerProject(t *testing.T, ctx context.Context, dataDir, projectID, name string) {
	t.Helper()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	_, err = global.DB().ExecContext(ctx, `
INSERT INTO projects (id, name, base_branch, exchange_name, created_at, updated_at)
VALUES (?, ?, 'main', 'origin', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, projectID, name)
	if err != nil {
		t.Fatalf("insert project row: %v", err)
	}
	if err := global.Close(); err != nil {
		t.Fatalf("close global database: %v", err)
	}
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
