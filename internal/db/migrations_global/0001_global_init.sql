-- Coordinator-global database launch schema.

CREATE TABLE app_metadata (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO app_metadata (key, value, updated_at)
VALUES
	('schema_version', '0001_global_init', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	('storage_format', '5', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE projects (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	repo_path TEXT UNIQUE,
	base_branch TEXT NOT NULL,
	exchange_name TEXT NOT NULL,
	exchange_path TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK (length(trim(name)) > 0),
	CHECK (length(trim(base_branch)) > 0)
);

CREATE TABLE workers (
	id TEXT PRIMARY KEY,
	labels_json TEXT NOT NULL DEFAULT '{}',
	taints_json TEXT NOT NULL DEFAULT '[]',
	harness_models_json TEXT NOT NULL DEFAULT '[]',
	capacity_persistent_agent INTEGER NOT NULL DEFAULT 0 CHECK (capacity_persistent_agent >= 0),
	capacity_ephemeral INTEGER NOT NULL DEFAULT 0 CHECK (capacity_ephemeral >= 0),
	status TEXT NOT NULL CHECK (status IN ('registered', 'offline')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_heartbeat_at TEXT,
	expires_at TEXT
);

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

CREATE TABLE tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token_hash TEXT NOT NULL UNIQUE,
	scope TEXT NOT NULL CHECK (scope IN ('owner', 'worker', 'session', 'console', 'hook', 'orchestrator', 'provisioner')),
	subject TEXT NOT NULL DEFAULT '',
	project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
	source_task_id TEXT,
	expires_at TEXT,
	revoked_at TEXT,
	created_at TEXT NOT NULL,
	CHECK (scope NOT IN ('session', 'console') OR project_id IS NOT NULL)
);

CREATE TABLE web_bootstrap_tokens (
	token_hash TEXT PRIMARY KEY,
	expires_at TEXT NOT NULL,
	used_at TEXT,
	created_at TEXT NOT NULL
);

CREATE TABLE web_sessions (
	id TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	csrf_token_hash TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	last_seen_at TEXT NOT NULL
);

CREATE TABLE idempotency_records (
	principal_key TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	method TEXT NOT NULL,
	path TEXT NOT NULL,
	request_hash TEXT NOT NULL,
	status_code INTEGER NOT NULL CHECK (status_code >= 100 AND status_code <= 599),
	response_body TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (principal_key, idempotency_key)
);

-- Named indexes.

CREATE INDEX idx_idempotency_pending
ON idempotency_records(status_code, created_at);

CREATE INDEX idx_tokens_scope_subject ON tokens(scope, subject);

CREATE INDEX idx_web_sessions_expires_at
ON web_sessions(expires_at);
