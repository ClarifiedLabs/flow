ALTER TABLE flows
    ADD COLUMN revision INTEGER NOT NULL DEFAULT 1
    CHECK (revision > 0);

UPDATE app_metadata
SET value = '0009_flow_revisions', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
