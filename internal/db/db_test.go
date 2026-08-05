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
	assertAppliedMigrations(t, migrations, "0001_init", "0002_full_fidelity_history", "0003_history_resume_durability")

	var schemaVersion string
	if err := store.DB().QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_version'").Scan(&schemaVersion); err != nil {
		t.Fatalf("read schema version metadata: %v", err)
	}
	if schemaVersion != "0003_history_resume_durability" {
		t.Fatalf("schema version = %q, want 0003_history_resume_durability", schemaVersion)
	}
	assertStorageFormat(t, store, "7")

	var relationPayloadNotNull int
	var relationPayloadDefault string
	if err := store.DB().QueryRowContext(ctx, `
SELECT "notnull", dflt_value
FROM pragma_table_info('feature_creation_intents')
WHERE name = 'relation_payload_json'`).Scan(&relationPayloadNotNull, &relationPayloadDefault); err != nil {
		t.Fatalf("inspect feature creation relation payload: %v", err)
	}
	if relationPayloadNotNull != 1 || relationPayloadDefault != "'[]'" {
		t.Fatalf("relation payload notnull/default = %d/%q, want 1/%q", relationPayloadNotNull, relationPayloadDefault, "'[]'")
	}
	var ownershipNotNull int
	var ownershipDefault string
	if err := store.DB().QueryRowContext(ctx, `
SELECT "notnull", dflt_value
FROM pragma_table_info('feature_creation_intents')
WHERE name = 'ref_created_by_intent'`).Scan(&ownershipNotNull, &ownershipDefault); err != nil {
		t.Fatalf("inspect feature ref ownership: %v", err)
	}
	if ownershipNotNull != 1 || ownershipDefault != "FALSE" {
		t.Fatalf("ref ownership notnull/default = %d/%q, want 1/FALSE", ownershipNotNull, ownershipDefault)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO feature_creation_intents (
	id, operation_key, title, branch, target_branch, target_sha, created_by, created_at, updated_at
) VALUES ('fci-default', 'default-payload', 'default payload', 'feature/default-payload', 'main', 'abc', 'human', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert feature intent with default relation payload: %v", err)
	}
	var relationPayload string
	var refCreatedByIntent bool
	if err := store.DB().QueryRowContext(ctx, `SELECT relation_payload_json, ref_created_by_intent FROM feature_creation_intents WHERE id = 'fci-default'`).Scan(&relationPayload, &refCreatedByIntent); err != nil {
		t.Fatalf("read default feature intent durability fields: %v", err)
	}
	if relationPayload != "[]" || refCreatedByIntent {
		t.Fatalf("default feature relation payload/ownership = %q/%v, want []/false", relationPayload, refCreatedByIntent)
	}

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
		[]string{"tasks", "workflow_runs", "workflow_node_runs", "workflow_artifacts", "workflow_waits", "workflow_transitions", "jobs", "leases", "worker_assignments", "sessions", "changes", "features", "feature_rebases", "history_workspace_summaries", "history_resumes"},
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
	assertAppliedMigrations(t, migrations, "0001_global_init")
	var schemaVersion string
	if err := store.DB().QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_version'").Scan(&schemaVersion); err != nil {
		t.Fatalf("read schema version metadata: %v", err)
	}
	if schemaVersion != "0001_global_init" {
		t.Fatalf("schema version = %q, want 0001_global_init", schemaVersion)
	}
	assertStorageFormat(t, store, "6")
	assertColumnAbsent(t, store, "projects", "exchange_url")

	assertTables(t, store, []string{"app_metadata", "projects", "workers", "capacity_slots", "tokens", "web_sessions", "web_bootstrap_tokens", "idempotency_records", "agent_defs"}, []string{"tasks", "jobs", "leases", "sessions", "changes"})
}

func TestOpenRejectsPreviousStorageFormat(t *testing.T) {
	for _, test := range []struct {
		name           string
		previousFormat string
		open           func(context.Context, string) (*Store, error)
	}{
		{name: "project", previousFormat: "6", open: Open},
		{name: "global", previousFormat: "5", open: OpenGlobal},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), test.name+".db")
			store, err := test.open(ctx, path)
			if err != nil {
				t.Fatalf("initialize database: %v", err)
			}
			if _, err := store.DB().ExecContext(ctx, "UPDATE app_metadata SET value = ? WHERE key = 'storage_format'", test.previousFormat); err != nil {
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
	assertAppliedMigrations(t, migrations, "0001_global_init")
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

	for _, scope := range []string{"session", "console"} {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO tokens (token_hash, scope, subject, project_id, created_at)
VALUES (?, ?, 'unbound', NULL, '2026-01-01T00:00:00Z')`, "hash-unbound-"+scope, scope); err == nil {
			t.Fatalf("%s token without project binding should be rejected", scope)
		}
	}

	for _, scope := range []string{"orchestrator", "provisioner"} {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO tokens (token_hash, scope, subject, project_id, created_at)
VALUES (?, ?, 'coordinator', NULL, '2026-01-01T00:00:00Z')`, "hash-"+scope, scope); err != nil {
			t.Fatalf("insert %s token: %v", scope, err)
		}
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

func TestFullFidelityHistoryMigrationPreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flow.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	initial, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, string(initial)); err != nil {
		t.Fatalf("apply initial schema: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
CREATE TABLE schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO schema_migrations (version) VALUES ('0001_init');
INSERT INTO history_captures (
    id, project_id, job_id, lease_id, lease_attempt, worker_id, role,
    expected_transcript, expected_harness, upload_grant_hash, reserved_at, updated_at
) VALUES (
    'hc-00000000000000000000000000000001', 'project-1', 'job-1', 'lease-1', 1, 'worker-1', 'author',
    0, 1, ?, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'
);
INSERT INTO history_capture_expected_artifacts (capture_id, logical_key, kind, created_at)
VALUES ('hc-00000000000000000000000000000001', 'harness/final/root', 'harness_root', '2026-08-01T00:00:00Z');
INSERT INTO history_artifacts (
    id, capture_id, logical_key, kind, phase, archive_id, media_type,
    sha256, stored_size, logical_size, entry_count, publication_state,
    blob_key, pending_at, committed_at, created_at
) VALUES (
    'ha-00000000000000000000000000000001', 'hc-00000000000000000000000000000001',
    'harness/final/root', 'harness_root', 'final', 'root', 'application/x-tar',
    ?, 1, 1, 1, 'committed', ?, '2026-08-01T00:00:00Z',
    '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'
);
INSERT INTO harness_archive_members (
    id, capture_id, artifact_id, archive_id, native_session_id,
    relative_member_path, member_kind, parse_status, created_at
) VALUES (
    'hm-00000000000000000000000000000001', 'hc-00000000000000000000000000000001',
    'ha-00000000000000000000000000000001', 'root', 'native-root',
    'sessions/native-root', 'root', 'parsed', '2026-08-01T00:00:00Z'
);
INSERT INTO harness_archive_member_sets (artifact_id, capture_id, member_count, declared_at)
VALUES ('ha-00000000000000000000000000000001', 'hc-00000000000000000000000000000001', 1, '2026-08-01T00:00:00Z');`,
		strings.Repeat("0", 64), strings.Repeat("1", 64), strings.Repeat("b", 65)); err != nil {
		t.Fatalf("seed current history schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrate current history schema: %v", err)
	}
	defer store.Close()

	var artifactKind, nativeSession string
	if err := store.DB().QueryRowContext(ctx, `
SELECT artifact.kind, member.native_session_id
FROM history_artifacts artifact
JOIN harness_archive_members member ON member.artifact_id = artifact.id
WHERE artifact.id = 'ha-00000000000000000000000000000001'`).Scan(&artifactKind, &nativeSession); err != nil {
		t.Fatalf("read preserved history rows: %v", err)
	}
	if artifactKind != "harness_root" || nativeSession != "native-root" {
		t.Fatalf("preserved artifact kind/session = %q/%q", artifactKind, nativeSession)
	}
	var workspaceAllowed int
	if err := store.DB().QueryRowContext(ctx, `
SELECT instr(sql, 'workspace_snapshot') FROM sqlite_master
WHERE type = 'table' AND name = 'history_artifacts'`).Scan(&workspaceAllowed); err != nil {
		t.Fatal(err)
	}
	if workspaceAllowed == 0 {
		t.Fatal("migrated artifact kind constraint does not include workspace_snapshot")
	}
	rows, err := store.DB().QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowID, foreignKeyID int64
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("foreign key violation after migration: table=%s row=%d parent=%s fk=%d", table, rowID, parent, foreignKeyID)
	}
}

func TestHistoryResumeDurabilityMigrationUpgradesPublishedHistorySchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flow.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_init.sql", "0002_full_fidelity_history.sql"} {
		content, readErr := migrationFS.ReadFile("migrations/" + name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := raw.ExecContext(ctx, string(content)); execErr != nil {
			t.Fatalf("apply %s: %v", name, execErr)
		}
	}
	if _, err := raw.ExecContext(ctx, `
INSERT INTO history_captures (
    id, project_id, job_id, lease_id, lease_attempt, worker_id, role,
    expected_transcript, expected_harness, upload_grant_hash, reserved_at, updated_at
) VALUES (
    'hc-00000000000000000000000000000003', 'project-3', 'job-3', 'lease-3', 1, 'worker-3', 'author',
    0, 0, ?, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'
);
INSERT INTO history_artifacts (
    id, capture_id, logical_key, kind, phase, media_type, format_version, schema_version,
    sha256, stored_size, logical_size, entry_count, publication_state, blob_key,
    pending_at, committed_at, created_at
) VALUES (
    'ha-00000000000000000000000000000003', 'hc-00000000000000000000000000000003',
    'manifest/final', 'manifest', 'final', 'application/json', 1, 1,
    ?, 1, 1, 1, 'committed', ?,
    '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'
);`, strings.Repeat("0", 64), strings.Repeat("1", 64), strings.Repeat("c", 65)); err != nil {
		t.Fatalf("seed published manifest: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
CREATE TABLE schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO schema_migrations (version) VALUES ('0001_init'), ('0002_full_fidelity_history');`); err != nil {
		t.Fatalf("record published migrations: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade published history schema: %v", err)
	}
	defer store.Close()
	for _, check := range []struct {
		kind string
		name string
	}{
		{kind: "table", name: "history_manifest_locks"},
		{kind: "index", name: "idx_history_resumes_idempotency"},
		{kind: "trigger", name: "trg_history_resumes_job_state"},
		{kind: "trigger", name: "trg_history_resumes_immutable_lineage"},
	} {
		var count int
		if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, check.kind, check.name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s %s count = %d, want 1", check.kind, check.name, count)
		}
	}
	columns, err := store.DB().QueryContext(ctx, "PRAGMA table_info(history_resumes)")
	if err != nil {
		t.Fatal(err)
	}
	defer columns.Close()
	seen := map[string]bool{}
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := columns.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
	}
	if err := columns.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen["source_native_session_id"] || !seen["requested_native_session_id"] || seen["source_root_session_id"] {
		t.Fatalf("history resume columns = %v", seen)
	}
	var revokeColumn int
	if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM pragma_table_info('history_captures') WHERE name = 'upload_grant_revoke_reason'`).Scan(&revokeColumn); err != nil {
		t.Fatal(err)
	}
	if revokeColumn != 1 {
		t.Fatalf("upload grant revoke column count = %d, want 1", revokeColumn)
	}
	var manifestLock int
	if err := store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM history_manifest_locks WHERE capture_id = 'hc-00000000000000000000000000000003'`).Scan(&manifestLock); err != nil {
		t.Fatal(err)
	}
	if manifestLock != 1 {
		t.Fatalf("backfilled manifest lock count = %d, want 1", manifestLock)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO history_artifacts (
    id, capture_id, logical_key, kind, phase, media_type, format_version, schema_version,
    sha256, stored_size, logical_size, entry_count, publication_state, blob_key,
    pending_at, committed_at, created_at
) VALUES (
    'ha-00000000000000000000000000000004', 'hc-00000000000000000000000000000003',
    'workspace/final', 'workspace_snapshot', 'final', 'application/x-tar', 1, 1,
    ?, 1, 1, 1, 'committed', ?,
    '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z'
)`, strings.Repeat("2", 64), strings.Repeat("d", 65)); err == nil || !strings.Contains(err.Error(), "inventory is frozen") {
		t.Fatalf("post-upgrade inventory mutation error = %v", err)
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
	assertAppliedMigrations(t, migrations, "0001_init", "0002_full_fidelity_history", "0003_history_resume_durability")
}

func assertAppliedMigrations(t *testing.T, got []string, want ...string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("migrations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("migrations = %v, want %v", got, want)
		}
	}
}
