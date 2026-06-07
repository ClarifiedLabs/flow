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

func TestNewProjectIDUsesPrefix(t *testing.T) {
	id, err := NewProjectID()
	if err != nil {
		t.Fatalf("new project id: %v", err)
	}
	if !strings.HasPrefix(id, "p-") || len(id) <= len("p-") {
		t.Fatalf("project id = %q, want p-<random>", id)
	}

	other, err := NewProjectID()
	if err != nil {
		t.Fatalf("new project id: %v", err)
	}
	if id == other {
		t.Fatalf("project ids should be random, got %q twice", id)
	}
}

func TestProjectServiceInsertAndLookups(t *testing.T) {
	ctx := context.Background()
	service := newProjectService(t)

	inserted, err := service.Insert(ctx, Project{
		ID:           "p-1111",
		Name:         "demo",
		RepoPath:     "/tmp/demo",
		BaseBranch:   "main",
		ExchangeName: "flow",
		ExchangeURL:  "file:///tmp/exchange.git",
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

	got, err := service.Get(ctx, "p-1111")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.ExchangeURL != "file:///tmp/exchange.git" || got.BaseBranch != "main" {
		t.Fatalf("get project = %+v", got)
	}

	byName, err := service.GetByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get project by name: %v", err)
	}
	if byName.ID != "p-1111" {
		t.Fatalf("get by name id = %q, want p-1111", byName.ID)
	}

	byRepo, err := service.GetByRepoPath(ctx, "/tmp/demo")
	if err != nil {
		t.Fatalf("get project by repo path: %v", err)
	}
	if byRepo.ID != "p-1111" {
		t.Fatalf("get by repo path id = %q, want p-1111", byRepo.ID)
	}

	byExchange, err := service.GetByExchangePath(ctx, "/tmp/exchange.git")
	if err != nil {
		t.Fatalf("get project by exchange path: %v", err)
	}
	if byExchange.ID != "p-1111" {
		t.Fatalf("get by exchange path id = %q, want p-1111", byExchange.ID)
	}

	if _, err := service.Get(ctx, "p-missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("get missing project err = %v, want ErrProjectNotFound", err)
	}
	if _, err := service.GetByRepoPath(ctx, "/tmp/nope"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("get missing repo path err = %v, want ErrProjectNotFound", err)
	}
}

func TestProjectServiceInsertDedupesName(t *testing.T) {
	ctx := context.Background()
	service := newProjectService(t)

	first, err := service.Insert(ctx, Project{
		ID: "p-aaaa", Name: "demo", RepoPath: "/tmp/demo-a",
		BaseBranch: "main", ExchangeName: "flow", ExchangeURL: "file:///tmp/a.git",
	})
	if err != nil {
		t.Fatalf("insert first project: %v", err)
	}
	if first.Name != "demo" {
		t.Fatalf("first name = %q, want demo", first.Name)
	}

	second, err := service.Insert(ctx, Project{
		ID: "p-bbbb", Name: "demo", RepoPath: "/tmp/demo-b",
		BaseBranch: "main", ExchangeName: "flow", ExchangeURL: "file:///tmp/b.git",
	})
	if err != nil {
		t.Fatalf("insert second project: %v", err)
	}
	if second.Name != "demo-2" {
		t.Fatalf("second name = %q, want demo-2", second.Name)
	}

	third, err := service.Insert(ctx, Project{
		ID: "p-cccc", Name: "demo", RepoPath: "/tmp/demo-c",
		BaseBranch: "main", ExchangeName: "flow", ExchangeURL: "file:///tmp/c.git",
	})
	if err != nil {
		t.Fatalf("insert third project: %v", err)
	}
	if third.Name != "demo-3" {
		t.Fatalf("third name = %q, want demo-3", third.Name)
	}
}

func TestProjectServiceInsertRejectsDuplicateRepoPath(t *testing.T) {
	ctx := context.Background()
	service := newProjectService(t)

	if _, err := service.Insert(ctx, Project{
		ID: "p-aaaa", Name: "demo", RepoPath: "/tmp/demo",
		BaseBranch: "main", ExchangeName: "flow", ExchangeURL: "file:///tmp/a.git",
	}); err != nil {
		t.Fatalf("insert first project: %v", err)
	}

	if _, err := service.Insert(ctx, Project{
		ID: "p-bbbb", Name: "other", RepoPath: "/tmp/demo",
		BaseBranch: "main", ExchangeName: "flow", ExchangeURL: "file:///tmp/b.git",
	}); !errors.Is(err, ErrProjectRepoPathExists) {
		t.Fatalf("duplicate repo path err = %v, want ErrProjectRepoPathExists", err)
	}
}

func TestProjectServiceListOrdersByName(t *testing.T) {
	ctx := context.Background()
	service := newProjectService(t)

	for _, p := range []Project{
		{ID: "p-2222", Name: "zeta", RepoPath: "/tmp/z", BaseBranch: "main", ExchangeName: "flow", ExchangeURL: "file:///tmp/z.git"},
		{ID: "p-1111", Name: "alpha", RepoPath: "/tmp/a", BaseBranch: "main", ExchangeName: "flow", ExchangeURL: "file:///tmp/a.git"},
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
