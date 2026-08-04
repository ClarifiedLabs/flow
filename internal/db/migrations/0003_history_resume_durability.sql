-- Durability and resume lifecycle additions layered on the published
-- full-fidelity history schema.

ALTER TABLE history_captures
    ADD COLUMN upload_grant_revoke_reason TEXT NOT NULL DEFAULT '' CHECK (length(upload_grant_revoke_reason) <= 4096);

-- An owner may revoke publication independently of terminalizing the capture.
-- Preserve all one-way lifecycle timestamps and the first explicit revoke reason.
DROP TRIGGER history_captures_lifecycle_timestamp_guard;
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
    OR (OLD.upload_grant_revoked_at IS NOT NULL AND NEW.upload_grant_revoked_at IS NOT OLD.upload_grant_revoked_at)
    OR (OLD.upload_grant_revoke_reason != '' AND NEW.upload_grant_revoke_reason IS NOT OLD.upload_grant_revoke_reason)
    OR (OLD.running_at IS NULL AND NEW.running_at IS NOT NULL AND NEW.state != 'running')
    OR (OLD.quiescing_at IS NULL AND NEW.quiescing_at IS NOT NULL AND NEW.state != 'quiescing')
    OR (OLD.sealed_at IS NULL AND NEW.sealed_at IS NOT NULL AND NEW.state != 'sealed')
    OR (OLD.uploading_at IS NULL AND NEW.uploading_at IS NOT NULL AND NEW.state != 'uploading')
    OR (OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL AND NEW.state != 'complete')
    OR (OLD.blocked_at IS NULL AND NEW.blocked_at IS NOT NULL AND NEW.state != 'blocked')
    OR (OLD.lost_at IS NULL AND NEW.lost_at IS NOT NULL AND NEW.state != 'lost')
    OR (OLD.waived_at IS NULL AND NEW.waived_at IS NOT NULL AND NEW.state != 'waived')
    OR (NEW.upload_grant_revoke_reason != '' AND NEW.upload_grant_revoked_at IS NULL)
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

-- Manifest generation takes an append-only lock that freezes producer-owned
-- inventory while still allowing the coordinator's canonical manifest insert.
CREATE TABLE history_manifest_locks (
    capture_id TEXT PRIMARY KEY REFERENCES history_captures(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL
);
-- Preserve the immutable inventory boundary for manifests committed under the
-- published version-2 schema before this lock table existed.
INSERT INTO history_manifest_locks (capture_id, created_at)
SELECT capture_id, COALESCE(committed_at, created_at)
FROM history_artifacts
WHERE kind = 'manifest'
  AND phase = 'final'
  AND publication_state = 'committed'
ON CONFLICT (capture_id) DO NOTHING;
CREATE TRIGGER history_manifest_locks_no_update
BEFORE UPDATE ON history_manifest_locks
BEGIN
    SELECT RAISE(ABORT, 'history manifest locks are immutable');
END;
CREATE TRIGGER history_manifest_locks_no_delete
BEFORE DELETE ON history_manifest_locks
BEGIN
    SELECT RAISE(ABORT, 'history manifest locks are retained');
END;
CREATE TRIGGER history_artifacts_final_inventory_freeze
BEFORE INSERT ON history_artifacts
WHEN NEW.kind != 'manifest'
 AND EXISTS (SELECT 1 FROM history_manifest_locks WHERE capture_id = NEW.capture_id)
BEGIN
    SELECT RAISE(ABORT, 'history artifact inventory is frozen after manifest generation');
END;
CREATE TRIGGER history_transcript_segments_final_inventory_freeze
BEFORE INSERT ON history_transcript_segments
WHEN EXISTS (SELECT 1 FROM history_manifest_locks WHERE capture_id = NEW.capture_id)
BEGIN
    SELECT RAISE(ABORT, 'history transcript inventory is frozen after manifest generation');
END;
CREATE TRIGGER harness_archive_members_final_inventory_freeze
BEFORE INSERT ON harness_archive_members
WHEN EXISTS (
    SELECT 1 FROM history_manifest_locks
    WHERE capture_id = (SELECT capture_id FROM history_artifacts WHERE id = NEW.artifact_id)
)
BEGIN
    SELECT RAISE(ABORT, 'Harness member inventory is frozen after manifest generation');
END;
CREATE TRIGGER harness_archive_member_sets_final_inventory_freeze
BEFORE INSERT ON harness_archive_member_sets
WHEN EXISTS (SELECT 1 FROM history_manifest_locks WHERE capture_id = NEW.capture_id)
BEGIN
    SELECT RAISE(ABORT, 'Harness member inventory is frozen after manifest generation');
END;
CREATE TRIGGER history_transcript_streams_final_inventory_freeze
BEFORE UPDATE ON history_transcript_streams
WHEN EXISTS (SELECT 1 FROM history_manifest_locks WHERE capture_id = NEW.capture_id)
 AND (NEW.state IS NOT OLD.state OR NEW.segment_count IS NOT OLD.segment_count
      OR NEW.logical_length IS NOT OLD.logical_length OR NEW.last_epoch IS NOT OLD.last_epoch
      OR NEW.stream_sha256 IS NOT OLD.stream_sha256)
BEGIN
    SELECT RAISE(ABORT, 'history transcript inventory is frozen after manifest generation');
END;
CREATE TRIGGER history_workspace_summaries_final_inventory_freeze
BEFORE INSERT ON history_workspace_summaries
WHEN EXISTS (SELECT 1 FROM history_manifest_locks WHERE capture_id = NEW.capture_id)
BEGIN
    SELECT RAISE(ABORT, 'history workspace inventory is frozen after manifest generation');
END;

-- The value stores the selected source session, which may be a delegated child;
-- use a name that does not incorrectly imply that it is always the archive root.
ALTER TABLE history_resumes
    RENAME COLUMN source_root_session_id TO source_native_session_id;
ALTER TABLE history_resumes
    ADD COLUMN requested_native_session_id TEXT NOT NULL DEFAULT '' CHECK (length(requested_native_session_id) <= 255);

CREATE UNIQUE INDEX idx_history_resumes_idempotency
    ON history_resumes(source_capture_id, requested_by, idempotency_key)
    WHERE idempotency_key <> '';

CREATE TRIGGER trg_history_resumes_job_state
AFTER UPDATE OF state ON jobs
FOR EACH ROW
WHEN OLD.state <> NEW.state
BEGIN
    UPDATE history_resumes
    SET state = CASE NEW.state
            WHEN 'queued' THEN 'queued'
            WHEN 'claimed' THEN 'claimed'
            WHEN 'running' THEN 'running'
            WHEN 'finished' THEN 'completed'
            WHEN 'failed' THEN 'failed'
            WHEN 'crashed' THEN 'failed'
            WHEN 'canceled' THEN 'cancelled'
            ELSE state
        END,
        claimed_at = CASE WHEN NEW.state = 'claimed' AND claimed_at IS NULL THEN NEW.updated_at ELSE claimed_at END,
        running_at = CASE WHEN NEW.state = 'running' AND running_at IS NULL THEN NEW.updated_at ELSE running_at END,
        completed_at = CASE WHEN NEW.state = 'finished' THEN NEW.updated_at ELSE completed_at END,
        failed_at = CASE WHEN NEW.state IN ('failed', 'crashed') THEN NEW.updated_at ELSE failed_at END,
        cancelled_at = CASE WHEN NEW.state = 'canceled' THEN NEW.updated_at ELSE cancelled_at END,
        error_code = CASE
            WHEN NEW.state = 'crashed' THEN 'job_crashed'
            WHEN NEW.state = 'failed' THEN 'job_failed'
            ELSE error_code
        END,
        updated_at = NEW.updated_at
    WHERE job_id = NEW.id;
END;

CREATE TRIGGER trg_history_resumes_immutable_lineage
BEFORE UPDATE OF source_capture_id, source_native_session_id, requested_native_session_id, source_harness_artifact_id,
    source_harness_sha256, source_workspace_artifact_id, source_workspace_sha256,
    target_task_id, target_change_id, job_id, required_head_commit,
    required_harness_build, required_harness_schema_version, requested_by, idempotency_key
ON history_resumes
FOR EACH ROW
BEGIN
    SELECT RAISE(ABORT, 'history resume lineage is immutable');
END;

UPDATE app_metadata
SET value = '0003_history_resume_durability', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
