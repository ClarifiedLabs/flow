CREATE TABLE agent_defs (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
	harness TEXT NOT NULL CHECK (harness IN ('harness')),
	model TEXT NOT NULL DEFAULT '',
	reasoning_effort TEXT NOT NULL DEFAULT '',
	prompt TEXT NOT NULL DEFAULT '',
	builtin INTEGER NOT NULL DEFAULT 0 CHECK (builtin IN (0, 1)),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

UPDATE app_metadata
SET value = '0002_global_agent_defs',
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
