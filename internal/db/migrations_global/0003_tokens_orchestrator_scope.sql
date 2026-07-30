-- Widen the tokens.scope CHECK to allow the dedicated 'orchestrator' scope
-- (authorizes only GET /v2/queue/stats). SQLite cannot alter a CHECK
-- constraint, so rebuild the table.
CREATE TABLE tokens_new (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token_hash TEXT NOT NULL UNIQUE,
	scope TEXT NOT NULL CHECK (scope IN ('owner', 'worker', 'session', 'console', 'hook', 'orchestrator')),
	subject TEXT NOT NULL DEFAULT '',
	project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
	source_task_id TEXT,
	expires_at TEXT,
	revoked_at TEXT,
	created_at TEXT NOT NULL,
	CHECK (scope NOT IN ('session', 'console') OR project_id IS NOT NULL)
);

INSERT INTO tokens_new (id, token_hash, scope, subject, project_id, source_task_id, expires_at, revoked_at, created_at)
SELECT id, token_hash, scope, subject, project_id, source_task_id, expires_at, revoked_at, created_at FROM tokens;

DROP TABLE tokens;
ALTER TABLE tokens_new RENAME TO tokens;

CREATE INDEX idx_tokens_scope_subject ON tokens(scope, subject);
