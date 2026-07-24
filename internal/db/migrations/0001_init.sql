CREATE TABLE app_metadata (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO app_metadata (key, value, updated_at)
VALUES ('schema_version', '0001_init', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

INSERT INTO app_metadata (key, value, updated_at)
VALUES ('storage_format', '4', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE id_allocators (
	name TEXT PRIMARY KEY,
	next_number INTEGER NOT NULL CHECK (next_number > 0)
);

INSERT INTO id_allocators (name, next_number)
VALUES ('task', 1), ('task_attachment', 1);

CREATE TABLE agent_defs (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
	harness TEXT NOT NULL CHECK (harness IN ('codex', 'claude', 'harness')),
	model TEXT NOT NULL DEFAULT '',
	reasoning_effort TEXT NOT NULL DEFAULT '',
	prompt TEXT NOT NULL DEFAULT '',
	builtin INTEGER NOT NULL DEFAULT 0 CHECK (builtin IN (0, 1)),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE flows (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
	description TEXT NOT NULL DEFAULT '',
	start_node_key TEXT NOT NULL DEFAULT '',
	transition_budget INTEGER NOT NULL DEFAULT 50 CHECK (transition_budget BETWEEN 1 AND 500),
	builtin INTEGER NOT NULL DEFAULT 0 CHECK (builtin IN (0, 1)),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

-- Editable workflow graph. Topology is relational while each trusted node
-- handler owns a strict, versioned JSON configuration contract.
CREATE TABLE flow_nodes (
	id TEXT PRIMARY KEY,
	flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
	node_key TEXT NOT NULL CHECK (length(trim(node_key)) > 0),
	name TEXT NOT NULL CHECK (length(trim(name)) > 0),
	kind TEXT NOT NULL CHECK (kind IN (
		'agent', 'automated_checks', 'change_review', 'human_gate',
		'verify_change', 'materialize_task_set', 'merge_change', 'terminal'
	)),
	position INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
	config_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(config_json)),
	UNIQUE (flow_id, node_key),
	UNIQUE (flow_id, position)
);

CREATE TABLE flow_edges (
	flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
	from_node_key TEXT NOT NULL,
	outcome TEXT NOT NULL CHECK (length(trim(outcome)) > 0),
	to_node_key TEXT NOT NULL,
	PRIMARY KEY (flow_id, from_node_key, outcome),
	FOREIGN KEY (flow_id, from_node_key) REFERENCES flow_nodes(flow_id, node_key) ON DELETE CASCADE,
	FOREIGN KEY (flow_id, to_node_key) REFERENCES flow_nodes(flow_id, node_key) ON DELETE CASCADE
);

CREATE TABLE tasks (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL CHECK (length(trim(title)) > 0),
	body TEXT NOT NULL DEFAULT '',
	priority INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_by_session_id TEXT,
	source_task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	source_change_id TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	flow_id TEXT REFERENCES flows(id) ON DELETE RESTRICT,
	lifecycle_state TEXT CHECK (lifecycle_state IN ('scheduled', 'in_progress', 'done')),
	done_resolution TEXT CHECK (done_resolution IN ('completed', 'merged', 'rejected', 'abandoned', 'cancelled', 'failed')),
	done_at TEXT,
	CHECK ((lifecycle_state = 'done') = (done_resolution IS NOT NULL AND done_at IS NOT NULL)),
	CHECK (lifecycle_state = 'done' OR (done_resolution IS NULL AND done_at IS NULL))
);

CREATE TABLE workflow_runs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	run_sequence INTEGER NOT NULL CHECK (run_sequence > 0),
	flow_id TEXT,
	flow_snapshot_json TEXT NOT NULL CHECK (json_valid(flow_snapshot_json)),
	state TEXT NOT NULL CHECK (state IN ('scheduled', 'running', 'waiting', 'completed', 'cancelled')),
	current_node_key TEXT NOT NULL DEFAULT '',
	current_node_run_id TEXT,
	current_artifact_id TEXT,
	transition_budget INTEGER NOT NULL CHECK (transition_budget BETWEEN 1 AND 500),
	transitions_used INTEGER NOT NULL DEFAULT 0 CHECK (transitions_used >= 0),
	version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
	created_at TEXT NOT NULL,
	started_at TEXT,
	completed_at TEXT,
	cancelled_at TEXT,
	completion_source TEXT NOT NULL DEFAULT '',
	UNIQUE (task_id, run_sequence)
);

CREATE UNIQUE INDEX idx_workflow_runs_one_active
	ON workflow_runs(task_id)
	WHERE state IN ('scheduled', 'running', 'waiting');

CREATE TABLE workflow_node_runs (
	id TEXT PRIMARY KEY,
	workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
	node_key TEXT NOT NULL,
	visit INTEGER NOT NULL CHECK (visit > 0),
	attempt INTEGER NOT NULL CHECK (attempt > 0),
	state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'waiting', 'succeeded', 'failed', 'cancelled')),
	input_artifact_id TEXT,
	output_artifact_id TEXT,
	outcome TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	started_at TEXT,
	completed_at TEXT,
	UNIQUE (workflow_run_id, node_key, visit, attempt)
);

CREATE UNIQUE INDEX idx_workflow_node_runs_one_active
	ON workflow_node_runs(workflow_run_id)
	WHERE state IN ('queued', 'running', 'waiting');

CREATE TABLE workflow_artifacts (
	id TEXT PRIMARY KEY,
	workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
	node_run_id TEXT NOT NULL REFERENCES workflow_node_runs(id) ON DELETE RESTRICT,
	session_id TEXT,
	creator_key TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('handoff', 'change', 'task_set')),
	summary_markdown TEXT NOT NULL,
	payload_json TEXT CHECK (payload_json IS NULL OR json_valid(payload_json)),
	payload_sha256 TEXT NOT NULL,
	base_revision TEXT,
	client_key TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (creator_key, client_key)
);

CREATE TABLE workflow_materializations (
	artifact_id TEXT PRIMARY KEY REFERENCES workflow_artifacts(id) ON DELETE RESTRICT,
	workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE RESTRICT,
	result_json TEXT NOT NULL CHECK (json_valid(result_json)),
	created_at TEXT NOT NULL
);

CREATE TABLE workflow_waits (
	id TEXT PRIMARY KEY,
	workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
	node_run_id TEXT REFERENCES workflow_node_runs(id) ON DELETE SET NULL,
	kind TEXT NOT NULL CHECK (kind IN ('human_gate', 'agent_request', 'operator_intervention')),
	reason TEXT NOT NULL DEFAULT '',
	details_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(details_json)),
	message TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL CHECK (state IN ('open', 'resolved')),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	resolved_by TEXT,
	resolved_at TEXT
);

CREATE UNIQUE INDEX idx_workflow_waits_one_open
	ON workflow_waits(workflow_run_id) WHERE state = 'open';

CREATE TABLE workflow_transitions (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE CASCADE,
	from_task_state TEXT NOT NULL DEFAULT '',
	to_task_state TEXT NOT NULL DEFAULT '',
	from_node_key TEXT NOT NULL DEFAULT '',
	to_node_key TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL DEFAULT '',
	event_kind TEXT NOT NULL CHECK (length(trim(event_kind)) > 0),
	payload_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload_json)),
	actor TEXT NOT NULL DEFAULT '',
	idempotency_key TEXT,
	created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_workflow_transitions_idempotency
	ON workflow_transitions(workflow_run_id, idempotency_key)
	WHERE idempotency_key IS NOT NULL;

CREATE TABLE task_relations (
	source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	target_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('parent_of', 'blocks', 'related_to')),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	PRIMARY KEY (source_task_id, target_task_id, kind),
	CHECK (source_task_id != target_task_id)
);

CREATE TABLE tags (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	slug TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	color TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL
);

CREATE TABLE task_tags (
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	PRIMARY KEY (task_id, tag_id)
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

CREATE TABLE git_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	event_hash TEXT NOT NULL UNIQUE,
	old_sha TEXT NOT NULL,
	new_sha TEXT NOT NULL,
	ref TEXT NOT NULL,
	actor TEXT NOT NULL,
	observed_at TEXT NOT NULL,
	received_at TEXT NOT NULL,
	source TEXT NOT NULL CHECK (source IN ('api', 'spool'))
);

CREATE TABLE jobs (
	id TEXT PRIMARY KEY,
	task_id TEXT REFERENCES tasks(id) ON DELETE CASCADE,
	change_id TEXT REFERENCES changes(id) ON DELETE SET NULL,
	workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE CASCADE,
	node_run_id TEXT REFERENCES workflow_node_runs(id) ON DELETE CASCADE,
	role TEXT NOT NULL CHECK (role IN ('author', 'reviewer', 'verifier', 'ci', 'console')),
	state TEXT NOT NULL CHECK (state IN ('queued', 'claimed', 'running', 'finished', 'failed', 'crashed', 'canceled')),
	capacity_bucket TEXT NOT NULL CHECK (capacity_bucket IN ('persistent_agent', 'ephemeral')),
	priority INTEGER NOT NULL DEFAULT 0,
	selector_json TEXT NOT NULL DEFAULT '{}',
	tolerations_json TEXT NOT NULL DEFAULT '[]',
	payload_json TEXT NOT NULL DEFAULT '{}',
	transcript_path TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK (role != 'author' OR task_id IS NOT NULL)
);

CREATE TABLE leases (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	worker_id TEXT NOT NULL,
	capacity_bucket TEXT NOT NULL CHECK (capacity_bucket IN ('persistent_agent', 'ephemeral')),
	leased_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	released_at TEXT,
	renewal_count INTEGER NOT NULL DEFAULT 0 CHECK (renewal_count >= 0)
);

CREATE TABLE checks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('ci', 'reviewer', 'verifier', 'human')),
	required INTEGER NOT NULL DEFAULT 1 CHECK (required IN (0, 1)),
	verdict TEXT NOT NULL CHECK (verdict IN ('pending', 'satisfied', 'blocked', 'skipped', 'errored')),
	exit_code INTEGER,
	details TEXT NOT NULL DEFAULT '',
	source_job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
	reporter TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (task_id, name),
	CHECK (length(trim(name)) > 0)
);

CREATE TABLE changes (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE CASCADE,
	branch TEXT NOT NULL,
	base TEXT NOT NULL DEFAULT 'main',
	head_sha TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	ready_at TEXT,
	merged_at TEXT,
	UNIQUE (task_id, branch),
	CHECK (length(trim(branch)) > 0),
	CHECK (length(trim(base)) > 0)
);

CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	task_id TEXT REFERENCES tasks(id) ON DELETE CASCADE,
	change_id TEXT REFERENCES changes(id) ON DELETE CASCADE,
	workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE CASCADE,
	node_run_id TEXT REFERENCES workflow_node_runs(id) ON DELETE CASCADE,
	job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	lease_id TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
	worker_id TEXT NOT NULL,
	role TEXT NOT NULL CHECK (role IN ('author', 'reviewer', 'verifier', 'console')),
	workspace_mode TEXT NOT NULL DEFAULT 'change' CHECK (workspace_mode IN ('base', 'change')),
	runtime_state TEXT NOT NULL CHECK (runtime_state IN ('starting', 'working', 'waiting', 'finished', 'crashed', 'abandoned')),
	branch TEXT NOT NULL,
	base TEXT NOT NULL DEFAULT 'main',
	harness TEXT NOT NULL DEFAULT '',
	transcript_path TEXT NOT NULL DEFAULT '',
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	finished_at TEXT,
	last_agent_activity_at TEXT,
	CHECK (length(trim(branch)) > 0),
	CHECK (length(trim(base)) > 0)
);

CREATE TABLE status_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	change_id TEXT REFERENCES changes(id) ON DELETE SET NULL,
	session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL,
	actor TEXT NOT NULL,
	message TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'note' CHECK (kind IN ('note', 'progress', 'plan', 'blocker', 'question')),
	created_at TEXT NOT NULL,
	resolved_at TEXT,
	CHECK (length(trim(message)) > 0)
);

CREATE TABLE session_messages (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	status_log_id INTEGER REFERENCES status_log(id) ON DELETE SET NULL,
	actor TEXT NOT NULL,
	body TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('pending', 'delivered')),
	created_at TEXT NOT NULL,
	delivered_at TEXT,
	delivery_error TEXT NOT NULL DEFAULT '',
	CHECK (length(trim(body)) > 0)
);

CREATE TABLE handoff_snapshots (
	change_id TEXT PRIMARY KEY REFERENCES changes(id) ON DELETE CASCADE,
	head_sha TEXT NOT NULL,
	present INTEGER NOT NULL CHECK (present IN (0, 1)),
	valid INTEGER NOT NULL CHECK (valid IN (0, 1)),
	summary TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);

CREATE TABLE handoff_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	change_id TEXT NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
	head_sha TEXT NOT NULL,
	present INTEGER NOT NULL CHECK (present IN (0, 1)),
	valid INTEGER NOT NULL CHECK (valid IN (0, 1)),
	summary TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	recorded_at TEXT NOT NULL
);

CREATE TABLE session_terminals (
	session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	target_url TEXT NOT NULL,
	tmux_socket_path TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK (length(trim(target_url)) > 0)
);

CREATE TABLE terminal_access_tokens (
	token_hash TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE review_threads (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	change_id TEXT NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
	state TEXT NOT NULL CHECK (state IN ('open', 'claimed', 'certified', 'reopened')),
	anchor_commit_sha TEXT NOT NULL,
	file_path TEXT NOT NULL,
	line INTEGER NOT NULL CHECK (line > 0),
	context TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL,
	claim_kind TEXT CHECK (claim_kind IN ('fixed', 'not_warranted', 'superseded')),
	claim_commit_sha TEXT,
	claimed_by TEXT,
	claimed_at TEXT,
	certified_by TEXT,
	certified_at TEXT,
	reopened_by TEXT,
	reopened_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	-- Opening-comment digest used to deduplicate retried review concerns.
	body_hash TEXT NOT NULL DEFAULT '',
	CHECK (length(trim(anchor_commit_sha)) > 0),
	CHECK (length(trim(file_path)) > 0),
	CHECK (claim_kind IS NOT NULL OR claim_commit_sha IS NULL)
);

CREATE TABLE review_comments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	thread_id TEXT NOT NULL REFERENCES review_threads(id) ON DELETE CASCADE,
	actor TEXT NOT NULL,
	body TEXT NOT NULL,
	created_at TEXT NOT NULL,
	CHECK (length(trim(body)) > 0)
);

CREATE TABLE job_terminals (
	job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
	lease_id TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
	target_url TEXT NOT NULL,
	tmux_socket_path TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK (length(trim(target_url)) > 0)
);

CREATE TABLE job_terminal_access_tokens (
	token_hash TEXT PRIMARY KEY,
	job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE merge_intents (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	change_id TEXT NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
	base_branch TEXT NOT NULL,
	exchange_path TEXT NOT NULL,
	head_sha TEXT NOT NULL,
	previous_base_sha TEXT NOT NULL,
	created_at TEXT NOT NULL,
	completed_at TEXT
);

CREATE TABLE consumer_watermarks (
	name TEXT PRIMARY KEY,
	last_seen_id INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);

CREATE TABLE task_attachments (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	stage TEXT NOT NULL CHECK (stage IN ('initial', 'author', 'reviewer', 'verifier')),
	filename TEXT NOT NULL CHECK (length(trim(filename)) > 0),
	content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	storage_key TEXT NOT NULL UNIQUE CHECK (length(trim(storage_key)) > 0),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL
);

CREATE VIEW task_review_state AS
SELECT
	i.id AS task_id,
	CASE
		WHEN EXISTS (SELECT 1 FROM changes ch WHERE ch.task_id = i.id AND ch.merged_at IS NOT NULL) THEN 'merged'
		WHEN EXISTS (SELECT 1 FROM checks c WHERE c.task_id = i.id AND c.required = 1 AND c.verdict = 'blocked') THEN 'changes_requested'
		WHEN EXISTS (SELECT 1 FROM checks c WHERE c.task_id = i.id AND c.required = 1)
			AND NOT EXISTS (SELECT 1 FROM checks c WHERE c.task_id = i.id AND c.required = 1 AND c.verdict != 'satisfied') THEN 'approved'
		ELSE 'in_review'
	END AS review_state
FROM tasks i;

CREATE INDEX idx_tasks_flow_id ON tasks(flow_id);
CREATE INDEX idx_tasks_lifecycle_state ON tasks(lifecycle_state, updated_at);
CREATE INDEX idx_tasks_done_at ON tasks(done_at DESC, id DESC) WHERE lifecycle_state = 'done';
CREATE INDEX idx_tasks_source_task_id ON tasks(source_task_id);
CREATE INDEX idx_task_relations_target ON task_relations(target_task_id, kind);
CREATE UNIQUE INDEX idx_task_relations_one_parent ON task_relations(target_task_id) WHERE kind = 'parent_of';
CREATE INDEX idx_task_tags_tag_id ON task_tags(tag_id);
CREATE INDEX idx_git_events_ref ON git_events(ref, observed_at);
CREATE INDEX idx_jobs_queue ON jobs(state, capacity_bucket, priority DESC, created_at);
CREATE UNIQUE INDEX idx_jobs_one_live_author_per_task ON jobs(task_id) WHERE role = 'author' AND state IN ('queued', 'claimed', 'running') AND task_id IS NOT NULL;
-- Console work is unique per project when task_id is NULL and per task otherwise.
CREATE UNIQUE INDEX idx_jobs_one_live_project_console ON jobs(role)
	WHERE role = 'console' AND task_id IS NULL AND state IN ('queued', 'claimed', 'running');
CREATE UNIQUE INDEX idx_jobs_one_live_task_console ON jobs(task_id)
	WHERE role = 'console' AND task_id IS NOT NULL AND state IN ('queued', 'claimed', 'running');
CREATE INDEX idx_leases_worker_live ON leases(worker_id, capacity_bucket) WHERE released_at IS NULL;
CREATE INDEX idx_leases_expired ON leases(expires_at) WHERE released_at IS NULL;
CREATE UNIQUE INDEX idx_leases_one_live_per_job ON leases(job_id) WHERE released_at IS NULL;
CREATE INDEX idx_checks_task_verdict ON checks(task_id, required, verdict);
CREATE INDEX idx_changes_task_unmerged ON changes(task_id, merged_at);
CREATE INDEX idx_changes_task_ready ON changes(task_id, ready_at, merged_at);
CREATE INDEX idx_changes_task_merged ON changes(task_id) WHERE merged_at IS NOT NULL;
CREATE INDEX idx_jobs_change_id ON jobs(change_id);
CREATE INDEX idx_jobs_workflow_node ON jobs(workflow_run_id, node_run_id);
CREATE INDEX idx_changes_workflow_run ON changes(workflow_run_id);
CREATE INDEX idx_sessions_workflow_node ON sessions(workflow_run_id, node_run_id);
CREATE INDEX idx_workflow_artifacts_run ON workflow_artifacts(workflow_run_id, created_at);
CREATE INDEX idx_workflow_transitions_task_seq ON workflow_transitions(task_id, seq DESC);
CREATE UNIQUE INDEX idx_sessions_one_active_author_per_task ON sessions(task_id) WHERE role = 'author' AND runtime_state IN ('starting', 'working', 'waiting');
CREATE UNIQUE INDEX idx_sessions_one_active_project_console ON sessions(role)
	WHERE role = 'console' AND task_id IS NULL AND runtime_state IN ('starting', 'working', 'waiting');
CREATE UNIQUE INDEX idx_sessions_one_active_task_console ON sessions(task_id)
	WHERE role = 'console' AND task_id IS NOT NULL AND runtime_state IN ('starting', 'working', 'waiting');
CREATE INDEX idx_sessions_change ON sessions(change_id, created_at);
CREATE INDEX idx_session_messages_pending ON session_messages(session_id, state, created_at);
CREATE INDEX idx_status_log_task_created ON status_log(task_id, created_at DESC, id DESC);
CREATE INDEX idx_status_log_task_kind_resolved
	ON status_log(task_id, kind, resolved_at, created_at DESC, id DESC);
CREATE INDEX idx_handoff_history_change_recorded ON handoff_history(change_id, recorded_at DESC, id DESC);
CREATE INDEX idx_terminal_access_tokens_session ON terminal_access_tokens(session_id, expires_at);
CREATE INDEX idx_review_threads_change_state ON review_threads(change_id, state, created_at);
CREATE INDEX idx_review_threads_task_state ON review_threads(task_id, state, created_at);
-- Idempotency guard for worker-applied reviewer concerns.
CREATE UNIQUE INDEX idx_review_threads_idem
	ON review_threads(change_id, anchor_commit_sha, file_path, line, body_hash)
	WHERE body_hash != '';
CREATE INDEX idx_review_comments_thread_created ON review_comments(thread_id, created_at, id);
CREATE INDEX idx_job_terminals_lease ON job_terminals(lease_id);
CREATE INDEX idx_job_terminal_access_tokens_job ON job_terminal_access_tokens(job_id, expires_at);
CREATE UNIQUE INDEX idx_merge_intents_change_open ON merge_intents(change_id) WHERE completed_at IS NULL;
CREATE INDEX idx_idempotency_pending ON idempotency_records(status_code, created_at);
CREATE INDEX idx_task_attachments_task_id ON task_attachments(task_id, created_at);
CREATE INDEX idx_task_attachments_stage ON task_attachments(stage);
