ALTER TABLE tasks
    ADD COLUMN requires_human_review INTEGER NOT NULL DEFAULT FALSE
    CHECK (requires_human_review IN (FALSE, TRUE));

-- Tasks created before this setting existed always entered the human-review
-- phase. Preserve that choice during upgrade; newly created tasks use the
-- column default above and opt in explicitly.
UPDATE tasks
SET requires_human_review = TRUE;

-- Existing built-in coding flows predate task-controlled gates. Upgrade only
-- the untouched seeded human-review gate. FlowService.Update advances the flow
-- timestamp, and the node/edge fingerprint prevents a customized mandatory
-- approval gate from being converted into an optional task-controlled gate.
UPDATE flow_nodes
SET config_json = json_set(
    config_json,
    '$.human_gate.task_opt_in', json('true'),
    '$.human_gate.skip_outcome', 'approved'
)
WHERE kind = 'human_gate'
  AND node_key = 'human-review'
  AND name = 'Human change review'
  AND position = 4
  AND json(config_json) = json('{"human_gate":{"instructions":"Review the change and choose whether it can proceed.","outcomes":["approved","changes_requested","rejected"]}}')
  AND flow_id IN (
      SELECT id
      FROM flows
      WHERE name = 'coding'
        AND description = 'Implement, check, review, verify, and merge a change.'
        AND start_node_key = 'implement'
        AND transition_budget = 50
        AND builtin = TRUE
        AND created_at = updated_at
  )
  AND 1 = (
      SELECT COUNT(*)
      FROM flow_edges
      WHERE flow_edges.flow_id = flow_nodes.flow_id
        AND flow_edges.to_node_key = flow_nodes.node_key
        AND flow_edges.from_node_key = 'verify'
        AND flow_edges.outcome = 'passed'
  )
  AND 1 = (
      SELECT COUNT(*)
      FROM flow_edges
      WHERE flow_edges.flow_id = flow_nodes.flow_id
        AND flow_edges.to_node_key = flow_nodes.node_key
  )
  AND 3 = (
      SELECT COUNT(*)
      FROM flow_edges
      WHERE flow_edges.flow_id = flow_nodes.flow_id
        AND flow_edges.from_node_key = flow_nodes.node_key
  )
  AND EXISTS (
      SELECT 1 FROM flow_edges
      WHERE flow_edges.flow_id = flow_nodes.flow_id
        AND flow_edges.from_node_key = flow_nodes.node_key
        AND flow_edges.outcome = 'approved'
        AND flow_edges.to_node_key = 'merge'
  )
  AND EXISTS (
      SELECT 1 FROM flow_edges
      WHERE flow_edges.flow_id = flow_nodes.flow_id
        AND flow_edges.from_node_key = flow_nodes.node_key
        AND flow_edges.outcome = 'changes_requested'
        AND flow_edges.to_node_key = 'implement'
  )
  AND EXISTS (
      SELECT 1 FROM flow_edges
      WHERE flow_edges.flow_id = flow_nodes.flow_id
        AND flow_edges.from_node_key = flow_nodes.node_key
        AND flow_edges.outcome = 'rejected'
        AND flow_edges.to_node_key = 'rejected'
  );

UPDATE app_metadata
SET value = '0008_optional_human_review', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
