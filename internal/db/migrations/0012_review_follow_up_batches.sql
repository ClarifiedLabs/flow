-- Durable review follow-up batching and organizer planning.
--
-- Aggregation reports are captured as immutable batches. Their eligible,
-- non-blocking task_action occurrences accumulate in a lineage-scoped set and
-- remain proposals until a separately human-reviewed organizer plan is
-- materialized.

CREATE TABLE review_follow_up_sets (
	id TEXT PRIMARY KEY,
	source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	source_change_id TEXT NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
	workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
	revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
	state TEXT NOT NULL DEFAULT 'open'
		CHECK (state IN ('open', 'organizer_pending', 'organizing', 'awaiting_review', 'materializing', 'materialized', 'attention', 'closed')),
	organizer_task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	active_plan_artifact_id TEXT REFERENCES workflow_artifacts(id) ON DELETE SET NULL,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (source_task_id, source_change_id, workflow_run_id)
);
CREATE INDEX idx_review_follow_up_sets_state
	ON review_follow_up_sets(state, updated_at, id);

CREATE TABLE review_follow_up_batches (
	id TEXT PRIMARY KEY,
	set_id TEXT NOT NULL REFERENCES review_follow_up_sets(id) ON DELETE CASCADE,
	source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	source_change_id TEXT NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
	workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
	node_run_id TEXT NOT NULL REFERENCES workflow_node_runs(id) ON DELETE CASCADE,
	check_name TEXT NOT NULL CHECK (length(trim(check_name)) > 0),
	source_job_id TEXT NOT NULL UNIQUE REFERENCES jobs(id) ON DELETE RESTRICT,
	reviewed_head_sha TEXT NOT NULL CHECK (length(trim(reviewed_head_sha)) > 0),
	report_sha256 TEXT NOT NULL CHECK (length(report_sha256) = 64),
	report_json TEXT NOT NULL CHECK (json_valid(report_json)),
	state TEXT NOT NULL DEFAULT 'accepted'
		CHECK (state IN ('accepted', 'superseded', 'invalidated')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX idx_review_follow_up_batches_set
	ON review_follow_up_batches(set_id, created_at, id);

CREATE TABLE review_follow_up_proposals (
	id TEXT PRIMARY KEY,
	batch_id TEXT NOT NULL REFERENCES review_follow_up_batches(id) ON DELETE CASCADE,
	comment_index INTEGER NOT NULL CHECK (comment_index >= 0),
	finding_hash TEXT NOT NULL CHECK (length(finding_hash) = 64),
	sha TEXT NOT NULL,
	file_path TEXT NOT NULL,
	line INTEGER NOT NULL CHECK (line > 0),
	body TEXT NOT NULL,
	severity TEXT NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low')),
	introduced_by_change BOOLEAN NOT NULL CHECK (introduced_by_change IN (FALSE, TRUE)),
	requirement TEXT NOT NULL,
	requirement_source TEXT NOT NULL,
	finding_basis TEXT NOT NULL,
	remediation_scope TEXT NOT NULL,
	scope_rationale TEXT NOT NULL,
	follow_up TEXT NOT NULL DEFAULT '',
	suggested_action TEXT NOT NULL CHECK (suggested_action IN ('create_task', 'use_existing_task')),
	suggested_title TEXT NOT NULL DEFAULT '',
	suggested_body TEXT NOT NULL DEFAULT '',
	suggested_task_id TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'active'
		CHECK (state IN ('active', 'dispositioned', 'withdrawn')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (batch_id, comment_index)
);
CREATE INDEX idx_review_follow_up_proposals_batch
	ON review_follow_up_proposals(batch_id, comment_index);
CREATE INDEX idx_review_follow_up_proposals_state
	ON review_follow_up_proposals(state, batch_id);

CREATE TABLE review_follow_up_plan_revisions (
	id TEXT PRIMARY KEY,
	set_id TEXT NOT NULL REFERENCES review_follow_up_sets(id) ON DELETE CASCADE,
	set_revision INTEGER NOT NULL CHECK (set_revision > 0),
	organizer_task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	organizer_workflow_run_id TEXT REFERENCES workflow_runs(id) ON DELETE SET NULL,
	plan_artifact_id TEXT REFERENCES workflow_artifacts(id) ON DELETE SET NULL,
	plan_sha256 TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'pending'
		CHECK (state IN ('pending', 'organizing', 'awaiting_review', 'approved', 'rejected', 'materializing', 'materialized', 'failed', 'stale')),
	materialization_result_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(materialization_result_json)),
	materialization_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (set_id, set_revision)
);
CREATE INDEX idx_review_follow_up_plan_revisions_task
	ON review_follow_up_plan_revisions(organizer_task_id, set_revision);

CREATE TABLE review_follow_up_dispositions (
	proposal_id TEXT NOT NULL REFERENCES review_follow_up_proposals(id) ON DELETE CASCADE,
	plan_revision_id TEXT NOT NULL REFERENCES review_follow_up_plan_revisions(id) ON DELETE CASCADE,
	disposition TEXT NOT NULL
		CHECK (disposition IN ('create_task', 'use_existing_task', 'merge_with_proposal', 'covered_by_source', 'discard_duplicate')),
	item_key TEXT,
	target_task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	canonical_proposal_id TEXT REFERENCES review_follow_up_proposals(id) ON DELETE RESTRICT,
	rationale TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (proposal_id, plan_revision_id)
);
CREATE INDEX idx_review_follow_up_dispositions_target
	ON review_follow_up_dispositions(target_task_id, plan_revision_id);

UPDATE app_metadata
SET value = '0012_review_follow_up_batches', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
