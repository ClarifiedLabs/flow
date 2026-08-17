INSERT INTO app_metadata (key, value, updated_at)
VALUES ('agent_defs_revision', '1', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

UPDATE app_metadata
SET value = '0002_agent_def_revisions', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
