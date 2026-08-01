-- Forward-compatible hardening for history capture databases that already
-- recorded the originally shipped 0007_history_captures migration.

ALTER TABLE history_captures ADD COLUMN upload_grant_generation INTEGER NOT NULL DEFAULT 1
    CHECK (upload_grant_generation >= 1);
ALTER TABLE history_captures ADD COLUMN upload_grant_rotated_at TEXT;
ALTER TABLE history_captures ADD COLUMN zero_harness_root_reason TEXT NOT NULL DEFAULT ''
    CHECK (length(zero_harness_root_reason) <= 1024);

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
    OR NEW.session_id IS NOT OLD.session_id
    OR NEW.workflow_run_id IS NOT OLD.workflow_run_id
    OR NEW.node_run_id IS NOT OLD.node_run_id
    OR NEW.node_visit IS NOT OLD.node_visit
    OR NEW.stage IS NOT OLD.stage
    OR NEW.role IS NOT OLD.role
    OR NEW.harness_name IS NOT OLD.harness_name
    OR NEW.harness_version IS NOT OLD.harness_version
    OR NEW.expected_transcript IS NOT OLD.expected_transcript
    OR NEW.expected_harness IS NOT OLD.expected_harness
BEGIN
    SELECT RAISE(ABORT, 'history capture attribution is immutable');
END;

CREATE TRIGGER history_captures_grant_rotation_guard
BEFORE UPDATE ON history_captures
WHEN NEW.upload_grant_hash IS NOT OLD.upload_grant_hash
    AND NOT (
        OLD.upload_grant_revoked_at IS NULL
        AND NEW.upload_grant_revoked_at IS NULL
        AND NEW.upload_grant_generation = OLD.upload_grant_generation + 1
        AND NEW.upload_grant_rotated_at IS NOT NULL
        AND NEW.upload_grant_rotated_at IS NOT OLD.upload_grant_rotated_at
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history upload grant rotation');
END;

CREATE TRIGGER history_captures_grant_generation_guard
BEFORE UPDATE ON history_captures
WHEN NEW.upload_grant_hash IS OLD.upload_grant_hash
    AND (
        NEW.upload_grant_generation IS NOT OLD.upload_grant_generation
        OR NEW.upload_grant_rotated_at IS NOT OLD.upload_grant_rotated_at
    )
BEGIN
    SELECT RAISE(ABORT, 'history upload grant generation requires rotation');
END;

CREATE TRIGGER history_captures_state_transition_guard
BEFORE UPDATE ON history_captures
WHEN NEW.state IS NOT OLD.state
    AND NOT (
        (OLD.state = 'reserved' AND NEW.state = 'running')
        OR (OLD.state = 'running' AND NEW.state = 'quiescing')
        OR (OLD.state = 'quiescing' AND NEW.state = 'sealed')
        OR (OLD.state = 'sealed' AND NEW.state = 'uploading')
        OR (NEW.state IN ('blocked', 'lost') AND OLD.state NOT IN ('complete', 'waived'))
        OR (OLD.state IN ('blocked', 'lost') AND NEW.state IN ('running', 'quiescing', 'sealed', 'uploading'))
        OR (NEW.state = 'complete' AND OLD.state IN ('uploading', 'sealed', 'blocked', 'lost'))
        OR (NEW.state = 'waived' AND OLD.state != 'complete')
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture state transition');
END;

CREATE TRIGGER history_captures_version_guard
BEFORE UPDATE ON history_captures
WHEN NEW.version < OLD.version
    OR NEW.version > OLD.version + 1
    OR (NEW.state IS NOT OLD.state AND NEW.version != OLD.version + 1)
    OR (NEW.execution_verdict IS NOT OLD.execution_verdict AND NEW.version != OLD.version + 1)
    OR (NEW.expected_set_declared_at IS NOT OLD.expected_set_declared_at AND NEW.version != OLD.version + 1)
    OR (NEW.upload_grant_hash IS NOT OLD.upload_grant_hash AND NEW.version != OLD.version + 1)
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture version transition');
END;

CREATE TRIGGER history_captures_execution_guard
BEFORE UPDATE ON history_captures
WHEN (NEW.execution_verdict IS NOT OLD.execution_verdict AND (
        OLD.execution_verdict != 'pending'
        OR NEW.execution_verdict = 'pending'
        OR NEW.execution_recorded_at IS NULL
    ))
    OR (NEW.execution_verdict IS OLD.execution_verdict AND (
        NEW.execution_exit_code IS NOT OLD.execution_exit_code
        OR NEW.execution_error_code IS NOT OLD.execution_error_code
        OR NEW.execution_recorded_at IS NOT OLD.execution_recorded_at
    ))
BEGIN
    SELECT RAISE(ABORT, 'invalid history execution verdict transition');
END;

CREATE TRIGGER history_captures_expected_set_guard
BEFORE UPDATE ON history_captures
WHEN OLD.expected_set_declared_at IS NOT NULL AND (
        NEW.expected_set_declared_at IS NOT OLD.expected_set_declared_at
        OR NEW.expected_final_artifact_count IS NOT OLD.expected_final_artifact_count
        OR NEW.expected_transcript_epoch IS NOT OLD.expected_transcript_epoch
        OR NEW.expected_transcript_segment_count IS NOT OLD.expected_transcript_segment_count
        OR NEW.expected_transcript_length IS NOT OLD.expected_transcript_length
        OR NEW.expected_transcript_sha256 IS NOT OLD.expected_transcript_sha256
        OR NEW.zero_harness_root_reason IS NOT OLD.zero_harness_root_reason
    )
BEGIN
    SELECT RAISE(ABORT, 'history capture expected set is immutable');
END;

CREATE TRIGGER history_captures_lifecycle_timestamp_guard
BEFORE UPDATE ON history_captures
WHEN (OLD.running_at IS NOT NULL AND NEW.running_at IS NOT OLD.running_at)
    OR (OLD.quiescing_at IS NOT NULL AND NEW.quiescing_at IS NOT OLD.quiescing_at)
    OR (OLD.sealed_at IS NOT NULL AND NEW.sealed_at IS NOT OLD.sealed_at)
    OR (OLD.uploading_at IS NOT NULL AND NEW.uploading_at IS NOT OLD.uploading_at)
    OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS NOT OLD.completed_at)
    OR (OLD.blocked_at IS NOT NULL AND NEW.blocked_at IS NOT OLD.blocked_at)
    OR (OLD.lost_at IS NOT NULL AND NEW.lost_at IS NOT OLD.lost_at)
    OR (OLD.waived_at IS NOT NULL AND NEW.waived_at IS NOT OLD.waived_at)
    OR (OLD.running_at IS NULL AND NEW.running_at IS NOT NULL AND NEW.state != 'running')
    OR (OLD.quiescing_at IS NULL AND NEW.quiescing_at IS NOT NULL AND NEW.state != 'quiescing')
    OR (OLD.sealed_at IS NULL AND NEW.sealed_at IS NOT NULL AND NEW.state != 'sealed')
    OR (OLD.uploading_at IS NULL AND NEW.uploading_at IS NOT NULL AND NEW.state != 'uploading')
    OR (OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL AND NEW.state != 'complete')
    OR (OLD.blocked_at IS NULL AND NEW.blocked_at IS NOT NULL AND NEW.state != 'blocked')
    OR (OLD.lost_at IS NULL AND NEW.lost_at IS NOT NULL AND NEW.state != 'lost')
    OR (OLD.waived_at IS NULL AND NEW.waived_at IS NOT NULL AND NEW.state != 'waived')
    OR (OLD.upload_grant_revoked_at IS NULL AND NEW.upload_grant_revoked_at IS NOT NULL
        AND NEW.state NOT IN ('complete', 'waived'))
    OR (NEW.state = 'running' AND NEW.running_at IS NULL)
    OR (NEW.state = 'quiescing' AND NEW.quiescing_at IS NULL)
    OR (NEW.state = 'sealed' AND NEW.sealed_at IS NULL)
    OR (NEW.state = 'uploading' AND NEW.uploading_at IS NULL)
    OR (NEW.state = 'complete' AND (NEW.completed_at IS NULL OR NEW.upload_grant_revoked_at IS NULL))
    OR (NEW.state = 'blocked' AND NEW.blocked_at IS NULL)
    OR (NEW.state = 'lost' AND NEW.lost_at IS NULL)
    OR (NEW.state = 'waived' AND (NEW.waived_at IS NULL OR NEW.upload_grant_revoked_at IS NULL))
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture lifecycle timestamps');
END;

ALTER TABLE history_artifacts ADD COLUMN checkpoint_stream TEXT NOT NULL DEFAULT ''
    CHECK (length(checkpoint_stream) <= 255);
ALTER TABLE history_artifacts ADD COLUMN reconcile_attempted_at TEXT;
UPDATE history_artifacts
SET checkpoint_stream = 'legacy-' || id
WHERE phase = 'checkpoint';

DROP INDEX idx_history_artifacts_pending;
CREATE INDEX idx_history_artifacts_pending
    ON history_artifacts(publication_state, reconcile_attempted_at, pending_at);
CREATE UNIQUE INDEX idx_history_artifacts_checkpoint_stream_generation
    ON history_artifacts(capture_id, kind, checkpoint_stream, checkpoint_generation)
    WHERE phase = 'checkpoint';

CREATE TRIGGER history_artifacts_checkpoint_shape_insert
BEFORE INSERT ON history_artifacts
WHEN NOT (
    (NEW.phase = 'checkpoint' AND length(trim(NEW.checkpoint_stream)) > 0)
    OR (NEW.phase = 'final' AND NEW.checkpoint_stream = '')
)
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact checkpoint stream');
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

CREATE TRIGGER history_artifacts_committed_at_guard
BEFORE UPDATE ON history_artifacts
WHEN NEW.publication_state IS OLD.publication_state
    AND NEW.committed_at IS NOT OLD.committed_at
BEGIN
    SELECT RAISE(ABORT, 'history artifact committed timestamp is immutable');
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

ALTER TABLE history_transcript_segments ADD COLUMN raw_sha256 TEXT NOT NULL DEFAULT ''
    CHECK (raw_sha256 = '' OR (length(raw_sha256) = 64 AND raw_sha256 NOT GLOB '*[^0-9a-f]*'));

CREATE TRIGGER history_transcript_segments_raw_insert_guard
BEFORE INSERT ON history_transcript_segments
WHEN length(NEW.raw_sha256) != 64 OR NEW.raw_sha256 GLOB '*[^0-9a-f]*'
BEGIN
    SELECT RAISE(ABORT, 'history transcript raw digest is required');
END;

DROP TRIGGER history_transcript_segments_no_update;
CREATE TRIGGER history_transcript_segments_no_update
BEFORE UPDATE ON history_transcript_segments
WHEN NOT (
    OLD.raw_sha256 = ''
    AND length(NEW.raw_sha256) = 64
    AND NEW.raw_sha256 NOT GLOB '*[^0-9a-f]*'
    AND NEW.capture_id IS OLD.capture_id
    AND NEW.epoch IS OLD.epoch
    AND NEW.sequence IS OLD.sequence
    AND NEW.start_offset IS OLD.start_offset
    AND NEW.end_offset IS OLD.end_offset
    AND NEW.uncompressed_size IS OLD.uncompressed_size
    AND NEW.stored_size IS OLD.stored_size
    AND NEW.sha256 IS OLD.sha256
    AND NEW.encoding IS OLD.encoding
    AND NEW.artifact_id IS OLD.artifact_id
    AND NEW.sealed_at IS OLD.sealed_at
    AND NEW.created_at IS OLD.created_at
)
BEGIN
    SELECT RAISE(ABORT, 'history transcript segments are immutable');
END;

CREATE TABLE history_upload_intents (
    temporary_upload_id TEXT PRIMARY KEY
        CHECK (length(temporary_upload_id) = 32
            AND temporary_upload_id NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'consumed', 'abandoned')),
    artifact_id TEXT UNIQUE REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    consumed_at TEXT,
    abandoned_at TEXT,
    CHECK (
        (state = 'active' AND artifact_id IS NULL AND consumed_at IS NULL AND abandoned_at IS NULL)
        OR (state = 'consumed' AND artifact_id IS NOT NULL AND consumed_at IS NOT NULL AND abandoned_at IS NULL)
        OR (state = 'abandoned' AND artifact_id IS NULL AND consumed_at IS NULL AND abandoned_at IS NOT NULL)
    )
);

CREATE INDEX idx_history_upload_intents_capture_state
    ON history_upload_intents(capture_id, state, heartbeat_at);
CREATE INDEX idx_history_upload_intents_active_heartbeat
    ON history_upload_intents(heartbeat_at) WHERE state = 'active';

CREATE TRIGGER history_upload_intents_update_guard
BEFORE UPDATE ON history_upload_intents
WHEN NEW.temporary_upload_id IS NOT OLD.temporary_upload_id
    OR NEW.capture_id IS NOT OLD.capture_id
    OR NEW.sha256 IS NOT OLD.sha256
    OR NEW.stored_size IS NOT OLD.stored_size
    OR NEW.created_at IS NOT OLD.created_at
    OR OLD.state != 'active'
    OR NOT (
        (NEW.state = 'active'
            AND NEW.artifact_id IS NULL
            AND NEW.consumed_at IS NULL
            AND NEW.abandoned_at IS NULL
            AND NEW.heartbeat_at >= OLD.heartbeat_at)
        OR (NEW.state = 'consumed'
            AND NEW.artifact_id IS NOT NULL
            AND NEW.consumed_at IS NOT NULL
            AND NEW.abandoned_at IS NULL)
        OR (NEW.state = 'abandoned'
            AND NEW.artifact_id IS NULL
            AND NEW.consumed_at IS NULL
            AND NEW.abandoned_at IS NOT NULL)
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history upload intent transition');
END;

CREATE TRIGGER history_upload_intents_no_delete
BEFORE DELETE ON history_upload_intents
BEGIN
    SELECT RAISE(ABORT, 'history upload intents are retained');
END;

UPDATE app_metadata
SET value = '0008_history_capture_hardening',
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
