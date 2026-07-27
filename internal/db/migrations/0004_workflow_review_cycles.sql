-- Review-author loops are a distinct automation budget from ordinary graph
-- transitions. Keep the count on the frozen workflow run so it survives
-- restarts and can be resolved through the workflow wait machinery.
ALTER TABLE workflow_runs
	ADD COLUMN review_cycle_budget INTEGER NOT NULL DEFAULT 2
	CHECK (review_cycle_budget BETWEEN 1 AND 500);

ALTER TABLE workflow_runs
	ADD COLUMN review_cycles_used INTEGER NOT NULL DEFAULT 0
	CHECK (review_cycles_used >= 0);

-- Recover the count for in-flight runs created before this guard existed.
-- A review cycle is specifically an automated review/verification send-back
-- to an author agent working in the change workspace.
UPDATE workflow_runs
SET review_cycles_used = (
	SELECT COUNT(*)
	FROM workflow_transitions AS transition
	WHERE transition.workflow_run_id = workflow_runs.id
		AND transition.event_kind = 'node_completed'
		AND transition.outcome = 'changes_requested'
		AND EXISTS (
			SELECT 1
			FROM json_each(workflow_runs.flow_snapshot_json, '$.nodes') AS source
			WHERE json_extract(source.value, '$.key') = transition.from_node_key
				AND json_extract(source.value, '$.kind') IN ('change_review', 'verify_change')
		)
		AND EXISTS (
			SELECT 1
			FROM json_each(workflow_runs.flow_snapshot_json, '$.nodes') AS target
			WHERE json_extract(target.value, '$.key') = transition.to_node_key
				AND json_extract(target.value, '$.kind') = 'agent'
				AND json_extract(target.value, '$.config.agent.workspace') = 'change'
		)
);

UPDATE app_metadata
SET value = '0004_workflow_review_cycles',
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
