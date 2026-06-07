CREATE TABLE app_metadata (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO app_metadata (key, value, updated_at)
VALUES ('schema_version', '0001_init', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE id_allocators (
	name TEXT PRIMARY KEY,
	next_number INTEGER NOT NULL CHECK (next_number > 0)
);

INSERT INTO id_allocators (name, next_number)
VALUES ('issue', 1), ('issue_attachment', 1);

CREATE TABLE issues (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL CHECK (length(trim(title)) > 0),
	body TEXT NOT NULL DEFAULT '',
	acceptance_criteria TEXT NOT NULL DEFAULT '',
	priority INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0),
	schedule_state TEXT NOT NULL CHECK (schedule_state IN ('backlog', 'up_next', 'closed')),
	triage_state TEXT NOT NULL CHECK (triage_state IN ('triage', 'accepted', 'rejected')),
	requires_human_review INTEGER NOT NULL DEFAULT 1 CHECK (requires_human_review IN (0, 1)),
	auto_merge INTEGER NOT NULL DEFAULT 0 CHECK (auto_merge IN (0, 1)),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_by_session_id TEXT,
	source_issue_id TEXT REFERENCES issues(id) ON DELETE SET NULL,
	source_change_id TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	closed_at TEXT,
	agent_harness TEXT NOT NULL DEFAULT 'codex' CHECK (agent_harness IN ('codex', 'claude', 'harness')),
	harness_args_json TEXT NOT NULL DEFAULT '{}',
	plan_mode INTEGER NOT NULL DEFAULT 0 CHECK (plan_mode IN (0, 1)),
	plan_body TEXT NOT NULL DEFAULT '',
	plan_status_log_id INTEGER,
	plan_session_id TEXT,
	plan_submitted_at TEXT,
	plan_approved_at TEXT,
	CHECK ((triage_state != 'rejected') OR (schedule_state = 'closed' AND closed_at IS NOT NULL))
);

CREATE TABLE issue_relations (
	source_issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	target_issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('parent_of', 'blocks', 'related_to')),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	PRIMARY KEY (source_issue_id, target_issue_id, kind),
	CHECK (source_issue_id != target_issue_id)
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

CREATE TABLE issue_tags (
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL,
	PRIMARY KEY (issue_id, tag_id)
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
	issue_id TEXT REFERENCES issues(id) ON DELETE CASCADE,
	change_id TEXT REFERENCES changes(id) ON DELETE SET NULL,
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
	CHECK (role != 'author' OR issue_id IS NOT NULL)
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
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('ci', 'reviewer', 'verifier', 'human')),
	required INTEGER NOT NULL DEFAULT 1 CHECK (required IN (0, 1)),
	verdict TEXT NOT NULL CHECK (verdict IN ('pending', 'satisfied', 'blocked', 'skipped')),
	exit_code INTEGER,
	details TEXT NOT NULL DEFAULT '',
	source_job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
	reporter TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (issue_id, name),
	CHECK (length(trim(name)) > 0)
);

CREATE TABLE changes (
	id TEXT PRIMARY KEY,
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	branch TEXT NOT NULL,
	base TEXT NOT NULL DEFAULT 'main',
	head_sha TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	ready_at TEXT,
	merged_at TEXT,
	UNIQUE (issue_id, branch),
	CHECK (length(trim(branch)) > 0),
	CHECK (length(trim(base)) > 0)
);

CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	issue_id TEXT REFERENCES issues(id) ON DELETE CASCADE,
	change_id TEXT REFERENCES changes(id) ON DELETE CASCADE,
	job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	lease_id TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
	worker_id TEXT NOT NULL,
	role TEXT NOT NULL CHECK (role IN ('author', 'reviewer', 'verifier', 'console')),
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
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
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
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
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

CREATE TABLE review_cycle_budgets (
	issue_id TEXT PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
	granted_cycles INTEGER NOT NULL CHECK (granted_cycles >= 0),
	used_cycles INTEGER NOT NULL DEFAULT 0 CHECK (used_cycles >= 0),
	exhausted_at TEXT,
	last_approved_at TEXT,
	last_approved_by TEXT NOT NULL DEFAULT '',
	last_instructions TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	CHECK (used_cycles <= granted_cycles)
);

CREATE TABLE workflow_state (
	issue_id TEXT PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
	phase TEXT NOT NULL CHECK (phase IN ('backlog', 'triage', 'up_next', 'planning', 'authoring', 'critique', 'acceptance', 'approved', 'merged_closed', 'rejected_closed', 'abandoned')),
	version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
	updated_at TEXT NOT NULL
);

CREATE TABLE transitions (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	from_phase TEXT NOT NULL DEFAULT '',
	event_kind TEXT NOT NULL CHECK (length(trim(event_kind)) > 0),
	payload_json TEXT NOT NULL DEFAULT '{}',
	guard_result TEXT NOT NULL DEFAULT '',
	to_phase TEXT NOT NULL CHECK (length(trim(to_phase)) > 0),
	actor TEXT NOT NULL DEFAULT '',
	idempotency_key TEXT,
	created_at TEXT NOT NULL
);

CREATE TABLE timers (
	id TEXT PRIMARY KEY,
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	fire_at TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (length(trim(kind)) > 0),
	payload_json TEXT NOT NULL DEFAULT '{}',
	fired_at TEXT,
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	dispatched_at TEXT
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
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
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

CREATE TABLE event_inbox (
	id TEXT PRIMARY KEY,
	issue_id TEXT NOT NULL,
	event_json TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	created_at TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	confirmed_at TEXT
);

CREATE TABLE session_human_wait_latches (
	session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('plan', 'question')),
	created_at TEXT NOT NULL
);

CREATE TABLE issue_attachments (
	id TEXT PRIMARY KEY,
	issue_id TEXT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	stage TEXT NOT NULL CHECK (stage IN ('initial', 'author', 'reviewer', 'verifier')),
	filename TEXT NOT NULL CHECK (length(trim(filename)) > 0),
	content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	storage_key TEXT NOT NULL UNIQUE CHECK (length(trim(storage_key)) > 0),
	created_by TEXT NOT NULL CHECK (created_by IN ('human', 'agent', 'system')),
	created_at TEXT NOT NULL
);

CREATE VIEW issue_review_state AS
SELECT
	i.id AS issue_id,
	CASE
		WHEN EXISTS (SELECT 1 FROM changes ch WHERE ch.issue_id = i.id AND ch.merged_at IS NOT NULL) THEN 'merged'
		WHEN EXISTS (SELECT 1 FROM checks c WHERE c.issue_id = i.id AND c.required = 1 AND c.verdict = 'blocked') THEN 'changes_requested'
		WHEN EXISTS (SELECT 1 FROM checks c WHERE c.issue_id = i.id AND c.required = 1)
			AND NOT EXISTS (SELECT 1 FROM checks c WHERE c.issue_id = i.id AND c.required = 1 AND c.verdict != 'satisfied') THEN 'approved'
		ELSE 'in_review'
	END AS review_state
FROM issues i;

CREATE INDEX idx_issues_schedule_state ON issues(schedule_state);
CREATE INDEX idx_issues_triage_state ON issues(triage_state);
CREATE INDEX idx_issues_source_issue_id ON issues(source_issue_id);
-- Done view queries closed issues and distinguishes merged from abandoned changes.
CREATE INDEX idx_issues_closed_at ON issues(closed_at DESC) WHERE schedule_state = 'closed';
CREATE INDEX idx_issue_relations_target ON issue_relations(target_issue_id, kind);
CREATE UNIQUE INDEX idx_issue_relations_one_parent ON issue_relations(target_issue_id) WHERE kind = 'parent_of';
CREATE INDEX idx_issue_tags_tag_id ON issue_tags(tag_id);
CREATE INDEX idx_git_events_ref ON git_events(ref, observed_at);
CREATE INDEX idx_jobs_queue ON jobs(state, capacity_bucket, priority DESC, created_at);
CREATE UNIQUE INDEX idx_jobs_one_live_author_per_issue ON jobs(issue_id) WHERE role = 'author' AND state IN ('queued', 'claimed', 'running') AND issue_id IS NOT NULL;
-- Console work is unique per project when issue_id is NULL and per issue otherwise.
CREATE UNIQUE INDEX idx_jobs_one_live_project_console ON jobs(role)
	WHERE role = 'console' AND issue_id IS NULL AND state IN ('queued', 'claimed', 'running');
CREATE UNIQUE INDEX idx_jobs_one_live_issue_console ON jobs(issue_id)
	WHERE role = 'console' AND issue_id IS NOT NULL AND state IN ('queued', 'claimed', 'running');
CREATE INDEX idx_leases_worker_live ON leases(worker_id, capacity_bucket) WHERE released_at IS NULL;
CREATE INDEX idx_leases_expired ON leases(expires_at) WHERE released_at IS NULL;
CREATE UNIQUE INDEX idx_leases_one_live_per_job ON leases(job_id) WHERE released_at IS NULL;
CREATE INDEX idx_checks_issue_verdict ON checks(issue_id, required, verdict);
CREATE INDEX idx_changes_issue_unmerged ON changes(issue_id, merged_at);
CREATE INDEX idx_changes_issue_ready ON changes(issue_id, ready_at, merged_at);
CREATE INDEX idx_changes_issue_merged ON changes(issue_id) WHERE merged_at IS NOT NULL;
CREATE INDEX idx_jobs_change_id ON jobs(change_id);
CREATE UNIQUE INDEX idx_sessions_one_active_author_per_issue ON sessions(issue_id) WHERE role = 'author' AND runtime_state IN ('starting', 'working', 'waiting');
CREATE UNIQUE INDEX idx_sessions_one_active_project_console ON sessions(role)
	WHERE role = 'console' AND issue_id IS NULL AND runtime_state IN ('starting', 'working', 'waiting');
CREATE UNIQUE INDEX idx_sessions_one_active_issue_console ON sessions(issue_id)
	WHERE role = 'console' AND issue_id IS NOT NULL AND runtime_state IN ('starting', 'working', 'waiting');
CREATE INDEX idx_sessions_change ON sessions(change_id, created_at);
CREATE INDEX idx_session_messages_pending ON session_messages(session_id, state, created_at);
CREATE INDEX idx_status_log_issue_created ON status_log(issue_id, created_at DESC, id DESC);
CREATE INDEX idx_status_log_issue_kind_resolved
	ON status_log(issue_id, kind, resolved_at, created_at DESC, id DESC);
CREATE INDEX idx_handoff_history_change_recorded ON handoff_history(change_id, recorded_at DESC, id DESC);
CREATE INDEX idx_terminal_access_tokens_session ON terminal_access_tokens(session_id, expires_at);
CREATE INDEX idx_review_threads_change_state ON review_threads(change_id, state, created_at);
CREATE INDEX idx_review_threads_issue_state ON review_threads(issue_id, state, created_at);
-- Idempotency guard for worker-applied reviewer concerns.
CREATE UNIQUE INDEX idx_review_threads_idem
	ON review_threads(change_id, anchor_commit_sha, file_path, line, body_hash)
	WHERE body_hash != '';
CREATE INDEX idx_review_comments_thread_created ON review_comments(thread_id, created_at, id);
CREATE INDEX idx_transitions_issue_seq ON transitions(issue_id, seq);
CREATE UNIQUE INDEX idx_transitions_idempotency ON transitions(issue_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_timers_issue ON timers(issue_id);
CREATE INDEX idx_timers_due ON timers(fire_at) WHERE dispatched_at IS NULL;
CREATE INDEX idx_job_terminals_lease ON job_terminals(lease_id);
CREATE INDEX idx_job_terminal_access_tokens_job ON job_terminal_access_tokens(job_id, expires_at);
CREATE UNIQUE INDEX idx_merge_intents_change_open ON merge_intents(change_id) WHERE completed_at IS NULL;
CREATE INDEX idx_idempotency_pending ON idempotency_records(status_code, created_at);
CREATE INDEX idx_event_inbox_pending ON event_inbox(created_at) WHERE confirmed_at IS NULL;
CREATE INDEX idx_issue_attachments_issue_id ON issue_attachments(issue_id, created_at);
CREATE INDEX idx_issue_attachments_stage ON issue_attachments(stage);
