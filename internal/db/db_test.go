package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenInitializesSQLite(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer store.Close()

	var foreignKeys int
	if err := store.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	var journalMode string
	if err := store.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode pragma: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	migrations, err := store.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	assertAppliedMigrations(t, migrations, "0001_init", "0002_job_dispatch_keys")

	var schemaVersion string
	if err := store.DB().QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_version'").Scan(&schemaVersion); err != nil {
		t.Fatalf("read schema version metadata: %v", err)
	}
	if schemaVersion != "0002_job_dispatch_keys" {
		t.Fatalf("schema version = %q, want 0002_job_dispatch_keys", schemaVersion)
	}
	assertStorageFormat(t, store, "4")

	var dispatchDefault string
	if err := store.DB().QueryRowContext(ctx, `
SELECT dflt_value FROM pragma_table_info('jobs') WHERE name = 'dispatch_key'`).Scan(&dispatchDefault); err != nil {
		t.Fatalf("inspect jobs.dispatch_key: %v", err)
	}
	if dispatchDefault != "''" {
		t.Fatalf("jobs.dispatch_key default = %q, want empty string", dispatchDefault)
	}
	var dispatchIndexSQL string
	if err := store.DB().QueryRowContext(ctx, `
SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_jobs_live_dispatch_key'`).Scan(&dispatchIndexSQL); err != nil {
		t.Fatalf("inspect dispatch-key index: %v", err)
	}
	for _, fragment := range []string{"UNIQUE INDEX", "dispatch_key", "queued", "claimed", "running"} {
		if !strings.Contains(dispatchIndexSQL, fragment) {
			t.Fatalf("dispatch-key index SQL %q missing %q", dispatchIndexSQL, fragment)
		}
	}
	assertColumnAbsent(t, store, "tasks", "acceptance_"+"criteria")

	assertTables(t, store,
		[]string{"tasks", "workflow_runs", "workflow_node_runs", "workflow_artifacts", "workflow_waits", "workflow_transitions", "jobs", "leases", "sessions", "changes"},
		[]string{"projects", "workers", "tokens", "web_sessions", "web_bootstrap_tokens", "workflow_state", "transitions", "task_flow_cursor", "task_phase_handoffs"},
	)
}

func TestOpenGlobalInitializesGlobalSchema(t *testing.T) {
	ctx := context.Background()
	store, err := OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	defer store.Close()

	var foreignKeys int
	if err := store.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	var journalMode string
	if err := store.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode pragma: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	migrations, err := store.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	assertAppliedMigrations(t, migrations, "0001_global_init", "0002_global_agent_defs")
	assertStorageFormat(t, store, "4")
	assertColumnAbsent(t, store, "projects", "exchange_url")

	assertTables(t, store, []string{"app_metadata", "projects", "workers", "tokens", "web_sessions", "web_bootstrap_tokens", "idempotency_records", "agent_defs"}, []string{"tasks", "jobs", "leases", "sessions", "changes"})
}

func TestOpenRejectsPreviousStorageFormat(t *testing.T) {
	for _, test := range []struct {
		name string
		open func(context.Context, string) (*Store, error)
	}{
		{name: "project", open: Open},
		{name: "global", open: OpenGlobal},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), test.name+".db")
			store, err := test.open(ctx, path)
			if err != nil {
				t.Fatalf("initialize database: %v", err)
			}
			if _, err := store.DB().ExecContext(ctx, "UPDATE app_metadata SET value = '3' WHERE key = 'storage_format'"); err != nil {
				t.Fatalf("downgrade storage marker: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("close database: %v", err)
			}

			if _, err := test.open(ctx, path); err == nil || !strings.Contains(err.Error(), "recreate the Flow data directory") {
				t.Fatalf("reopen err = %v, want incompatible-format guidance", err)
			}
		})
	}
}

func assertColumnAbsent(t *testing.T, store *Store, table string, column string) {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?",
		table,
		column,
	).Scan(&count); err != nil {
		t.Fatalf("inspect %s.%s: %v", table, column, err)
	}
	if count != 0 {
		t.Fatalf("column %s.%s still exists", table, column)
	}
}

func assertStorageFormat(t *testing.T, store *Store, want string) {
	t.Helper()
	var got string
	if err := store.DB().QueryRow("SELECT value FROM app_metadata WHERE key = 'storage_format'").Scan(&got); err != nil {
		t.Fatalf("read storage format: %v", err)
	}
	if got != want {
		t.Fatalf("storage format = %q, want %q", got, want)
	}
}

func TestOpenGlobalMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "global.db")

	first, err := OpenGlobal(ctx, dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first database: %v", err)
	}

	second, err := OpenGlobal(ctx, dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()

	migrations, err := second.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	assertAppliedMigrations(t, migrations, "0001_global_init", "0002_global_agent_defs")
}

func TestGlobalTokensCarryProjectBinding(t *testing.T) {
	ctx := context.Background()
	store, err := OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	defer store.Close()

	if _, err := store.DB().ExecContext(ctx, `
	INSERT INTO projects (id, name, repo_path, base_branch, exchange_name, created_at, updated_at)
	VALUES ('p-1234', 'demo', '/tmp/demo', 'main', 'flow', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO tokens (token_hash, scope, subject, project_id, created_at)
VALUES ('hash-1', 'session', 's-abc', 'p-1234', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert session token with project binding: %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO tokens (token_hash, scope, subject, project_id, created_at)
VALUES ('hash-2', 'session', 's-def', NULL, '2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("session token without project binding should be rejected")
	}

	var projectID string
	if err := store.DB().QueryRowContext(ctx, "SELECT project_id FROM tokens WHERE token_hash = 'hash-1'").Scan(&projectID); err != nil {
		t.Fatalf("read token project binding: %v", err)
	}
	if projectID != "p-1234" {
		t.Fatalf("project_id = %q, want p-1234", projectID)
	}
}

func assertTables(t *testing.T, store *Store, want []string, absent []string) {
	t.Helper()

	rows, err := store.DB().Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}

	for _, name := range want {
		if !tables[name] {
			t.Fatalf("missing table %q", name)
		}
	}
	for _, name := range absent {
		if tables[name] {
			t.Fatalf("table %q should not exist in this database", name)
		}
	}
}

func TestOpenMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "flow.db")

	first, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first database: %v", err)
	}

	second, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()

	migrations, err := second.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	assertAppliedMigrations(t, migrations, "0001_init", "0002_job_dispatch_keys")
}

func assertAppliedMigrations(t *testing.T, got []string, want ...string) {
	t.Helper()

	seen := map[string]bool{}
	for _, migration := range got {
		seen[migration] = true
	}
	for _, migration := range want {
		if !seen[migration] {
			t.Fatalf("migrations = %v, missing %s", got, migration)
		}
	}
}
