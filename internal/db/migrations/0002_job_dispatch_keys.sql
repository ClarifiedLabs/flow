ALTER TABLE jobs ADD COLUMN dispatch_key TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_jobs_live_dispatch_key
	ON jobs(dispatch_key)
	WHERE dispatch_key <> ''
		AND state IN ('queued', 'claimed', 'running');

UPDATE app_metadata
SET value = '0002_job_dispatch_keys',
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
