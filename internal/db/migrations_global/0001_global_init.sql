-- Coordinator-global database launch schema.

CREATE TABLE app_metadata (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO app_metadata (key, value, updated_at)
VALUES
	('schema_version', '0001_global_init', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	('storage_format', '6', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

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
	capacity_bucket TEXT NOT NULL DEFAULT '' CHECK (capacity_bucket IN ('', 'persistent_agent', 'ephemeral')),
	status TEXT NOT NULL CHECK (status IN ('registered', 'offline')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_heartbeat_at TEXT,
	expires_at TEXT
);

-- A capacity slot is one provisioned, one-shot worker process. Slots exist
-- before a project job is selected so the orchestrator can keep verified idle
-- capacity without weakening project-local assignment ownership.
CREATE TABLE capacity_slots (
	id TEXT PRIMARY KEY,
	worker_id TEXT NOT NULL UNIQUE CHECK (length(trim(worker_id)) > 0),
	provider_id TEXT NOT NULL CHECK (length(trim(provider_id)) > 0),
	profile_name TEXT NOT NULL CHECK (length(trim(profile_name)) > 0),
	provider_request_id TEXT NOT NULL CHECK (length(trim(provider_request_id)) > 0),
	provider_type TEXT NOT NULL CHECK (length(trim(provider_type)) > 0),
	provider_options_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(provider_options_json)),
	state TEXT NOT NULL CHECK (state IN ('provisioning', 'unready', 'ready', 'bound', 'closed')),
	profile_labels_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(profile_labels_json)),
	profile_taints_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(profile_taints_json)),
	profile_harness_models_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(profile_harness_models_json)),
	allowed_roles_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(allowed_roles_json)),
	allowed_buckets_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(allowed_buckets_json)),
	required_selector_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(required_selector_json)),
	startup_deadline TEXT NOT NULL,
	retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
	next_retry_at TEXT,
	last_attempt_at TEXT,
	capability_checked_at TEXT,
	capability_error TEXT NOT NULL DEFAULT '',
	assignment_id TEXT,
	project_id TEXT REFERENCES projects(id) ON DELETE RESTRICT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	closed_at TEXT,
	close_reason TEXT,
	last_provider_error TEXT,
	credentials_revoked_at TEXT,
	cleaned_at TEXT,
	UNIQUE (provider_id, provider_request_id),
	CHECK ((state = 'bound') = (assignment_id IS NOT NULL AND project_id IS NOT NULL) OR state = 'closed'),
	CHECK ((state = 'closed') = (closed_at IS NOT NULL))
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

CREATE INDEX idx_capacity_slots_profile_state
ON capacity_slots(provider_id, profile_name, state, created_at);

CREATE INDEX idx_web_sessions_expires_at
ON web_sessions(expires_at);
