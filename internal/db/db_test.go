package db

import (
	"context"
	"database/sql"
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
	assertAppliedMigrations(t, migrations,
		"0001_init",
		"0002_job_dispatch_keys",
		"0003_workflow_hold",
		"0004_workflow_review_cycles",
		"0005_features",
		"0006_convergence_promotions",
		"0007_history_captures",
	)

	var schemaVersion string
	if err := store.DB().QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_version'").Scan(&schemaVersion); err != nil {
		t.Fatalf("read schema version metadata: %v", err)
	}
	if schemaVersion != "0007_history_captures" {
		t.Fatalf("schema version = %q, want 0007_history_captures", schemaVersion)
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
	var reviewCycleBudgetDefault string
	if err := store.DB().QueryRowContext(ctx, `
SELECT dflt_value FROM pragma_table_info('workflow_runs') WHERE name = 'review_cycle_budget'`).Scan(&reviewCycleBudgetDefault); err != nil {
		t.Fatalf("inspect workflow_runs.review_cycle_budget: %v", err)
	}
	if reviewCycleBudgetDefault != "2" {
		t.Fatalf("workflow_runs.review_cycle_budget default = %q, want 2", reviewCycleBudgetDefault)
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

	var flowNodesKindSQL string
	if err := store.DB().QueryRowContext(ctx, `
SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'flow_nodes'`).Scan(&flowNodesKindSQL); err != nil {
		t.Fatalf("inspect flow_nodes table: %v", err)
	}
	if !strings.Contains(flowNodesKindSQL, "'finalize_rebase'") {
		t.Fatalf("flow_nodes kind CHECK %q missing 'finalize_rebase'", flowNodesKindSQL)
	}
	var featureIDColumn int
	if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'feature_id'`).Scan(&featureIDColumn); err != nil {
		t.Fatalf("inspect tasks.feature_id: %v", err)
	}
	if featureIDColumn != 1 {
		t.Fatalf("tasks.feature_id column missing")
	}

	assertTables(t, store,
		[]string{"tasks", "workflow_runs", "workflow_node_runs", "workflow_artifacts", "workflow_waits", "workflow_transitions", "jobs", "leases", "sessions", "changes", "features", "feature_rebases"},
		[]string{"projects", "workers", "tokens", "web_sessions", "web_bootstrap_tokens", "workflow_state", "transitions", "task_flow_cursor", "task_phase_handoffs", "flow_nodes_new", "flow_edges_backup"},
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
	assertAppliedMigrations(t, migrations,
		"0001_init",
		"0002_job_dispatch_keys",
		"0003_workflow_hold",
		"0004_workflow_review_cycles",
		"0005_features",
	)
}

func TestReviewCycleMigrationBackfillsExistingWorkflowTransitions(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open pre-migration database: %v", err)
	}
	defer database.Close()
	for _, name := range []string{
		"migrations/0001_init.sql",
		"migrations/0002_job_dispatch_keys.sql",
		"migrations/0003_workflow_hold.sql",
	} {
		migration, err := migrationFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := database.ExecContext(ctx, string(migration)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := database.ExecContext(ctx, `
INSERT INTO tasks (id, title, created_by, created_at, updated_at)
VALUES ('t-existing', 'Existing loop', 'human', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO workflow_runs (
	id, task_id, run_sequence, flow_snapshot_json, state, current_node_key,
	transition_budget, created_at
) VALUES (
	'wr-existing',
	't-existing',
	1,
	'{"nodes":[{"key":"review","kind":"change_review","config":{"change_review":{"agents":[]}}},{"key":"implement","kind":"agent","config":{"agent":{"workspace":"change"}}}]}',
	'running',
	'review',
	50,
	'2026-01-01T00:00:00Z'
);
INSERT INTO workflow_transitions (
	task_id, workflow_run_id, from_node_key, to_node_key, outcome,
	event_kind, created_at
) VALUES
	('t-existing', 'wr-existing', 'review', 'implement', 'changes_requested', 'node_completed', '2026-01-01T00:01:00Z'),
	('t-existing', 'wr-existing', 'review', 'implement', 'approved', 'node_completed', '2026-01-01T00:02:00Z');
`); err != nil {
		t.Fatalf("seed existing review transitions: %v", err)
	}
	migration, err := migrationFS.ReadFile("migrations/0004_workflow_review_cycles.sql")
	if err != nil {
		t.Fatalf("read review-cycle migration: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply review-cycle migration: %v", err)
	}
	var budget, used int
	if err := database.QueryRowContext(ctx, `
SELECT review_cycle_budget, review_cycles_used
FROM workflow_runs
WHERE id = 'wr-existing'`).Scan(&budget, &used); err != nil {
		t.Fatalf("read backfilled workflow run: %v", err)
	}
	if budget != 2 || used != 1 {
		t.Fatalf("review cycle budget/count = %d/%d, want 1/2 used/budget", used, budget)
	}
}

func TestFeaturesMigrationPreservesFlowGraphs(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open pre-migration database: %v", err)
	}
	defer database.Close()
	for _, name := range []string{
		"migrations/0001_init.sql",
		"migrations/0002_job_dispatch_keys.sql",
		"migrations/0003_workflow_hold.sql",
		"migrations/0004_workflow_review_cycles.sql",
	} {
		migration, err := migrationFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := database.ExecContext(ctx, string(migration)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := database.ExecContext(ctx, `
INSERT INTO flows (id, name, start_node_key, created_at, updated_at)
VALUES ('f-existing', 'existing', 'implement', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO flow_nodes (id, flow_id, node_key, name, kind, position)
VALUES
	('fn-1', 'f-existing', 'implement', 'Implement', 'agent', 1),
	('fn-2', 'f-existing', 'done', 'Done', 'terminal', 2);
INSERT INTO flow_edges (flow_id, from_node_key, outcome, to_node_key)
VALUES ('f-existing', 'implement', 'completed', 'done');
`); err != nil {
		t.Fatalf("seed existing flow graph: %v", err)
	}
	migration, err := migrationFS.ReadFile("migrations/0005_features.sql")
	if err != nil {
		t.Fatalf("read features migration: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply features migration: %v", err)
	}

	var nodeCount, edgeCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_nodes WHERE flow_id = 'f-existing'`).Scan(&nodeCount); err != nil {
		t.Fatalf("count preserved flow nodes: %v", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_edges WHERE flow_id = 'f-existing'`).Scan(&edgeCount); err != nil {
		t.Fatalf("count preserved flow edges: %v", err)
	}
	if nodeCount != 2 || edgeCount != 1 {
		t.Fatalf("preserved flow graph = %d nodes/%d edges, want 2/1", nodeCount, edgeCount)
	}
	if _, err := database.ExecContext(ctx, `
INSERT INTO flow_nodes (id, flow_id, node_key, name, kind, position)
VALUES ('fn-3', 'f-existing', 'finalize', 'Finalize', 'finalize_rebase', 3)`); err != nil {
		t.Fatalf("insert finalize_rebase node: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
INSERT INTO flows (id, name, created_at, updated_at) VALUES ('f-bad', 'bad', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO flow_nodes (id, flow_id, node_key, name, kind, position)
VALUES ('fn-4', 'f-bad', 'bogus', 'Bogus', 'bogus_kind', 1)`); err == nil {
		t.Fatal("flow_nodes kind CHECK should still reject unknown kinds")
	}
	var scratchTables int
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('flow_nodes_new', 'flow_edges_backup')`).Scan(&scratchTables); err != nil {
		t.Fatalf("inspect scratch tables: %v", err)
	}
	if scratchTables != 0 {
		t.Fatalf("rebuild scratch tables remain: %d", scratchTables)
	}
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
