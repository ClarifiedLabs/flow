-- Repair databases that recorded 0008 before legacy pending handoffs and
-- declarations were backfilled. Valid declarations and every expected row stay
-- immutable; incompatible declarations are explicitly audited and terminalized.

CREATE TRIGGER harness_archive_members_no_update
BEFORE UPDATE ON harness_archive_members
BEGIN
    SELECT RAISE(ABORT, 'Harness archive members are immutable');
END;
CREATE TRIGGER harness_archive_members_no_delete
BEFORE DELETE ON harness_archive_members
BEGIN
    SELECT RAISE(ABORT, 'Harness archive members are retained');
END;

-- A separate declaration marker makes even an empty archive-member index an
-- immutable exact set. Existing nonempty indexes are declared at migration time.
CREATE TABLE harness_archive_member_sets (
    artifact_id TEXT PRIMARY KEY REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    member_count INTEGER NOT NULL CHECK (member_count >= 0),
    declared_at TEXT NOT NULL
);
INSERT INTO harness_archive_member_sets (artifact_id, capture_id, member_count, declared_at)
SELECT artifact.id, artifact.capture_id, COUNT(member.id), MIN(member.created_at)
FROM history_artifacts artifact
JOIN harness_archive_members member ON member.artifact_id = artifact.id
GROUP BY artifact.id, artifact.capture_id;
CREATE TRIGGER harness_archive_member_sets_insert_guard
BEFORE INSERT ON harness_archive_member_sets
WHEN NEW.capture_id IS NOT (SELECT capture_id FROM history_artifacts WHERE id = NEW.artifact_id)
    OR NEW.member_count != (SELECT COUNT(*) FROM harness_archive_members WHERE artifact_id = NEW.artifact_id)
BEGIN
    SELECT RAISE(ABORT, 'invalid Harness archive member set declaration');
END;
CREATE TRIGGER harness_archive_members_insert_guard
BEFORE INSERT ON harness_archive_members
WHEN EXISTS (SELECT 1 FROM harness_archive_member_sets declared WHERE declared.artifact_id = NEW.artifact_id)
BEGIN
    SELECT RAISE(ABORT, 'Harness archive member sets are immutable');
END;
CREATE TRIGGER harness_archive_member_sets_no_update
BEFORE UPDATE ON harness_archive_member_sets
BEGIN
    SELECT RAISE(ABORT, 'Harness archive member sets are immutable');
END;
CREATE TRIGGER harness_archive_member_sets_no_delete
BEFORE DELETE ON harness_archive_member_sets
BEGIN
    SELECT RAISE(ABORT, 'Harness archive member sets are retained');
END;

-- Keep each reconciliation class query index-backed and limit-bounded, both for
-- project-wide sweeps and capture-scoped repair calls.
DROP INDEX idx_history_artifacts_pending;
CREATE INDEX idx_history_artifacts_pending
    ON history_artifacts(reconcile_attempted_at, pending_at, id)
    WHERE publication_state = 'pending';
CREATE INDEX idx_history_artifacts_capture_pending
    ON history_artifacts(capture_id, reconcile_attempted_at, pending_at, id)
    WHERE publication_state = 'pending';

CREATE TEMP TABLE history_0009_expected_classification AS
WITH counts AS (
    SELECT
        capture.id AS capture_id,
        capture.state AS old_state,
        capture.version AS old_version,
        capture.expected_harness,
        capture.execution_verdict,
        capture.zero_harness_root_reason,
        capture.expected_final_artifact_count AS declared_count,
        (SELECT COUNT(*) FROM history_capture_expected_artifacts expected
         WHERE expected.capture_id = capture.id) AS expected_count,
        (SELECT COUNT(*) FROM history_capture_expected_artifacts expected
         WHERE expected.capture_id = capture.id AND expected.kind = 'manifest') AS expected_manifests,
        (SELECT COUNT(*) FROM history_capture_expected_artifacts expected
         WHERE expected.capture_id = capture.id AND expected.kind = 'harness_root') AS expected_roots,
        (SELECT COUNT(*) FROM history_artifacts artifact
         WHERE artifact.capture_id = capture.id AND artifact.phase = 'final' AND artifact.kind = 'manifest') AS actual_manifests,
        (SELECT COUNT(*) FROM history_artifacts artifact
         WHERE artifact.capture_id = capture.id AND artifact.phase = 'final' AND artifact.kind = 'harness_root') AS actual_roots,
        (SELECT COUNT(*)
         FROM history_capture_expected_artifacts expected
         JOIN history_artifacts artifact
           ON artifact.capture_id = expected.capture_id AND artifact.logical_key = expected.logical_key
         WHERE expected.capture_id = capture.id
           AND (artifact.kind != expected.kind OR artifact.phase != 'final')) AS occupied_expected_mismatches,
        (SELECT COUNT(*)
         FROM history_artifacts artifact
         LEFT JOIN history_capture_expected_artifacts expected
           ON expected.capture_id = artifact.capture_id AND expected.logical_key = artifact.logical_key
         WHERE artifact.capture_id = capture.id
           AND artifact.phase = 'final'
           AND artifact.kind IN ('manifest', 'harness_root')
           AND (expected.capture_id IS NULL OR expected.kind != artifact.kind)) AS unexpected_final_artifacts
    FROM history_captures capture
    WHERE capture.expected_set_declared_at IS NOT NULL
)
SELECT
    counts.*,
    CASE
        WHEN counts.declared_count IS NOT counts.expected_count
          OR counts.expected_manifests != 1
          OR (counts.expected_harness = 0 AND counts.expected_roots != 0)
          OR (counts.expected_harness = 1 AND counts.expected_roots = 0
              AND counts.execution_verdict IN ('pending', 'succeeded'))
          OR counts.actual_manifests > 1
          OR (counts.expected_harness = 0 AND counts.actual_roots > 0)
          OR counts.occupied_expected_mismatches != 0
          OR counts.unexpected_final_artifacts != 0
          OR (counts.expected_harness = 0 AND length(trim(counts.zero_harness_root_reason)) != 0)
          OR (counts.expected_roots != 0 AND length(trim(counts.zero_harness_root_reason)) != 0)
            THEN CASE WHEN counts.old_state IN ('complete', 'waived') THEN 'terminal' ELSE 'waive' END
        WHEN counts.expected_harness = 1 AND counts.expected_roots = 0
          AND length(trim(counts.zero_harness_root_reason)) = 0 THEN 'reason'
        ELSE 'valid'
    END AS action
FROM counts;

INSERT INTO history_capture_events (
    id, capture_id, event_kind, from_state, to_state, capture_version,
    actor, code, details_json, occurred_at
)
SELECT
    'he-' || lower(hex(randomblob(16))), classification.capture_id,
    'legacy_expected_set_classified', classification.old_state,
    CASE WHEN classification.action = 'waive' THEN 'waived' ELSE classification.old_state END,
    classification.old_version + CASE WHEN classification.action IN ('waive', 'reason') THEN 1 ELSE 0 END,
    'migration:0009', classification.action,
    json_object(
        'action', classification.action,
        'declared_count', classification.declared_count,
        'expected_count', classification.expected_count,
        'expected_manifests', classification.expected_manifests,
        'expected_roots', classification.expected_roots,
        'actual_manifests', classification.actual_manifests,
        'actual_roots', classification.actual_roots,
        'occupied_expected_mismatches', classification.occupied_expected_mismatches,
        'unexpected_final_artifacts', classification.unexpected_final_artifacts
    ),
    strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
FROM history_0009_expected_classification classification
WHERE classification.action != 'valid';

-- The reason column was introduced by 0008 and is part of the declaration
-- projection. Temporarily remove only that projection guard to backfill the one
-- valid legacy shape that could not previously record a reason.
DROP TRIGGER history_captures_expected_set_guard;
UPDATE history_captures
SET zero_harness_root_reason = 'Legacy declaration recorded zero Harness roots before reasons were required',
    version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id IN (
    SELECT capture_id FROM history_0009_expected_classification WHERE action = 'reason'
);
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

UPDATE history_captures
SET state = 'waived',
    error_code = 'legacy_expected_set_invalid',
    error_message = 'Legacy final expected set is incompatible with canonical completion rules',
    waiver_reason = 'Legacy final expected set was terminalized during migration because it is incompatible with canonical completion rules',
    waived_at = COALESCE(waived_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    upload_grant_revoked_at = COALESCE(upload_grant_revoked_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id IN (
    SELECT capture_id FROM history_0009_expected_classification WHERE action = 'waive'
);

-- The service inserts child rows before atomically declaring the parent
-- projection. Once declared, direct SQL cannot extend the expected set.
CREATE TRIGGER history_capture_expected_artifacts_insert_guard
BEFORE INSERT ON history_capture_expected_artifacts
WHEN (SELECT expected_set_declared_at FROM history_captures WHERE id = NEW.capture_id) IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable after declaration');
END;

DROP TABLE history_0009_expected_classification;

-- Pending rows predate upload intents but represent already-consumed temporary
-- uploads. Preserve idempotent producer retry and quota accounting by recording
-- those handoffs as consumed. Existing matching intents are left untouched.
INSERT INTO history_upload_intents (
    temporary_upload_id, capture_id, sha256, stored_size, state, artifact_id,
    created_at, heartbeat_at, consumed_at, abandoned_at
)
SELECT
    artifact.temporary_upload_id, artifact.capture_id, artifact.sha256,
    artifact.stored_size, 'consumed', artifact.id,
    artifact.created_at, artifact.pending_at, artifact.pending_at, NULL
FROM history_artifacts artifact
WHERE artifact.publication_state = 'pending'
  AND NOT EXISTS (
      SELECT 1 FROM history_upload_intents intent WHERE intent.artifact_id = artifact.id
  )
ORDER BY artifact.pending_at, artifact.id;

-- Abort rather than silently migrating a conflicting preexisting intent.
CREATE TEMP TABLE history_0009_intent_validation (
    valid INTEGER NOT NULL CHECK (valid = 1)
);
INSERT INTO history_0009_intent_validation (valid)
SELECT CASE WHEN EXISTS (
    SELECT 1
    FROM history_upload_intents intent
    WHERE intent.artifact_id = artifact.id
      AND intent.temporary_upload_id = artifact.temporary_upload_id
      AND intent.capture_id = artifact.capture_id
      AND intent.sha256 = artifact.sha256
      AND intent.stored_size = artifact.stored_size
      AND intent.state = 'consumed'
) THEN 1 ELSE 0 END
FROM history_artifacts artifact
WHERE artifact.publication_state = 'pending';
DROP TABLE history_0009_intent_validation;

UPDATE app_metadata
SET value = '0009_history_capture_legacy_backfill',
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
