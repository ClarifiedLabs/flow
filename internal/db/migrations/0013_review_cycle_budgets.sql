-- Restore the legacy author-session review-cycle budget removed when the
-- generic workflow schema replaced the launch schema. SessionService still
-- uses this task-scoped budget when an owner reply queues a review-fix author
-- job outside an active session.

CREATE TABLE review_cycle_budgets (
	task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
	granted_cycles INTEGER NOT NULL CHECK (granted_cycles >= 0),
	used_cycles INTEGER NOT NULL DEFAULT 0 CHECK (used_cycles >= 0),
	exhausted_at TEXT,
	last_approved_at TEXT,
	last_approved_by TEXT NOT NULL DEFAULT '',
	last_instructions TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	CHECK (used_cycles <= granted_cycles)
);

UPDATE app_metadata
SET value = '0013_review_cycle_budgets', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
