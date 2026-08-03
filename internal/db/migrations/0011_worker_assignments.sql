CREATE TABLE worker_assignments (
	id TEXT PRIMARY KEY,
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
	CHECK ((state = 'claimed') = (claimed_lease_id IS NOT NULL) OR state = 'closed'),
	CHECK ((state = 'closed') = (closed_at IS NOT NULL))
);

CREATE UNIQUE INDEX idx_worker_assignments_active_job
	ON worker_assignments(job_id)
	WHERE state IN ('pending', 'claimed');

CREATE INDEX idx_worker_assignments_provider_profile_state
	ON worker_assignments(provider_id, profile_name, state);

CREATE INDEX idx_worker_assignments_cleanup
	ON worker_assignments(provider_id, state, cleaned_at);

CREATE INDEX idx_worker_assignments_startup_deadline
	ON worker_assignments(state, startup_deadline);

-- Workflow cancellation and owner force-done paths update jobs directly inside
-- larger transactions. Keep assignment closure inseparable from every terminal
-- job transition, regardless of which service owns that transition.
CREATE TRIGGER close_worker_assignment_on_terminal_job
AFTER UPDATE OF state ON jobs
WHEN NEW.state IN ('finished', 'failed', 'crashed', 'canceled')
BEGIN
	UPDATE worker_assignments
	SET state = 'closed', updated_at = NEW.updated_at,
		closed_at = COALESCE(closed_at, NEW.updated_at), close_reason = NEW.state
	WHERE job_id = NEW.id AND state IN ('pending', 'claimed');
END;

UPDATE app_metadata
SET value = '0011_worker_assignments', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
