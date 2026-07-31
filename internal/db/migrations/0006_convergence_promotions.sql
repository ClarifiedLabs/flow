-- A convergence promotion replaces an oversized source implementation with a
-- feature planning workflow rooted at the exact reviewed base commit. The
-- intent is durable before the feature ref is created so a coordinator restart
-- can safely replay each later step without adopting a different Git state.

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

CREATE INDEX idx_convergence_promotions_state
    ON convergence_promotions(state, created_at);

-- The disposition audit row is written after the durable intent, and the
-- convergence decision helpers re-read it from the transitions feed.
CREATE INDEX idx_workflow_transitions_convergence
    ON workflow_transitions(workflow_run_id, event_kind, seq DESC);

UPDATE app_metadata
SET value = '0006_convergence_promotions',
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
