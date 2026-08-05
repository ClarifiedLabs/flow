-- Project database launch schema.
-- Runtime invariants are enforced by constraints and triggers below.

-- Metadata and ID allocation.

CREATE TABLE app_metadata (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO app_metadata (key, value, updated_at)
VALUES
	('schema_version', '0001_init', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	('storage_format', '7', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE id_allocators (
	name TEXT PRIMARY KEY,
	next_number INTEGER NOT NULL CHECK (next_number > 0)
);

INSERT INTO id_allocators (name, next_number)
VALUES
	('task', 1),
	('epic', 1),
	('task_attachment', 1),
	('feature', 1),
	('feature_rebase', 1);

-- Workflow definitions.

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

CREATE TABLE flow_nodes (
	id TEXT PRIMARY KEY,
	flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
	node_key TEXT NOT NULL CHECK (length(trim(node_key)) > 0),
	name TEXT NOT NULL CHECK (length(trim(name)) > 0),
	kind TEXT NOT NULL CHECK (kind IN (
		'agent', 'automated_checks', 'change_review', 'human_gate',
		'verify_change', 'materialize_task_set', 'merge_change', 'finalize_rebase', 'terminal'
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

-- Work items and tasks.

CREATE TABLE work_items (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL CHECK (kind IN ('task', 'epic', 'feature')),
	created_at TEXT NOT NULL
);

CREATE TABLE tasks (
	id TEXT PRIMARY KEY REFERENCES work_items(id) ON DELETE CASCADE,
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
	feature_id TEXT REFERENCES features(id) ON DELETE SET NULL,
	CHECK ((lifecycle_state = 'done') = (done_resolution IS NOT NULL AND done_at IS NOT NULL)),
	CHECK (lifecycle_state = 'done' OR (done_resolution IS NULL AND done_at IS NULL))
);

CREATE TABLE epics (
	id TEXT PRIMARY KEY REFERENCES work_items(id) ON DELETE CASCADE,
	title TEXT NOT NULL CHECK (length(trim(title)) > 0),
	body TEXT NOT NULL DEFAULT '',
	priority INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0),
	status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'completed', 'archived')),
	completion_policy TEXT NOT NULL DEFAULT 'all_children' CHECK (completion_policy IN ('all_children', 'manual')),
	completed_automatically INTEGER NOT NULL DEFAULT 0 CHECK (completed_automatically IN (0, 1)),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	completed_at TEXT,
	archived_at TEXT,
	CHECK (status != 'completed' OR completed_at IS NOT NULL),
	CHECK (status != 'archived' OR archived_at IS NOT NULL)
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

CREATE TABLE work_item_relations (
	source_item_id TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
	target_item_id TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('parent_of', 'blocks', 'related_to')),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	PRIMARY KEY (source_item_id, target_item_id, kind),
	CHECK (source_item_id != target_item_id),
	CHECK (kind != 'related_to' OR source_item_id < target_item_id)
);

-- Workflow runtime.

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
	review_cycle_budget INTEGER NOT NULL DEFAULT 2 CHECK (review_cycle_budget BETWEEN 1 AND 500),
	review_cycles_used INTEGER NOT NULL DEFAULT 0 CHECK (review_cycles_used >= 0),
	version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
	created_at TEXT NOT NULL,
	started_at TEXT,
	completed_at TEXT,
	cancelled_at TEXT,
	completion_source TEXT NOT NULL DEFAULT '',
	held_at TEXT,
	held_by TEXT NOT NULL DEFAULT '',
	UNIQUE (task_id, run_sequence)
);

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
	state TEXT NOT NULL DEFAULT 'prepared' CHECK (state IN ('prepared', 'completed')),
	result_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(result_json)),
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
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

-- Jobs, sessions, and worker execution.

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
	required_harness TEXT NOT NULL DEFAULT '',
	required_model TEXT NOT NULL DEFAULT '',
	tolerations_json TEXT NOT NULL DEFAULT '[]',
	payload_json TEXT NOT NULL DEFAULT '{}',
	transcript_path TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	dispatch_key TEXT NOT NULL DEFAULT '',
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
	tmux_socket_path TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE terminal_access_tokens (
	token_hash TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE job_terminals (
	job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
	lease_id TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
	tmux_socket_path TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE job_terminal_access_tokens (
	token_hash TEXT PRIMARY KEY,
	job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE worker_assignments (
	id TEXT PRIMARY KEY,
	capacity_slot_id TEXT NOT NULL CHECK (length(trim(capacity_slot_id)) > 0),
	worker_id TEXT NOT NULL UNIQUE CHECK (length(trim(worker_id)) > 0),
	job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
	provider_id TEXT NOT NULL CHECK (length(trim(provider_id)) > 0),
	profile_name TEXT NOT NULL CHECK (length(trim(profile_name)) > 0),
	provider_request_id TEXT NOT NULL CHECK (length(trim(provider_request_id)) > 0),
	provider_type TEXT NOT NULL CHECK (length(trim(provider_type)) > 0),
	provider_options_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(provider_options_json)),
	state TEXT NOT NULL CHECK (state IN ('pending', 'claimed', 'closed')),
	role TEXT NOT NULL CHECK (role IN ('author', 'reviewer', 'verifier', 'ci', 'console')),
	capacity_bucket TEXT NOT NULL CHECK (capacity_bucket IN ('persistent_agent', 'ephemeral')),
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
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	closed_at TEXT,
	close_reason TEXT,
	last_provider_error TEXT,
	claimed_lease_id TEXT REFERENCES leases(id) ON DELETE RESTRICT,
	credentials_revoked_at TEXT,
	cleaned_at TEXT,
	UNIQUE (provider_id, provider_request_id),
	UNIQUE (capacity_slot_id),
	CHECK ((state = 'claimed') = (claimed_lease_id IS NOT NULL) OR state = 'closed'),
	CHECK ((state = 'closed') = (closed_at IS NOT NULL))
);

-- Reviews and merges.

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

CREATE TABLE review_follow_up_actions (
	source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	check_name TEXT NOT NULL,
	finding_hash TEXT NOT NULL,
	request_hash TEXT NOT NULL,
	action TEXT NOT NULL CHECK (action IN ('create_task', 'use_existing_task')),
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	PRIMARY KEY (source_task_id, check_name, finding_hash),
	CHECK (length(trim(check_name)) > 0),
	CHECK (length(trim(finding_hash)) > 0),
	CHECK (length(trim(request_hash)) > 0),
	CHECK (source_task_id != task_id)
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

-- Features and convergence.

CREATE TABLE features (
	id TEXT PRIMARY KEY REFERENCES work_items(id) ON DELETE CASCADE,
	title TEXT NOT NULL CHECK (length(trim(title)) > 0),
	-- title_norm keeps feature titles unique per project, case-insensitively.
	title_norm TEXT GENERATED ALWAYS AS (lower(trim(title))) VIRTUAL UNIQUE,
	body TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'landed', 'archived')),
	integration_feature_id TEXT REFERENCES features(id) ON DELETE RESTRICT,
	created_from_sha TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	landed_at TEXT,
	land_sha TEXT NOT NULL DEFAULT '',
	land_target_feature_id TEXT REFERENCES features(id) ON DELETE RESTRICT,
	land_target_branch TEXT NOT NULL DEFAULT '',
	land_target_sha TEXT NOT NULL DEFAULT ''
);

CREATE TABLE feature_creation_intents (
	id TEXT PRIMARY KEY,
	operation_key TEXT NOT NULL UNIQUE,
	parent_item_id TEXT REFERENCES work_items(id) ON DELETE RESTRICT,
	integration_feature_id TEXT REFERENCES features(id) ON DELETE RESTRICT,
	title TEXT NOT NULL CHECK (length(trim(title)) > 0),
	body TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL UNIQUE,
	target_branch TEXT NOT NULL,
	target_sha TEXT NOT NULL,
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	state TEXT NOT NULL DEFAULT 'prepared' CHECK (state IN ('prepared', 'ref_created', 'completed')),
	relation_payload_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(relation_payload_json)),
	ref_created_by_intent BOOLEAN NOT NULL DEFAULT FALSE CHECK (ref_created_by_intent IN (FALSE, TRUE)),
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE feature_rebases (
	id TEXT PRIMARY KEY,
	feature_id TEXT NOT NULL REFERENCES features(id) ON DELETE CASCADE,
	task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	restrict_blocked_to TEXT NOT NULL DEFAULT '',
	old_tip_sha TEXT NOT NULL,
	target_base TEXT NOT NULL,
	target_base_sha TEXT NOT NULL,
	target_feature_id TEXT REFERENCES features(id) ON DELETE RESTRICT,
	new_tip_sha TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL CHECK (state IN ('running', 'finalized', 'stale', 'failed', 'cancelled')),
	created_at TEXT NOT NULL,
	completed_at TEXT
);

CREATE TABLE convergence_promotions (
    source_task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE RESTRICT,
    workflow_run_id TEXT NOT NULL UNIQUE REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    evidence_fingerprint TEXT NOT NULL UNIQUE,
    evidence_json TEXT NOT NULL CHECK (json_valid(evidence_json)),
    feature_id TEXT NOT NULL UNIQUE REFERENCES features(id) ON DELETE RESTRICT,
    planning_task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
    planning_workflow_run_id TEXT UNIQUE REFERENCES workflow_runs(id) ON DELETE RESTRICT,
    feature_title TEXT NOT NULL CHECK (length(trim(feature_title)) > 0),
    feature_body TEXT NOT NULL DEFAULT '',
    planning_flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE RESTRICT,
    planning_title TEXT NOT NULL CHECK (length(trim(planning_title)) > 0),
    planning_body TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL CHECK (actor IN ('human', 'agent', 'system')),
    note TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'prepared' CHECK (state IN ('prepared', 'materialized', 'completed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT
);

-- Durable history capture.

CREATE TABLE history_captures (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'hc-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    project_id TEXT NOT NULL CHECK (length(trim(project_id)) BETWEEN 1 AND 255),
    job_id TEXT NOT NULL CHECK (length(trim(job_id)) BETWEEN 1 AND 255),
    lease_id TEXT NOT NULL CHECK (length(trim(lease_id)) BETWEEN 1 AND 255),
    lease_attempt INTEGER NOT NULL CHECK (lease_attempt > 0),
    worker_id TEXT NOT NULL CHECK (length(trim(worker_id)) BETWEEN 1 AND 255),
    task_id TEXT NOT NULL DEFAULT '' CHECK (length(task_id) <= 255),
    session_id TEXT NOT NULL DEFAULT '' CHECK (length(session_id) <= 255),
    workflow_run_id TEXT NOT NULL DEFAULT '' CHECK (length(workflow_run_id) <= 255),
    node_run_id TEXT NOT NULL DEFAULT '' CHECK (length(node_run_id) <= 255),
    node_visit INTEGER CHECK (node_visit IS NULL OR node_visit > 0),
    stage TEXT NOT NULL DEFAULT '' CHECK (length(stage) <= 128),
    role TEXT NOT NULL CHECK (length(trim(role)) BETWEEN 1 AND 64),
    harness_name TEXT NOT NULL DEFAULT '' CHECK (length(harness_name) <= 128),
    harness_version TEXT NOT NULL DEFAULT '' CHECK (length(harness_version) <= 128),
    expected_transcript INTEGER NOT NULL CHECK (expected_transcript IN (0, 1)),
    expected_harness INTEGER NOT NULL CHECK (expected_harness IN (0, 1)),

    state TEXT NOT NULL DEFAULT 'reserved' CHECK (state IN (
        'reserved', 'running', 'quiescing', 'sealed', 'uploading',
        'complete', 'blocked', 'lost', 'waived'
    )),
    execution_verdict TEXT NOT NULL DEFAULT 'pending' CHECK (execution_verdict IN (
        'pending', 'succeeded', 'failed', 'cancelled', 'crashed'
    )),
    execution_exit_code INTEGER,
    execution_error_code TEXT NOT NULL DEFAULT '' CHECK (length(execution_error_code) <= 64),
    execution_recorded_at TEXT,

    upload_grant_hash TEXT NOT NULL UNIQUE
        CHECK (length(upload_grant_hash) = 64
            AND upload_grant_hash NOT GLOB '*[^0-9a-f]*'),
    upload_grant_generation INTEGER NOT NULL DEFAULT 1 CHECK (upload_grant_generation >= 1),
    upload_grant_rotated_at TEXT,
    upload_grant_revoked_at TEXT,

    expected_set_declared_at TEXT,
    expected_final_artifact_count INTEGER CHECK (
        expected_final_artifact_count IS NULL OR expected_final_artifact_count >= 0
    ),
    expected_transcript_epoch INTEGER CHECK (
        expected_transcript_epoch IS NULL OR expected_transcript_epoch >= 0
    ),
    expected_transcript_segment_count INTEGER CHECK (
        expected_transcript_segment_count IS NULL OR expected_transcript_segment_count >= 0
    ),
    expected_transcript_length INTEGER CHECK (
        expected_transcript_length IS NULL OR expected_transcript_length >= 0
    ),
    expected_transcript_sha256 TEXT CHECK (
        expected_transcript_sha256 IS NULL OR (
            length(expected_transcript_sha256) = 64
            AND expected_transcript_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),

    last_hint_at TEXT,
    checkpoint_hint_count INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint_hint_count >= 0),
    checkpoint_coalesced_count INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint_coalesced_count >= 0),
    checkpoint_dirty_generation INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint_dirty_generation >= 0),
    last_checkpoint_attempt_generation INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_attempt_generation >= 0),
    last_checkpoint_committed_generation INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_committed_generation >= 0),
    last_checkpoint_bytes INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_bytes >= 0),
    last_checkpoint_entries INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_entries >= 0),

    zero_harness_root_reason TEXT NOT NULL DEFAULT '' CHECK (length(zero_harness_root_reason) <= 1024),
    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    error_message TEXT NOT NULL DEFAULT '' CHECK (length(error_message) <= 1024),
    waiver_reason TEXT NOT NULL DEFAULT '' CHECK (length(waiver_reason) <= 1024),
    version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
    reserved_at TEXT NOT NULL,
    running_at TEXT,
    quiescing_at TEXT,
    sealed_at TEXT,
    uploading_at TEXT,
    completed_at TEXT,
    blocked_at TEXT,
    lost_at TEXT,
    waived_at TEXT,
    updated_at TEXT NOT NULL,

    UNIQUE (job_id, lease_id),
    UNIQUE (job_id, lease_attempt),
    CHECK ((execution_verdict = 'pending') = (execution_recorded_at IS NULL)),
    CHECK (execution_verdict != 'succeeded' OR execution_exit_code IS NULL OR execution_exit_code = 0),
    CHECK ((expected_set_declared_at IS NULL) = (expected_final_artifact_count IS NULL)),
    CHECK (expected_set_declared_at IS NOT NULL OR (
        expected_transcript_epoch IS NULL
        AND expected_transcript_segment_count IS NULL
        AND expected_transcript_length IS NULL
        AND expected_transcript_sha256 IS NULL
    )),
    CHECK (state != 'running' OR running_at IS NOT NULL),
    CHECK (state != 'quiescing' OR quiescing_at IS NOT NULL),
    CHECK (state != 'sealed' OR sealed_at IS NOT NULL),
    CHECK (state != 'uploading' OR uploading_at IS NOT NULL),
    CHECK (state != 'complete' OR completed_at IS NOT NULL),
    CHECK (state != 'blocked' OR blocked_at IS NOT NULL),
    CHECK (state != 'lost' OR lost_at IS NOT NULL),
    CHECK (state != 'waived' OR (waived_at IS NOT NULL AND length(trim(waiver_reason)) > 0)),
    CHECK (state NOT IN ('complete', 'waived') OR upload_grant_revoked_at IS NOT NULL)
);

CREATE TABLE history_capture_expected_artifacts (
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    logical_key TEXT NOT NULL CHECK (length(trim(logical_key)) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('harness_root', 'manifest')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (capture_id, logical_key)
);

CREATE TABLE history_artifacts (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'ha-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    logical_key TEXT NOT NULL CHECK (length(trim(logical_key)) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('transcript_segment', 'harness_root', 'manifest')),
    phase TEXT NOT NULL CHECK (phase IN ('checkpoint', 'final')),
    checkpoint_generation INTEGER,
    checkpoint_stream TEXT NOT NULL DEFAULT '' CHECK (length(checkpoint_stream) <= 255),
    checkpoint_trigger TEXT NOT NULL DEFAULT '' CHECK (length(checkpoint_trigger) <= 64),
    archive_id TEXT NOT NULL DEFAULT '' CHECK (length(archive_id) <= 255),
    media_type TEXT NOT NULL CHECK (length(trim(media_type)) BETWEEN 1 AND 255),
    format_version INTEGER NOT NULL DEFAULT 1 CHECK (format_version > 0),
    schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    logical_size INTEGER NOT NULL CHECK (logical_size >= 0),
    entry_count INTEGER NOT NULL DEFAULT 0 CHECK (entry_count >= 0),
    publication_state TEXT NOT NULL CHECK (publication_state IN ('pending', 'committed')),
    temporary_upload_id TEXT NOT NULL DEFAULT '' CHECK (length(temporary_upload_id) <= 255),
    blob_key TEXT NOT NULL UNIQUE CHECK (length(blob_key) = 65),
    superseded_by_artifact_id TEXT REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    superseded_at TEXT,
    pending_at TEXT NOT NULL,
    reconcile_attempted_at TEXT,
    committed_at TEXT,
    created_at TEXT NOT NULL,

    UNIQUE (capture_id, logical_key),
    CHECK ((phase = 'checkpoint') = (checkpoint_generation IS NOT NULL)),
    CHECK (checkpoint_generation IS NULL OR checkpoint_generation > 0),
    CHECK (phase = 'checkpoint' OR checkpoint_trigger = ''),
    CHECK (kind != 'transcript_segment' OR (phase = 'final' AND checkpoint_generation IS NULL)),
    CHECK ((publication_state = 'committed') = (committed_at IS NOT NULL)),
    CHECK (publication_state != 'pending' OR length(temporary_upload_id) > 0),
    CHECK ((superseded_by_artifact_id IS NULL) = (superseded_at IS NULL)),
    CHECK (superseded_by_artifact_id IS NULL OR superseded_by_artifact_id != id)
);

CREATE TABLE history_transcript_streams (
    capture_id TEXT PRIMARY KEY REFERENCES history_captures(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'sealed')),
    segment_count INTEGER NOT NULL DEFAULT 0 CHECK (segment_count >= 0),
    logical_length INTEGER NOT NULL DEFAULT 0 CHECK (logical_length >= 0),
    last_epoch INTEGER,
    last_sequence INTEGER,
    stream_sha256 TEXT CHECK (
        stream_sha256 IS NULL OR (length(stream_sha256) = 64 AND stream_sha256 NOT GLOB '*[^0-9a-f]*')
    ),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    sealed_at TEXT,
    CHECK ((state = 'sealed') = (sealed_at IS NOT NULL)),
    CHECK ((segment_count = 0) = (last_epoch IS NULL AND last_sequence IS NULL)),
    CHECK ((state = 'sealed') = (stream_sha256 IS NOT NULL))
);

CREATE TABLE history_transcript_segments (
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    epoch INTEGER NOT NULL CHECK (epoch >= 0),
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    start_offset INTEGER NOT NULL CHECK (start_offset >= 0),
    end_offset INTEGER NOT NULL CHECK (end_offset > start_offset),
    uncompressed_size INTEGER NOT NULL CHECK (uncompressed_size > 0),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    raw_sha256 TEXT NOT NULL DEFAULT ''
        CHECK (raw_sha256 = '' OR (length(raw_sha256) = 64 AND raw_sha256 NOT GLOB '*[^0-9a-f]*')),
    encoding TEXT NOT NULL CHECK (encoding IN ('identity', 'gzip')),
    artifact_id TEXT NOT NULL UNIQUE REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    sealed_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (capture_id, epoch, sequence),
    UNIQUE (capture_id, start_offset),
    UNIQUE (capture_id, end_offset),
    CHECK (uncompressed_size = end_offset - start_offset)
);

CREATE TABLE harness_archive_members (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'hm-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    artifact_id TEXT NOT NULL REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    archive_id TEXT NOT NULL CHECK (length(trim(archive_id)) BETWEEN 1 AND 255),
    native_session_id TEXT NOT NULL DEFAULT '' CHECK (length(native_session_id) <= 255),
    native_parent_session_id TEXT NOT NULL DEFAULT '' CHECK (length(native_parent_session_id) <= 255),
    relative_member_path TEXT NOT NULL CHECK (length(trim(relative_member_path)) BETWEEN 1 AND 4096),
    member_kind TEXT NOT NULL CHECK (member_kind IN ('root', 'delegated_child')),
    agent_name TEXT NOT NULL DEFAULT '' CHECK (length(agent_name) <= 255),
    status TEXT NOT NULL DEFAULT '' CHECK (length(status) <= 128),
    model TEXT NOT NULL DEFAULT '' CHECK (length(model) <= 255),
    harness_build TEXT NOT NULL DEFAULT '' CHECK (length(harness_build) <= 255),
    parse_status TEXT NOT NULL CHECK (parse_status IN ('parsed', 'partial', 'missing', 'invalid', 'unsupported')),
    created_at TEXT NOT NULL,
    UNIQUE (capture_id, archive_id, relative_member_path),
    UNIQUE (artifact_id, relative_member_path)
);

CREATE TABLE harness_archive_member_sets (
    artifact_id TEXT PRIMARY KEY REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    member_count INTEGER NOT NULL CHECK (member_count >= 0),
    declared_at TEXT NOT NULL
);

CREATE TABLE history_checkpoint_hints (
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    source_event TEXT NOT NULL CHECK (length(trim(source_event)) BETWEEN 1 AND 64),
    first_received_at TEXT NOT NULL,
    last_received_at TEXT NOT NULL,
    hint_count INTEGER NOT NULL CHECK (hint_count > 0),
    coalesced_count INTEGER NOT NULL DEFAULT 0 CHECK (coalesced_count >= 0 AND coalesced_count <= hint_count),
    dirty_generation INTEGER NOT NULL DEFAULT 0 CHECK (dirty_generation >= 0),
    worker_outcome TEXT NOT NULL CHECK (worker_outcome IN (
        'pending', 'coalesced', 'segment_requested', 'checkpoint_started',
        'checkpoint_committed', 'failed', 'ignored'
    )),
    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
    PRIMARY KEY (capture_id, source_event)
);

CREATE TABLE history_capture_events (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'he-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    event_kind TEXT NOT NULL CHECK (length(trim(event_kind)) BETWEEN 1 AND 64),
    from_state TEXT NOT NULL DEFAULT '' CHECK (length(from_state) <= 32),
    to_state TEXT NOT NULL DEFAULT '' CHECK (length(to_state) <= 32),
    capture_version INTEGER NOT NULL CHECK (capture_version >= 0),
    actor TEXT NOT NULL CHECK (length(trim(actor)) BETWEEN 1 AND 255),
    code TEXT NOT NULL DEFAULT '' CHECK (length(code) <= 64),
    details_json TEXT NOT NULL DEFAULT '{}' CHECK (length(details_json) <= 4096 AND json_valid(details_json)),
    occurred_at TEXT NOT NULL
);

CREATE TABLE history_upload_intents (
    temporary_upload_id TEXT PRIMARY KEY
        CHECK (length(temporary_upload_id) = 32
            AND temporary_upload_id NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'consumed', 'abandoned')),
    artifact_id TEXT UNIQUE REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    consumed_at TEXT,
    abandoned_at TEXT,
    CHECK (
        (state = 'active' AND artifact_id IS NULL AND consumed_at IS NULL AND abandoned_at IS NULL)
        OR (state = 'consumed' AND artifact_id IS NOT NULL AND consumed_at IS NOT NULL AND abandoned_at IS NULL)
        OR (state = 'abandoned' AND artifact_id IS NULL AND consumed_at IS NULL AND abandoned_at IS NOT NULL)
    )
);

CREATE TABLE history_upload_events (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'hu-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    artifact_id TEXT NOT NULL DEFAULT '' CHECK (length(artifact_id) <= 255),
    logical_key TEXT NOT NULL DEFAULT '' CHECK (length(logical_key) <= 512),
    event_kind TEXT NOT NULL CHECK (event_kind IN (
        'pending', 'publish_started', 'published', 'committed',
        'reconciled', 'failed', 'conflict'
    )),
    attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt > 0),
    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    details_json TEXT NOT NULL DEFAULT '{}' CHECK (length(details_json) <= 4096 AND json_valid(details_json)),
    occurred_at TEXT NOT NULL
);

-- Coordinator utility state.

CREATE TABLE consumer_watermarks (
	name TEXT PRIMARY KEY,
	last_seen_id INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
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

CREATE INDEX idx_changes_task_merged ON changes(task_id) WHERE merged_at IS NOT NULL;

CREATE INDEX idx_changes_task_ready ON changes(task_id, ready_at, merged_at);

CREATE INDEX idx_changes_task_unmerged ON changes(task_id, merged_at);

CREATE INDEX idx_changes_workflow_run ON changes(workflow_run_id);

CREATE INDEX idx_checks_task_verdict ON checks(task_id, required, verdict);

CREATE INDEX idx_convergence_promotions_state
    ON convergence_promotions(state, created_at);

CREATE UNIQUE INDEX idx_feature_rebases_one_running
	ON feature_rebases(feature_id) WHERE state = 'running';

CREATE INDEX idx_git_events_ref ON git_events(ref, observed_at);

CREATE INDEX idx_handoff_history_change_recorded ON handoff_history(change_id, recorded_at DESC, id DESC);

CREATE INDEX idx_harness_archive_members_native_session
    ON harness_archive_members(capture_id, native_session_id)
    WHERE native_session_id <> '';

CREATE INDEX idx_history_artifacts_capture_kind
    ON history_artifacts(capture_id, phase, kind, checkpoint_generation, logical_key);

CREATE INDEX idx_history_artifacts_capture_pending
    ON history_artifacts(capture_id, reconcile_attempted_at, pending_at, id)
    WHERE publication_state = 'pending';

CREATE UNIQUE INDEX idx_history_artifacts_checkpoint_stream_generation
    ON history_artifacts(capture_id, kind, checkpoint_stream, checkpoint_generation)
    WHERE phase = 'checkpoint';

CREATE INDEX idx_history_artifacts_pending
    ON history_artifacts(reconcile_attempted_at, pending_at, id)
    WHERE publication_state = 'pending';

CREATE INDEX idx_history_artifacts_temporary
    ON history_artifacts(temporary_upload_id) WHERE publication_state = 'pending';

CREATE INDEX idx_history_capture_events_feed
    ON history_capture_events(capture_id, occurred_at, id);

CREATE INDEX idx_history_captures_job
    ON history_captures(job_id, lease_attempt DESC);

CREATE INDEX idx_history_captures_pending
    ON history_captures(state, updated_at)
    WHERE state NOT IN ('complete', 'waived');

CREATE INDEX idx_history_captures_session
    ON history_captures(session_id, reserved_at DESC) WHERE session_id <> '';

CREATE INDEX idx_history_captures_task
    ON history_captures(task_id, reserved_at DESC, id DESC) WHERE task_id <> '';

CREATE INDEX idx_history_captures_workflow
    ON history_captures(workflow_run_id, reserved_at DESC) WHERE workflow_run_id <> '';

CREATE INDEX idx_history_transcript_segments_offsets
    ON history_transcript_segments(capture_id, start_offset, end_offset);

CREATE INDEX idx_history_upload_events_artifact
    ON history_upload_events(artifact_id, occurred_at) WHERE artifact_id <> '';

CREATE INDEX idx_history_upload_events_feed
    ON history_upload_events(capture_id, occurred_at, id);

CREATE INDEX idx_history_upload_intents_active_heartbeat
    ON history_upload_intents(heartbeat_at) WHERE state = 'active';

CREATE INDEX idx_history_upload_intents_capture_state
    ON history_upload_intents(capture_id, state, heartbeat_at);

CREATE INDEX idx_idempotency_pending ON idempotency_records(status_code, created_at);

CREATE INDEX idx_job_terminal_access_tokens_job ON job_terminal_access_tokens(job_id, expires_at);

CREATE INDEX idx_job_terminals_lease ON job_terminals(lease_id);

CREATE INDEX idx_jobs_change_id ON jobs(change_id);

CREATE UNIQUE INDEX idx_jobs_live_dispatch_key
	ON jobs(dispatch_key)
	WHERE dispatch_key <> ''
		AND state IN ('queued', 'claimed', 'running');

CREATE UNIQUE INDEX idx_jobs_one_live_author_per_task ON jobs(task_id) WHERE role = 'author' AND state IN ('queued', 'claimed', 'running') AND task_id IS NOT NULL;

CREATE UNIQUE INDEX idx_jobs_one_live_project_console ON jobs(role)
	WHERE role = 'console' AND task_id IS NULL AND state IN ('queued', 'claimed', 'running');

CREATE UNIQUE INDEX idx_jobs_one_live_task_console ON jobs(task_id)
	WHERE role = 'console' AND task_id IS NOT NULL AND state IN ('queued', 'claimed', 'running');

CREATE INDEX idx_jobs_queue ON jobs(state, capacity_bucket, priority DESC, created_at);

CREATE INDEX idx_jobs_workflow_node ON jobs(workflow_run_id, node_run_id);

CREATE INDEX idx_leases_expired ON leases(expires_at) WHERE released_at IS NULL;

CREATE UNIQUE INDEX idx_leases_one_live_per_job ON leases(job_id) WHERE released_at IS NULL;

CREATE INDEX idx_leases_worker_live ON leases(worker_id, capacity_bucket) WHERE released_at IS NULL;

CREATE UNIQUE INDEX idx_merge_intents_change_open ON merge_intents(change_id) WHERE completed_at IS NULL;

CREATE INDEX idx_review_comments_thread_created ON review_comments(thread_id, created_at, id);

CREATE INDEX idx_review_threads_change_state ON review_threads(change_id, state, created_at);

CREATE UNIQUE INDEX idx_review_threads_idem
	ON review_threads(change_id, anchor_commit_sha, file_path, line, body_hash)
	WHERE body_hash != '';

CREATE INDEX idx_review_threads_task_state ON review_threads(task_id, state, created_at);

CREATE INDEX idx_session_messages_pending ON session_messages(session_id, state, created_at);

CREATE INDEX idx_sessions_change ON sessions(change_id, created_at);

CREATE UNIQUE INDEX idx_sessions_one_active_author_per_task ON sessions(task_id) WHERE role = 'author' AND runtime_state IN ('starting', 'working', 'waiting');

CREATE UNIQUE INDEX idx_sessions_one_active_project_console ON sessions(role)
	WHERE role = 'console' AND task_id IS NULL AND runtime_state IN ('starting', 'working', 'waiting');

CREATE UNIQUE INDEX idx_sessions_one_active_task_console ON sessions(task_id)
	WHERE role = 'console' AND task_id IS NOT NULL AND runtime_state IN ('starting', 'working', 'waiting');

CREATE INDEX idx_sessions_workflow_node ON sessions(workflow_run_id, node_run_id);

CREATE INDEX idx_status_log_task_created ON status_log(task_id, created_at DESC, id DESC);

CREATE INDEX idx_status_log_task_kind_resolved
	ON status_log(task_id, kind, resolved_at, created_at DESC, id DESC);

CREATE INDEX idx_task_attachments_stage ON task_attachments(stage);

CREATE INDEX idx_task_attachments_task_id ON task_attachments(task_id, created_at);

CREATE INDEX idx_task_tags_tag_id ON task_tags(tag_id);

CREATE INDEX idx_tasks_done_at ON tasks(done_at DESC, id DESC) WHERE lifecycle_state = 'done';

CREATE INDEX idx_tasks_feature_id ON tasks(feature_id);

CREATE INDEX idx_tasks_flow_id ON tasks(flow_id);

CREATE INDEX idx_tasks_lifecycle_state ON tasks(lifecycle_state, updated_at);

CREATE INDEX idx_tasks_source_task_id ON tasks(source_task_id);

CREATE INDEX idx_terminal_access_tokens_session ON terminal_access_tokens(session_id, expires_at);

CREATE UNIQUE INDEX idx_work_item_relations_one_parent ON work_item_relations(target_item_id) WHERE kind = 'parent_of';

CREATE INDEX idx_work_item_relations_source ON work_item_relations(source_item_id, kind);

CREATE INDEX idx_work_item_relations_target ON work_item_relations(target_item_id, kind);

CREATE UNIQUE INDEX idx_worker_assignments_active_job
	ON worker_assignments(job_id)
	WHERE state IN ('pending', 'claimed');

CREATE INDEX idx_worker_assignments_cleanup
	ON worker_assignments(provider_id, state, cleaned_at);

CREATE INDEX idx_worker_assignments_provider_profile_state
	ON worker_assignments(provider_id, profile_name, state);

CREATE INDEX idx_worker_assignments_startup_deadline
	ON worker_assignments(state, startup_deadline);

CREATE INDEX idx_workflow_artifacts_run ON workflow_artifacts(workflow_run_id, created_at);

CREATE UNIQUE INDEX idx_workflow_node_runs_one_active
	ON workflow_node_runs(workflow_run_id)
	WHERE state IN ('queued', 'running', 'waiting');

CREATE INDEX idx_workflow_runs_held ON workflow_runs(held_at) WHERE held_at IS NOT NULL;

CREATE UNIQUE INDEX idx_workflow_runs_one_active
	ON workflow_runs(task_id)
	WHERE state IN ('scheduled', 'running', 'waiting');

CREATE INDEX idx_workflow_transitions_convergence
    ON workflow_transitions(workflow_run_id, event_kind, seq DESC);

CREATE UNIQUE INDEX idx_workflow_transitions_idempotency
	ON workflow_transitions(workflow_run_id, idempotency_key)
	WHERE idempotency_key IS NOT NULL;

CREATE INDEX idx_workflow_transitions_task_seq ON workflow_transitions(task_id, seq DESC);

CREATE UNIQUE INDEX idx_workflow_waits_one_open
	ON workflow_waits(workflow_run_id) WHERE state = 'open';

-- Read models.

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

-- Runtime invariant and lifecycle triggers. All referenced tables exist above.

CREATE TRIGGER close_worker_assignment_on_terminal_job
AFTER UPDATE OF state ON jobs
WHEN NEW.state IN ('finished', 'failed', 'crashed', 'canceled')
BEGIN
	UPDATE worker_assignments
	SET state = 'closed', updated_at = NEW.updated_at,
		closed_at = COALESCE(closed_at, NEW.updated_at), close_reason = NEW.state
	WHERE job_id = NEW.id AND state IN ('pending', 'claimed');
END;

CREATE TRIGGER epics_require_epic_work_item
BEFORE INSERT ON epics
WHEN COALESCE((SELECT kind FROM work_items WHERE id = NEW.id), '') != 'epic'
BEGIN
	SELECT RAISE(ABORT, 'epic requires matching epic work item');
END;

CREATE TRIGGER features_require_feature_work_item
BEFORE INSERT ON features
WHEN COALESCE((SELECT kind FROM work_items WHERE id = NEW.id), '') != 'feature'
BEGIN
	SELECT RAISE(ABORT, 'feature requires matching feature work item');
END;

CREATE TRIGGER harness_archive_member_sets_insert_guard
BEFORE INSERT ON harness_archive_member_sets
WHEN NEW.capture_id IS NOT (SELECT capture_id FROM history_artifacts WHERE id = NEW.artifact_id)
    OR NEW.member_count != (SELECT COUNT(*) FROM harness_archive_members WHERE artifact_id = NEW.artifact_id)
BEGIN
    SELECT RAISE(ABORT, 'invalid Harness archive member set declaration');
END;

CREATE TRIGGER harness_archive_member_sets_no_delete
BEFORE DELETE ON harness_archive_member_sets
BEGIN
    SELECT RAISE(ABORT, 'Harness archive member sets are retained');
END;

CREATE TRIGGER harness_archive_member_sets_no_update
BEFORE UPDATE ON harness_archive_member_sets
BEGIN
    SELECT RAISE(ABORT, 'Harness archive member sets are immutable');
END;

CREATE TRIGGER harness_archive_members_insert_guard
BEFORE INSERT ON harness_archive_members
WHEN EXISTS (SELECT 1 FROM harness_archive_member_sets declared WHERE declared.artifact_id = NEW.artifact_id)
BEGIN
    SELECT RAISE(ABORT, 'Harness archive member sets are immutable');
END;

CREATE TRIGGER harness_archive_members_no_delete
BEFORE DELETE ON harness_archive_members
BEGIN
    SELECT RAISE(ABORT, 'Harness archive members are retained');
END;

CREATE TRIGGER harness_archive_members_no_update
BEFORE UPDATE ON harness_archive_members
BEGIN
    SELECT RAISE(ABORT, 'Harness archive members are immutable');
END;

CREATE TRIGGER history_artifacts_checkpoint_shape_insert
BEFORE INSERT ON history_artifacts
WHEN NOT (
    (NEW.phase = 'checkpoint' AND length(trim(NEW.checkpoint_stream)) > 0)
    OR (NEW.phase = 'final' AND NEW.checkpoint_stream = '')
)
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact checkpoint stream');
END;

CREATE TRIGGER history_artifacts_committed_at_guard
BEFORE UPDATE ON history_artifacts
WHEN NEW.publication_state IS OLD.publication_state
    AND NEW.committed_at IS NOT OLD.committed_at
BEGIN
    SELECT RAISE(ABORT, 'history artifact committed timestamp is immutable');
END;

CREATE TRIGGER history_artifacts_immutable_metadata
BEFORE UPDATE ON history_artifacts
WHEN NEW.id IS NOT OLD.id
    OR NEW.capture_id IS NOT OLD.capture_id
    OR NEW.logical_key IS NOT OLD.logical_key
    OR NEW.kind IS NOT OLD.kind
    OR NEW.phase IS NOT OLD.phase
    OR NEW.checkpoint_generation IS NOT OLD.checkpoint_generation
    OR NEW.checkpoint_stream IS NOT OLD.checkpoint_stream
    OR NEW.checkpoint_trigger IS NOT OLD.checkpoint_trigger
    OR NEW.archive_id IS NOT OLD.archive_id
    OR NEW.media_type IS NOT OLD.media_type
    OR NEW.format_version IS NOT OLD.format_version
    OR NEW.schema_version IS NOT OLD.schema_version
    OR NEW.sha256 IS NOT OLD.sha256
    OR NEW.stored_size IS NOT OLD.stored_size
    OR NEW.logical_size IS NOT OLD.logical_size
    OR NEW.entry_count IS NOT OLD.entry_count
    OR NEW.temporary_upload_id IS NOT OLD.temporary_upload_id
    OR NEW.blob_key IS NOT OLD.blob_key
    OR NEW.pending_at IS NOT OLD.pending_at
    OR NEW.created_at IS NOT OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'history artifact metadata is immutable');
END;

CREATE TRIGGER history_artifacts_no_delete
BEFORE DELETE ON history_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history artifacts are retained');
END;

CREATE TRIGGER history_artifacts_publication_guard
BEFORE UPDATE ON history_artifacts
WHEN NEW.publication_state IS NOT OLD.publication_state
    AND NOT (
        OLD.publication_state = 'pending'
        AND NEW.publication_state = 'committed'
        AND OLD.committed_at IS NULL
        AND NEW.committed_at IS NOT NULL
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact publication transition');
END;

CREATE TRIGGER history_artifacts_supersession_guard
BEFORE UPDATE ON history_artifacts
WHEN (OLD.superseded_by_artifact_id IS NOT NULL AND (
        NEW.superseded_by_artifact_id IS NULL
        OR NEW.superseded_at IS NULL
        OR ((NEW.superseded_by_artifact_id IS NOT OLD.superseded_by_artifact_id
                OR NEW.superseded_at IS NOT OLD.superseded_at)
            AND NOT EXISTS (
                SELECT 1 FROM history_artifacts final_manifest
                WHERE final_manifest.id = NEW.superseded_by_artifact_id
                  AND final_manifest.capture_id = NEW.capture_id
                  AND final_manifest.kind = 'manifest'
                  AND final_manifest.phase = 'final'
                  AND final_manifest.publication_state = 'committed'
            ))
    ))
    OR (OLD.superseded_by_artifact_id IS NULL AND NEW.superseded_by_artifact_id IS NOT NULL
        AND NEW.superseded_at IS NULL)
    OR (OLD.superseded_by_artifact_id IS NULL AND NEW.superseded_by_artifact_id IS NULL
        AND NEW.superseded_at IS NOT OLD.superseded_at)
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact supersession transition');
END;

CREATE TRIGGER history_capture_events_no_delete
BEFORE DELETE ON history_capture_events
BEGIN
    SELECT RAISE(ABORT, 'history capture events are append-only');
END;

CREATE TRIGGER history_capture_events_no_update
BEFORE UPDATE ON history_capture_events
BEGIN
    SELECT RAISE(ABORT, 'history capture events are append-only');
END;

CREATE TRIGGER history_capture_expected_artifacts_insert_guard
BEFORE INSERT ON history_capture_expected_artifacts
WHEN (SELECT expected_set_declared_at FROM history_captures WHERE id = NEW.capture_id) IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable after declaration');
END;

CREATE TRIGGER history_capture_expected_artifacts_no_delete
BEFORE DELETE ON history_capture_expected_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable');
END;

CREATE TRIGGER history_capture_expected_artifacts_no_update
BEFORE UPDATE ON history_capture_expected_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable');
END;

CREATE TRIGGER history_captures_execution_guard
BEFORE UPDATE ON history_captures
WHEN (NEW.execution_verdict IS NOT OLD.execution_verdict AND (
        OLD.execution_verdict != 'pending'
        OR NEW.execution_verdict = 'pending'
        OR NEW.execution_recorded_at IS NULL
    ))
    OR (NEW.execution_verdict IS OLD.execution_verdict AND (
        NEW.execution_exit_code IS NOT OLD.execution_exit_code
        OR NEW.execution_error_code IS NOT OLD.execution_error_code
        OR NEW.execution_recorded_at IS NOT OLD.execution_recorded_at
    ))
BEGIN
    SELECT RAISE(ABORT, 'invalid history execution verdict transition');
END;

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

CREATE TRIGGER history_captures_grant_generation_guard
BEFORE UPDATE ON history_captures
WHEN NEW.upload_grant_hash IS OLD.upload_grant_hash
    AND (
        NEW.upload_grant_generation IS NOT OLD.upload_grant_generation
        OR NEW.upload_grant_rotated_at IS NOT OLD.upload_grant_rotated_at
    )
BEGIN
    SELECT RAISE(ABORT, 'history upload grant generation requires rotation');
END;

CREATE TRIGGER history_captures_grant_rotation_guard
BEFORE UPDATE ON history_captures
WHEN NEW.upload_grant_hash IS NOT OLD.upload_grant_hash
    AND NOT (
        OLD.upload_grant_revoked_at IS NULL
        AND NEW.upload_grant_revoked_at IS NULL
        AND NEW.upload_grant_generation = OLD.upload_grant_generation + 1
        AND NEW.upload_grant_rotated_at IS NOT NULL
        AND NEW.upload_grant_rotated_at IS NOT OLD.upload_grant_rotated_at
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history upload grant rotation');
END;

CREATE TRIGGER history_captures_immutable_attribution
BEFORE UPDATE ON history_captures
WHEN NEW.id IS NOT OLD.id
    OR NEW.project_id IS NOT OLD.project_id
    OR NEW.job_id IS NOT OLD.job_id
    OR NEW.lease_id IS NOT OLD.lease_id
    OR NEW.lease_attempt IS NOT OLD.lease_attempt
    OR NEW.worker_id IS NOT OLD.worker_id
    OR NEW.task_id IS NOT OLD.task_id
    OR NEW.session_id IS NOT OLD.session_id
    OR NEW.workflow_run_id IS NOT OLD.workflow_run_id
    OR NEW.node_run_id IS NOT OLD.node_run_id
    OR NEW.node_visit IS NOT OLD.node_visit
    OR NEW.stage IS NOT OLD.stage
    OR NEW.role IS NOT OLD.role
    OR NEW.harness_name IS NOT OLD.harness_name
    OR NEW.harness_version IS NOT OLD.harness_version
    OR NEW.expected_transcript IS NOT OLD.expected_transcript
    OR NEW.expected_harness IS NOT OLD.expected_harness
BEGIN
    SELECT RAISE(ABORT, 'history capture attribution is immutable');
END;

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
    OR (OLD.running_at IS NULL AND NEW.running_at IS NOT NULL AND NEW.state != 'running')
    OR (OLD.quiescing_at IS NULL AND NEW.quiescing_at IS NOT NULL AND NEW.state != 'quiescing')
    OR (OLD.sealed_at IS NULL AND NEW.sealed_at IS NOT NULL AND NEW.state != 'sealed')
    OR (OLD.uploading_at IS NULL AND NEW.uploading_at IS NOT NULL AND NEW.state != 'uploading')
    OR (OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL AND NEW.state != 'complete')
    OR (OLD.blocked_at IS NULL AND NEW.blocked_at IS NOT NULL AND NEW.state != 'blocked')
    OR (OLD.lost_at IS NULL AND NEW.lost_at IS NOT NULL AND NEW.state != 'lost')
    OR (OLD.waived_at IS NULL AND NEW.waived_at IS NOT NULL AND NEW.state != 'waived')
    OR (OLD.upload_grant_revoked_at IS NULL AND NEW.upload_grant_revoked_at IS NOT NULL
        AND NEW.state NOT IN ('complete', 'waived'))
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

CREATE TRIGGER history_captures_no_delete
BEFORE DELETE ON history_captures
BEGIN
    SELECT RAISE(ABORT, 'history captures are retained');
END;

CREATE TRIGGER history_captures_state_transition_guard
BEFORE UPDATE ON history_captures
WHEN NEW.state IS NOT OLD.state
    AND NOT (
        (OLD.state = 'reserved' AND NEW.state = 'running')
        OR (OLD.state = 'running' AND NEW.state = 'quiescing')
        OR (OLD.state = 'quiescing' AND NEW.state = 'sealed')
        OR (OLD.state = 'sealed' AND NEW.state = 'uploading')
        OR (NEW.state IN ('blocked', 'lost') AND OLD.state NOT IN ('complete', 'waived'))
        OR (OLD.state IN ('blocked', 'lost') AND NEW.state IN ('running', 'quiescing', 'sealed', 'uploading'))
        OR (NEW.state = 'complete' AND OLD.state IN ('uploading', 'sealed', 'blocked', 'lost'))
        OR (NEW.state = 'waived' AND OLD.state != 'complete')
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture state transition');
END;

CREATE TRIGGER history_captures_version_guard
BEFORE UPDATE ON history_captures
WHEN NEW.version < OLD.version
    OR NEW.version > OLD.version + 1
    OR (NEW.state IS NOT OLD.state AND NEW.version != OLD.version + 1)
    OR (NEW.execution_verdict IS NOT OLD.execution_verdict AND NEW.version != OLD.version + 1)
    OR (NEW.expected_set_declared_at IS NOT OLD.expected_set_declared_at AND NEW.version != OLD.version + 1)
    OR (NEW.upload_grant_hash IS NOT OLD.upload_grant_hash AND NEW.version != OLD.version + 1)
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture version transition');
END;

CREATE TRIGGER history_transcript_segments_no_delete
BEFORE DELETE ON history_transcript_segments
BEGIN
    SELECT RAISE(ABORT, 'history transcript segments are immutable');
END;

CREATE TRIGGER history_transcript_segments_no_update
BEFORE UPDATE ON history_transcript_segments
WHEN NOT (
    OLD.raw_sha256 = ''
    AND length(NEW.raw_sha256) = 64
    AND NEW.raw_sha256 NOT GLOB '*[^0-9a-f]*'
    AND NEW.capture_id IS OLD.capture_id
    AND NEW.epoch IS OLD.epoch
    AND NEW.sequence IS OLD.sequence
    AND NEW.start_offset IS OLD.start_offset
    AND NEW.end_offset IS OLD.end_offset
    AND NEW.uncompressed_size IS OLD.uncompressed_size
    AND NEW.stored_size IS OLD.stored_size
    AND NEW.sha256 IS OLD.sha256
    AND NEW.encoding IS OLD.encoding
    AND NEW.artifact_id IS OLD.artifact_id
    AND NEW.sealed_at IS OLD.sealed_at
    AND NEW.created_at IS OLD.created_at
)
BEGIN
    SELECT RAISE(ABORT, 'history transcript segments are immutable');
END;

CREATE TRIGGER history_transcript_segments_raw_insert_guard
BEFORE INSERT ON history_transcript_segments
WHEN length(NEW.raw_sha256) != 64 OR NEW.raw_sha256 GLOB '*[^0-9a-f]*'
BEGIN
    SELECT RAISE(ABORT, 'history transcript raw digest is required');
END;

CREATE TRIGGER history_transcript_streams_no_reopen
BEFORE UPDATE ON history_transcript_streams
WHEN OLD.state = 'sealed' AND (
    NEW.state IS NOT OLD.state
    OR NEW.segment_count IS NOT OLD.segment_count
    OR NEW.logical_length IS NOT OLD.logical_length
    OR NEW.last_epoch IS NOT OLD.last_epoch
    OR NEW.last_sequence IS NOT OLD.last_sequence
    OR NEW.stream_sha256 IS NOT OLD.stream_sha256
    OR NEW.sealed_at IS NOT OLD.sealed_at
)
BEGIN
    SELECT RAISE(ABORT, 'sealed history transcript stream is immutable');
END;

CREATE TRIGGER history_upload_events_no_delete
BEFORE DELETE ON history_upload_events
BEGIN
    SELECT RAISE(ABORT, 'history upload events are append-only');
END;

CREATE TRIGGER history_upload_events_no_update
BEFORE UPDATE ON history_upload_events
BEGIN
    SELECT RAISE(ABORT, 'history upload events are append-only');
END;

CREATE TRIGGER history_upload_intents_no_delete
BEFORE DELETE ON history_upload_intents
BEGIN
    SELECT RAISE(ABORT, 'history upload intents are retained');
END;

CREATE TRIGGER history_upload_intents_update_guard
BEFORE UPDATE ON history_upload_intents
WHEN NEW.temporary_upload_id IS NOT OLD.temporary_upload_id
    OR NEW.capture_id IS NOT OLD.capture_id
    OR NEW.sha256 IS NOT OLD.sha256
    OR NEW.stored_size IS NOT OLD.stored_size
    OR NEW.created_at IS NOT OLD.created_at
    OR OLD.state != 'active'
    OR NOT (
        (NEW.state = 'active'
            AND NEW.artifact_id IS NULL
            AND NEW.consumed_at IS NULL
            AND NEW.abandoned_at IS NULL
            AND NEW.heartbeat_at >= OLD.heartbeat_at)
        OR (NEW.state = 'consumed'
            AND NEW.artifact_id IS NOT NULL
            AND NEW.consumed_at IS NOT NULL
            AND NEW.abandoned_at IS NULL)
        OR (NEW.state = 'abandoned'
            AND NEW.artifact_id IS NULL
            AND NEW.consumed_at IS NULL
            AND NEW.abandoned_at IS NOT NULL)
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history upload intent transition');
END;

CREATE TRIGGER tasks_require_task_work_item
BEFORE INSERT ON tasks
WHEN COALESCE((SELECT kind FROM work_items WHERE id = NEW.id), '') != 'task'
BEGIN
	SELECT RAISE(ABORT, 'task requires matching task work item');
END;

CREATE TRIGGER work_items_kind_is_immutable
BEFORE UPDATE OF kind ON work_items
WHEN NEW.kind != OLD.kind
BEGIN
	SELECT RAISE(ABORT, 'work item kind is immutable');
END;

CREATE TRIGGER workflow_runs_require_task_work_item
BEFORE INSERT ON workflow_runs
WHEN COALESCE((SELECT kind FROM work_items WHERE id = NEW.task_id), '') != 'task'
BEGIN
	SELECT RAISE(ABORT, 'workflow run requires task work item');
END;
