-- Full-fidelity history capture, discovery, export, and resume metadata.
-- Foreign keys are deferred while the two CHECK-constrained tables are rebuilt;
-- the migration tests run foreign_key_check after both old tables are replaced.
PRAGMA defer_foreign_keys = ON;

ALTER TABLE history_captures
    ADD COLUMN change_id TEXT NOT NULL DEFAULT '' CHECK (length(change_id) <= 255);
ALTER TABLE history_captures
    ADD COLUMN harness_schema_version INTEGER NOT NULL DEFAULT 0 CHECK (harness_schema_version >= 0);
ALTER TABLE history_captures
    ADD COLUMN resumed_from_capture_id TEXT REFERENCES history_captures(id) ON DELETE RESTRICT;
ALTER TABLE history_captures
    ADD COLUMN resumed_from_harness_session_id TEXT NOT NULL DEFAULT '' CHECK (length(resumed_from_harness_session_id) <= 255);

DROP TRIGGER history_captures_immutable_attribution;
CREATE TRIGGER history_captures_immutable_attribution
BEFORE UPDATE ON history_captures
WHEN NEW.id IS NOT OLD.id
    OR NEW.project_id IS NOT OLD.project_id
    OR NEW.job_id IS NOT OLD.job_id
    OR NEW.lease_id IS NOT OLD.lease_id
    OR NEW.lease_attempt IS NOT OLD.lease_attempt
    OR NEW.worker_id IS NOT OLD.worker_id
    OR NEW.task_id IS NOT OLD.task_id
    OR NEW.change_id IS NOT OLD.change_id
    OR NEW.session_id IS NOT OLD.session_id
    OR NEW.workflow_run_id IS NOT OLD.workflow_run_id
    OR NEW.node_run_id IS NOT OLD.node_run_id
    OR NEW.node_visit IS NOT OLD.node_visit
    OR NEW.stage IS NOT OLD.stage
    OR NEW.role IS NOT OLD.role
    OR NEW.harness_name IS NOT OLD.harness_name
    OR NEW.harness_version IS NOT OLD.harness_version
    OR NEW.harness_schema_version IS NOT OLD.harness_schema_version
    OR NEW.resumed_from_capture_id IS NOT OLD.resumed_from_capture_id
    OR NEW.resumed_from_harness_session_id IS NOT OLD.resumed_from_harness_session_id
    OR NEW.expected_transcript IS NOT OLD.expected_transcript
    OR NEW.expected_harness IS NOT OLD.expected_harness
BEGIN
    SELECT RAISE(ABORT, 'history capture attribution is immutable');
END;

-- SQLite cannot alter a CHECK constraint. Rebuild the expected-set table while
-- preserving all rows and its append-only guards.
CREATE TABLE history_capture_expected_artifacts_new (
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    logical_key TEXT NOT NULL CHECK (length(trim(logical_key)) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('harness_root', 'workspace_snapshot', 'manifest')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (capture_id, logical_key)
);
INSERT INTO history_capture_expected_artifacts_new (capture_id, logical_key, kind, created_at)
SELECT capture_id, logical_key, kind, created_at
FROM history_capture_expected_artifacts;
DROP TABLE history_capture_expected_artifacts;
ALTER TABLE history_capture_expected_artifacts_new RENAME TO history_capture_expected_artifacts;

CREATE TRIGGER history_capture_expected_artifacts_insert_guard
BEFORE INSERT ON history_capture_expected_artifacts
WHEN (SELECT expected_set_declared_at FROM history_captures WHERE id = NEW.capture_id) IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable after declaration');
END;
CREATE TRIGGER history_capture_expected_artifacts_no_delete
BEFORE DELETE ON history_capture_expected_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable');
END;
CREATE TRIGGER history_capture_expected_artifacts_no_update
BEFORE UPDATE ON history_capture_expected_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable');
END;

-- Rebuild the artifact table to admit reconstructive workspace snapshots. The
-- table name is never changed until after the old table is dropped, so existing
-- dependent foreign-key declarations continue to target history_artifacts.
-- SQLite reparses triggers during DROP TABLE, so temporarily remove the one
-- trigger on a dependent table that queries history_artifacts.
DROP TRIGGER harness_archive_member_sets_insert_guard;
CREATE TABLE history_artifacts_new (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'ha-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    logical_key TEXT NOT NULL CHECK (length(trim(logical_key)) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('transcript_segment', 'harness_root', 'workspace_snapshot', 'manifest')),
    phase TEXT NOT NULL CHECK (phase IN ('checkpoint', 'final')),
    checkpoint_generation INTEGER,
    checkpoint_stream TEXT NOT NULL DEFAULT '' CHECK (length(checkpoint_stream) <= 255),
    checkpoint_trigger TEXT NOT NULL DEFAULT '' CHECK (length(checkpoint_trigger) <= 64),
    archive_id TEXT NOT NULL DEFAULT '' CHECK (length(archive_id) <= 255),
    media_type TEXT NOT NULL CHECK (length(trim(media_type)) BETWEEN 1 AND 255),
    format_version INTEGER NOT NULL DEFAULT 1 CHECK (format_version > 0),
    schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    logical_size INTEGER NOT NULL CHECK (logical_size >= 0),
    entry_count INTEGER NOT NULL DEFAULT 0 CHECK (entry_count >= 0),
    publication_state TEXT NOT NULL CHECK (publication_state IN ('pending', 'committed')),
    temporary_upload_id TEXT NOT NULL DEFAULT '' CHECK (length(temporary_upload_id) <= 255),
    blob_key TEXT NOT NULL UNIQUE CHECK (length(blob_key) = 65),
    superseded_by_artifact_id TEXT REFERENCES history_artifacts_new(id) ON DELETE RESTRICT,
    superseded_at TEXT,
    pending_at TEXT NOT NULL,
    reconcile_attempted_at TEXT,
    committed_at TEXT,
    created_at TEXT NOT NULL,

    UNIQUE (capture_id, logical_key),
    CHECK ((phase = 'checkpoint') = (checkpoint_generation IS NOT NULL)),
    CHECK (checkpoint_generation IS NULL OR checkpoint_generation > 0),
    CHECK (phase = 'checkpoint' OR checkpoint_trigger = ''),
    CHECK (kind != 'transcript_segment' OR (phase = 'final' AND checkpoint_generation IS NULL)),
    CHECK ((publication_state = 'committed') = (committed_at IS NOT NULL)),
    CHECK (publication_state != 'pending' OR length(temporary_upload_id) > 0),
    CHECK ((superseded_by_artifact_id IS NULL) = (superseded_at IS NULL)),
    CHECK (superseded_by_artifact_id IS NULL OR superseded_by_artifact_id != id)
);
INSERT INTO history_artifacts_new (
    id, capture_id, logical_key, kind, phase, checkpoint_generation,
    checkpoint_stream, checkpoint_trigger, archive_id, media_type,
    format_version, schema_version, sha256, stored_size, logical_size,
    entry_count, publication_state, temporary_upload_id, blob_key,
    superseded_by_artifact_id, superseded_at, pending_at,
    reconcile_attempted_at, committed_at, created_at
)
SELECT id, capture_id, logical_key, kind, phase, checkpoint_generation,
       checkpoint_stream, checkpoint_trigger, archive_id, media_type,
       format_version, schema_version, sha256, stored_size, logical_size,
       entry_count, publication_state, temporary_upload_id, blob_key,
       superseded_by_artifact_id, superseded_at, pending_at,
       reconcile_attempted_at, committed_at, created_at
FROM history_artifacts;
DROP TABLE history_artifacts;
ALTER TABLE history_artifacts_new RENAME TO history_artifacts;

CREATE TRIGGER harness_archive_member_sets_insert_guard
BEFORE INSERT ON harness_archive_member_sets
WHEN NEW.capture_id IS NOT (SELECT capture_id FROM history_artifacts WHERE id = NEW.artifact_id)
    OR NEW.member_count != (SELECT COUNT(*) FROM harness_archive_members WHERE artifact_id = NEW.artifact_id)
BEGIN
    SELECT RAISE(ABORT, 'invalid Harness archive member set declaration');
END;

CREATE INDEX idx_history_artifacts_capture_kind
    ON history_artifacts(capture_id, phase, kind, checkpoint_generation, logical_key);
CREATE INDEX idx_history_artifacts_capture_pending
    ON history_artifacts(capture_id, reconcile_attempted_at, pending_at, id)
    WHERE publication_state = 'pending';
CREATE UNIQUE INDEX idx_history_artifacts_checkpoint_stream_generation
    ON history_artifacts(capture_id, kind, checkpoint_stream, checkpoint_generation)
    WHERE phase = 'checkpoint';
CREATE INDEX idx_history_artifacts_pending
    ON history_artifacts(reconcile_attempted_at, pending_at, id)
    WHERE publication_state = 'pending';
CREATE INDEX idx_history_artifacts_temporary
    ON history_artifacts(temporary_upload_id) WHERE publication_state = 'pending';

CREATE TRIGGER history_artifacts_checkpoint_shape_insert
BEFORE INSERT ON history_artifacts
WHEN NOT (
    (NEW.phase = 'checkpoint' AND length(trim(NEW.checkpoint_stream)) > 0)
    OR (NEW.phase = 'final' AND NEW.checkpoint_stream = '')
)
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact checkpoint stream');
END;
CREATE TRIGGER history_artifacts_committed_at_guard
BEFORE UPDATE ON history_artifacts
WHEN NEW.publication_state IS OLD.publication_state
    AND NEW.committed_at IS NOT OLD.committed_at
BEGIN
    SELECT RAISE(ABORT, 'history artifact committed timestamp is immutable');
END;
CREATE TRIGGER history_artifacts_immutable_metadata
BEFORE UPDATE ON history_artifacts
WHEN NEW.id IS NOT OLD.id
    OR NEW.capture_id IS NOT OLD.capture_id
    OR NEW.logical_key IS NOT OLD.logical_key
    OR NEW.kind IS NOT OLD.kind
    OR NEW.phase IS NOT OLD.phase
    OR NEW.checkpoint_generation IS NOT OLD.checkpoint_generation
    OR NEW.checkpoint_stream IS NOT OLD.checkpoint_stream
    OR NEW.checkpoint_trigger IS NOT OLD.checkpoint_trigger
    OR NEW.archive_id IS NOT OLD.archive_id
    OR NEW.media_type IS NOT OLD.media_type
    OR NEW.format_version IS NOT OLD.format_version
    OR NEW.schema_version IS NOT OLD.schema_version
    OR NEW.sha256 IS NOT OLD.sha256
    OR NEW.stored_size IS NOT OLD.stored_size
    OR NEW.logical_size IS NOT OLD.logical_size
    OR NEW.entry_count IS NOT OLD.entry_count
    OR NEW.temporary_upload_id IS NOT OLD.temporary_upload_id
    OR NEW.blob_key IS NOT OLD.blob_key
    OR NEW.pending_at IS NOT OLD.pending_at
    OR NEW.created_at IS NOT OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'history artifact metadata is immutable');
END;
CREATE TRIGGER history_artifacts_no_delete
BEFORE DELETE ON history_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history artifacts are retained');
END;
CREATE TRIGGER history_artifacts_publication_guard
BEFORE UPDATE ON history_artifacts
WHEN NEW.publication_state IS NOT OLD.publication_state
    AND NOT (
        OLD.publication_state = 'pending'
        AND NEW.publication_state = 'committed'
        AND OLD.committed_at IS NULL
        AND NEW.committed_at IS NOT NULL
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact publication transition');
END;
CREATE TRIGGER history_artifacts_supersession_guard
BEFORE UPDATE ON history_artifacts
WHEN (OLD.superseded_by_artifact_id IS NOT NULL AND (
        NEW.superseded_by_artifact_id IS NULL
        OR NEW.superseded_at IS NULL
        OR ((NEW.superseded_by_artifact_id IS NOT OLD.superseded_by_artifact_id
                OR NEW.superseded_at IS NOT OLD.superseded_at)
            AND NOT EXISTS (
                SELECT 1 FROM history_artifacts final_manifest
                WHERE final_manifest.id = NEW.superseded_by_artifact_id
                  AND final_manifest.capture_id = NEW.capture_id
                  AND final_manifest.kind = 'manifest'
                  AND final_manifest.phase = 'final'
                  AND final_manifest.publication_state = 'committed'
            ))
    ))
    OR (OLD.superseded_by_artifact_id IS NULL AND NEW.superseded_by_artifact_id IS NOT NULL
        AND NEW.superseded_at IS NULL)
    OR (OLD.superseded_by_artifact_id IS NULL AND NEW.superseded_by_artifact_id IS NULL
        AND NEW.superseded_at IS NOT OLD.superseded_at)
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact supersession transition');
END;

CREATE TABLE history_workspace_summaries (
    artifact_id TEXT PRIMARY KEY REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    archive_schema_version INTEGER NOT NULL CHECK (archive_schema_version > 0),
    branch TEXT NOT NULL DEFAULT '' CHECK (length(branch) <= 1024),
    detached INTEGER NOT NULL DEFAULT 0 CHECK (detached IN (0, 1)),
    base_ref TEXT NOT NULL DEFAULT '' CHECK (length(base_ref) <= 1024),
    base_commit TEXT NOT NULL DEFAULT '' CHECK (length(base_commit) <= 64),
    head_commit TEXT NOT NULL CHECK (length(trim(head_commit)) BETWEEN 1 AND 64),
    staged_count INTEGER NOT NULL DEFAULT 0 CHECK (staged_count >= 0),
    unstaged_count INTEGER NOT NULL DEFAULT 0 CHECK (unstaged_count >= 0),
    untracked_count INTEGER NOT NULL DEFAULT 0 CHECK (untracked_count >= 0),
    inventory_digest TEXT NOT NULL
        CHECK (length(inventory_digest) = 64 AND inventory_digest NOT GLOB '*[^0-9a-f]*'),
    validation_status TEXT NOT NULL CHECK (validation_status IN ('valid', 'invalid', 'unsupported')),
    created_at TEXT NOT NULL,
    UNIQUE (capture_id, artifact_id)
);
CREATE INDEX idx_history_workspace_summaries_capture
    ON history_workspace_summaries(capture_id, artifact_id);
CREATE TRIGGER history_workspace_summaries_insert_guard
BEFORE INSERT ON history_workspace_summaries
WHEN NOT EXISTS (
    SELECT 1 FROM history_artifacts artifact
    WHERE artifact.id = NEW.artifact_id
      AND artifact.capture_id = NEW.capture_id
      AND artifact.kind = 'workspace_snapshot'
      AND artifact.phase = 'final'
      AND artifact.publication_state = 'committed'
)
BEGIN
    SELECT RAISE(ABORT, 'workspace summary requires a committed final workspace artifact');
END;
CREATE TRIGGER history_workspace_summaries_no_update
BEFORE UPDATE ON history_workspace_summaries
BEGIN
    SELECT RAISE(ABORT, 'history workspace summaries are immutable');
END;
CREATE TRIGGER history_workspace_summaries_no_delete
BEFORE DELETE ON history_workspace_summaries
BEGIN
    SELECT RAISE(ABORT, 'history workspace summaries are retained');
END;

CREATE TABLE history_resumes (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'hr-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    source_capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    source_root_session_id TEXT NOT NULL CHECK (length(trim(source_root_session_id)) BETWEEN 1 AND 255),
    source_harness_artifact_id TEXT NOT NULL REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    source_harness_sha256 TEXT NOT NULL
        CHECK (length(source_harness_sha256) = 64 AND source_harness_sha256 NOT GLOB '*[^0-9a-f]*'),
    source_workspace_artifact_id TEXT NOT NULL REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    source_workspace_sha256 TEXT NOT NULL
        CHECK (length(source_workspace_sha256) = 64 AND source_workspace_sha256 NOT GLOB '*[^0-9a-f]*'),
    target_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    target_change_id TEXT NOT NULL REFERENCES changes(id) ON DELETE RESTRICT,
    job_id TEXT NOT NULL UNIQUE REFERENCES jobs(id) ON DELETE RESTRICT,
    required_head_commit TEXT NOT NULL CHECK (length(trim(required_head_commit)) BETWEEN 1 AND 64),
    required_harness_build TEXT NOT NULL CHECK (length(trim(required_harness_build)) BETWEEN 1 AND 128),
    required_harness_schema_version INTEGER NOT NULL CHECK (required_harness_schema_version > 0),
    state TEXT NOT NULL CHECK (state IN (
        'queued', 'claimed', 'restoring', 'running', 'completed', 'failed', 'cancelled'
    )),
    requested_by TEXT NOT NULL CHECK (length(trim(requested_by)) BETWEEN 1 AND 255),
    idempotency_key TEXT NOT NULL DEFAULT '' CHECK (length(idempotency_key) <= 255),
    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    error_message TEXT NOT NULL DEFAULT '' CHECK (length(error_message) <= 1024),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    claimed_at TEXT,
    restoring_at TEXT,
    running_at TEXT,
    completed_at TEXT,
    failed_at TEXT,
    cancelled_at TEXT,
    CHECK (state != 'claimed' OR claimed_at IS NOT NULL),
    CHECK (state != 'restoring' OR restoring_at IS NOT NULL),
    CHECK (state != 'running' OR running_at IS NOT NULL),
    CHECK (state != 'completed' OR completed_at IS NOT NULL),
    CHECK (state != 'failed' OR failed_at IS NOT NULL),
    CHECK (state != 'cancelled' OR cancelled_at IS NOT NULL)
);
CREATE INDEX idx_history_resumes_source
    ON history_resumes(source_capture_id, created_at DESC, id DESC);
CREATE INDEX idx_history_resumes_target
    ON history_resumes(target_task_id, target_change_id, created_at DESC, id DESC);
CREATE INDEX idx_history_resumes_state
    ON history_resumes(state, updated_at, id);

CREATE INDEX idx_history_captures_project_time
    ON history_captures(project_id, reserved_at DESC, id DESC);
CREATE INDEX idx_history_captures_project_task_time
    ON history_captures(project_id, task_id, reserved_at DESC, id DESC) WHERE task_id <> '';
CREATE INDEX idx_history_captures_project_job_time
    ON history_captures(project_id, job_id, reserved_at DESC, id DESC);
CREATE INDEX idx_history_captures_project_session_time
    ON history_captures(project_id, session_id, reserved_at DESC, id DESC) WHERE session_id <> '';
CREATE INDEX idx_history_captures_lineage
    ON history_captures(project_id, resumed_from_capture_id, reserved_at DESC, id DESC)
    WHERE resumed_from_capture_id IS NOT NULL;

UPDATE app_metadata
SET value = '0002_full_fidelity_history', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
