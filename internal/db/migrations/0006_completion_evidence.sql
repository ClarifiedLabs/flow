-- 0006_completion_evidence.sql: optional resolution message and typed evidence
-- on completed tasks. Both are nullable; existing done tasks carry NULL (no
-- message/evidence), and the lifecycle CHECK constraints on done_resolution /
-- done_at are unaffected.

ALTER TABLE tasks
    ADD COLUMN done_message TEXT;

ALTER TABLE tasks
    ADD COLUMN done_evidence_json TEXT;

UPDATE app_metadata
SET value = '0006_completion_evidence', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
