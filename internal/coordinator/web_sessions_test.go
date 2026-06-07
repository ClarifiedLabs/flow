package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestWebSessionBootstrapIsSingleUseAndAuthenticates(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	service := NewWebSessionService(store.DB())
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	bootstrap, err := service.CreateBootstrap(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("create bootstrap: %v", err)
	}
	if bootstrap.Token == "" || !bootstrap.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("bootstrap = %+v", bootstrap)
	}

	session, err := service.ConsumeBootstrap(ctx, bootstrap.Token, time.Hour)
	if err != nil {
		t.Fatalf("consume bootstrap: %v", err)
	}
	if session.ID == "" || session.Token == "" || session.CSRFToken == "" || !session.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("session = %+v", session)
	}

	if _, err := service.ConsumeBootstrap(ctx, bootstrap.Token, time.Hour); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("second consume err = %v, want ErrInvalidCredential", err)
	}
	authenticated, err := service.Authenticate(ctx, session.Token, "", false)
	if err != nil {
		t.Fatalf("authenticate without csrf for read: %v", err)
	}
	if authenticated.ID != session.ID || !authenticated.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("authenticated session = %+v, want id %q expires %s", authenticated, session.ID, session.ExpiresAt)
	}
	if _, err := service.Authenticate(ctx, session.Token, "", true); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("authenticate missing csrf err = %v, want ErrInvalidCredential", err)
	}
	if _, err := service.Authenticate(ctx, session.Token, session.CSRFToken, true); err != nil {
		t.Fatalf("authenticate with csrf: %v", err)
	}
}

func TestWebSessionBootstrapAndSessionExpire(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	service := NewWebSessionService(store.DB())
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	expiredBootstrap, err := service.CreateBootstrap(ctx, time.Minute)
	if err != nil {
		t.Fatalf("create expired bootstrap: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := service.ConsumeBootstrap(ctx, expiredBootstrap.Token, time.Hour); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expired bootstrap consume err = %v, want ErrInvalidCredential", err)
	}

	now = time.Date(2026, 6, 7, 13, 0, 0, 0, time.UTC)
	bootstrap, err := service.CreateBootstrap(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("create bootstrap: %v", err)
	}
	session, err := service.ConsumeBootstrap(ctx, bootstrap.Token, time.Minute)
	if err != nil {
		t.Fatalf("consume bootstrap: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := service.Authenticate(ctx, session.Token, session.CSRFToken, true); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expired session auth err = %v, want ErrInvalidCredential", err)
	}
}
