package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func newCredentialServiceForTest(t *testing.T) (*CredentialService, *flowdb.Store) {
	t.Helper()

	store, err := flowdb.OpenGlobal(context.Background(), filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return NewCredentialService(store.DB()), store
}

func TestEnsureTokenPreservesRevocation(t *testing.T) {
	ctx := context.Background()
	service, store := newCredentialServiceForTest(t)
	if err := service.EnsureToken(ctx, CredentialInput{
		Token: "owner-token",
		Scope: TokenScopeOwner,
	}); err != nil {
		t.Fatalf("ensure owner token: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE tokens
SET revoked_at = ?
WHERE token_hash = ?`, formatTime(time.Now().UTC()), HashToken("owner-token")); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	if err := service.EnsureToken(ctx, CredentialInput{
		Token: "owner-token",
		Scope: TokenScopeOwner,
	}); err != nil {
		t.Fatalf("ensure owner token after revocation: %v", err)
	}

	if _, err := service.Authenticate(ctx, "owner-token"); err != ErrInvalidCredential {
		t.Fatalf("Authenticate revoked token err = %v, want ErrInvalidCredential", err)
	}
}

func TestSessionTokenCarriesProjectBinding(t *testing.T) {
	ctx := context.Background()
	service, store := newCredentialServiceForTest(t)

	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO projects (id, name, repo_path, base_branch, exchange_name, exchange_url, created_at, updated_at)
VALUES ('p-1234', 'demo', '/tmp/demo', 'main', 'flow', 'file:///tmp/demo.git', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	projectID := "p-1234"
	if err := service.EnsureToken(ctx, CredentialInput{
		Token:     "session-token",
		Scope:     TokenScopeSession,
		Subject:   "s-abc",
		ProjectID: &projectID,
	}); err != nil {
		t.Fatalf("ensure session token: %v", err)
	}

	principal, err := service.Authenticate(ctx, "session-token")
	if err != nil {
		t.Fatalf("authenticate session token: %v", err)
	}
	if principal.ProjectID == nil || *principal.ProjectID != "p-1234" {
		t.Fatalf("principal project id = %v, want p-1234", principal.ProjectID)
	}

	owner, err := service.CreateToken(ctx, CredentialInput{Scope: TokenScopeOwner})
	if err != nil {
		t.Fatalf("create owner token: %v", err)
	}
	ownerPrincipal, err := service.Authenticate(ctx, owner)
	if err != nil {
		t.Fatalf("authenticate owner token: %v", err)
	}
	if ownerPrincipal.ProjectID != nil {
		t.Fatalf("owner principal project id = %v, want nil", ownerPrincipal.ProjectID)
	}
}

func TestSessionTokenRequiresProjectBinding(t *testing.T) {
	ctx := context.Background()
	service, _ := newCredentialServiceForTest(t)

	err := service.EnsureToken(ctx, CredentialInput{
		Token:   "session-token",
		Scope:   TokenScopeSession,
		Subject: "s-abc",
	})
	if err == nil {
		t.Fatal("session token without project binding should be rejected")
	}
}

func TestReplaceSubjectTokenRevokesPreviousWorkerToken(t *testing.T) {
	ctx := context.Background()
	service, _ := newCredentialServiceForTest(t)

	first, err := service.ReplaceSubjectToken(ctx, CredentialInput{
		Scope:   TokenScopeWorker,
		Subject: "w-local",
	})
	if err != nil {
		t.Fatalf("create first worker token: %v", err)
	}
	second, err := service.ReplaceSubjectToken(ctx, CredentialInput{
		Scope:   TokenScopeWorker,
		Subject: "w-local",
	})
	if err != nil {
		t.Fatalf("replace worker token: %v", err)
	}
	if first == second {
		t.Fatal("replacement token matched first token")
	}
	if _, err := service.Authenticate(ctx, first); err != ErrInvalidCredential {
		t.Fatalf("old token authenticate err = %v, want ErrInvalidCredential", err)
	}
	principal, err := service.Authenticate(ctx, second)
	if err != nil {
		t.Fatalf("authenticate replacement token: %v", err)
	}
	if principal.Scope != TokenScopeWorker || principal.Subject != "w-local" {
		t.Fatalf("principal = %+v, want worker w-local", principal)
	}
}

func TestReplaceSubjectCredentialUsesSuppliedToken(t *testing.T) {
	ctx := context.Background()
	service, _ := newCredentialServiceForTest(t)

	if err := service.ReplaceSubjectCredential(ctx, CredentialInput{
		Token: "owner-token-1",
		Scope: TokenScopeOwner,
	}); err != nil {
		t.Fatalf("store first owner token: %v", err)
	}
	if err := service.ReplaceSubjectCredential(ctx, CredentialInput{
		Token: "owner-token-2",
		Scope: TokenScopeOwner,
	}); err != nil {
		t.Fatalf("replace owner token: %v", err)
	}

	if _, err := service.Authenticate(ctx, "owner-token-1"); err != ErrInvalidCredential {
		t.Fatalf("old owner token authenticate err = %v, want ErrInvalidCredential", err)
	}
	principal, err := service.Authenticate(ctx, "owner-token-2")
	if err != nil {
		t.Fatalf("authenticate replacement owner token: %v", err)
	}
	if principal.Scope != TokenScopeOwner || principal.Subject != "owner" {
		t.Fatalf("principal = %+v, want owner", principal)
	}
}
