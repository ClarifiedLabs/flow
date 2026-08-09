package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func newProjectService(t *testing.T) *ProjectService {
	t.Helper()

	store, err := flowdb.OpenGlobal(context.Background(), filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return NewProjectService(store.DB())
}

func TestProjectIDFromNameNormalizesHumanName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Flow App":        "p-flow-app",
		"  API___Server ": "p-api-server",
		"Release 2026":    "p-release-2026",
		"CAFÉ":            "p-caf",
	}
	for name, want := range tests {
		got, err := ProjectIDFromName(name)
		if err != nil {
			t.Fatalf("ProjectIDFromName(%q): %v", name, err)
		}
		if got != want {
			t.Errorf("ProjectIDFromName(%q) = %q, want %q", name, got, want)
		}
	}

	for _, name := range []string{"", "---", strings.Repeat("a", maxProjectKeyLength+1)} {
		if _, err := ProjectIDFromName(name); err == nil {
			t.Errorf("ProjectIDFromName(%q) succeeded, want error", name)
		}
	}
}

func TestProjectIDFromTaskID(t *testing.T) {
	t.Parallel()
	for taskID, wantProject := range map[string]string{
		"t-flow-app-0001":      "p-flow-app",
		"t-release-2026-10423": "p-release-2026",
	} {
		got, ok := ProjectIDFromTaskID(taskID)
		if !ok || got != wantProject {
			t.Errorf("ProjectIDFromTaskID(%q) = (%q, %t), want (%q, true)", taskID, got, ok, wantProject)
		}
	}
	for _, taskID := range []string{"i-0001", "t-flow-001", "t-Flow-0001", "t-flow-nope"} {
		if projectID, ok := ProjectIDFromTaskID(taskID); ok {
			t.Errorf("ProjectIDFromTaskID(%q) = (%q, true), want invalid", taskID, projectID)
		}
	}
}

func TestProjectServiceInsertAndLookups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := newProjectService(t)

	inserted, err := service.Insert(ctx, Project{
		ID:           "p-demo",
		Name:         "demo",
		RepoPath:     "/tmp/demo",
		BaseBranch:   "main",
		ExchangeName: "flow",
		ExchangePath: "/tmp/exchange.git",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if inserted.Name != "demo" {
		t.Fatalf("inserted name = %q, want demo", inserted.Name)
	}
	if inserted.CreatedAt.IsZero() || inserted.UpdatedAt.IsZero() {
		t.Fatal("inserted project should carry timestamps")
	}

	got, err := service.Get(ctx, "p-demo")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.ExchangePath != "/tmp/exchange.git" || got.BaseBranch != "main" {
		t.Fatalf("get project = %+v", got)
	}

	byName, err := service.GetByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get project by name: %v", err)
	}
	if byName.ID != "p-demo" {
		t.Fatalf("get by name id = %q, want p-demo", byName.ID)
	}

	byRepo, err := service.GetByRepoPath(ctx, "/tmp/demo")
	if err != nil {
		t.Fatalf("get project by repo path: %v", err)
	}
	if byRepo.ID != "p-demo" {
		t.Fatalf("get by repo path id = %q, want p-demo", byRepo.ID)
	}

	byExchange, err := service.GetByExchangePath(ctx, "/tmp/exchange.git")
	if err != nil {
		t.Fatalf("get project by exchange path: %v", err)
	}
	if byExchange.ID != "p-demo" {
		t.Fatalf("get by exchange path id = %q, want p-demo", byExchange.ID)
	}

	if _, err := service.Get(ctx, "p-missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("get missing project err = %v, want ErrProjectNotFound", err)
	}
	if _, err := service.GetByRepoPath(ctx, "/tmp/nope"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("get missing repo path err = %v, want ErrProjectNotFound", err)
	}
}

func TestProjectServiceInsertRejectsDuplicateName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := newProjectService(t)

	first, err := service.Insert(ctx, Project{
		ID: "p-demo", Name: "demo", RepoPath: "/tmp/demo-a",
		BaseBranch: "main", ExchangeName: "flow",
	})
	if err != nil {
		t.Fatalf("insert first project: %v", err)
	}
	if first.Name != "demo" {
		t.Fatalf("first name = %q, want demo", first.Name)
	}

	_, err = service.Insert(ctx, Project{
		ID: "p-demo", Name: "demo", RepoPath: "/tmp/demo-b",
		BaseBranch: "main", ExchangeName: "flow",
	})
	if !errors.Is(err, ErrProjectNameExists) {
		t.Fatalf("duplicate name err = %v, want ErrProjectNameExists", err)
	}
}

func TestProjectServiceInsertRejectsDuplicateRepoPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := newProjectService(t)

	if _, err := service.Insert(ctx, Project{
		ID: "p-demo", Name: "demo", RepoPath: "/tmp/demo",
		BaseBranch: "main", ExchangeName: "flow",
	}); err != nil {
		t.Fatalf("insert first project: %v", err)
	}

	if _, err := service.Insert(ctx, Project{
		ID: "p-other", Name: "other", RepoPath: "/tmp/demo",
		BaseBranch: "main", ExchangeName: "flow",
	}); !errors.Is(err, ErrProjectRepoPathExists) {
		t.Fatalf("duplicate repo path err = %v, want ErrProjectRepoPathExists", err)
	}
}

func TestProjectServiceListOrdersByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := newProjectService(t)

	for _, p := range []Project{
		{ID: "p-zeta", Name: "zeta", RepoPath: "/tmp/z", BaseBranch: "main", ExchangeName: "flow"},
		{ID: "p-alpha", Name: "alpha", RepoPath: "/tmp/a", BaseBranch: "main", ExchangeName: "flow"},
	} {
		if _, err := service.Insert(ctx, p); err != nil {
			t.Fatalf("insert project %s: %v", p.Name, err)
		}
	}

	projects, err := service.List(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("len(projects) = %d, want 2", len(projects))
	}
	if projects[0].Name != "alpha" || projects[1].Name != "zeta" {
		t.Fatalf("projects = [%s %s], want [alpha zeta]", projects[0].Name, projects[1].Name)
	}
}
