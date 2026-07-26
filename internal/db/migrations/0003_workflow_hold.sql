-- Operator hold ("held by you"). A hold is orthogonal to workflow_waits: it
-- composes with an already-open wait, which the single-slot
-- idx_workflow_waits_one_open index would otherwise forbid, and it is never
-- cleared as a side effect of node completion the way resolveOpenWaitTx clears
-- waits. The executor refuses to advance a run while held_at is set; in-flight
-- worker jobs keep running, which is what "take over in a terminal" needs.
ALTER TABLE workflow_runs ADD COLUMN held_at TEXT;

ALTER TABLE workflow_runs ADD COLUMN held_by TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_workflow_runs_held ON workflow_runs(held_at) WHERE held_at IS NOT NULL;

UPDATE app_metadata
SET value = '0003_workflow_hold',
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
