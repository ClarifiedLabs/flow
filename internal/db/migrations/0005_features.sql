-- Features group a project's tasks behind a long-lived feature branch in the
-- exchange remote. tasks.feature_id assigns a task to a feature; feature
-- branches live under refs/heads/feature/* with coordinator-only updates, and
-- every rebase is recorded in feature_rebases for audit, crash recovery, and
-- the finalize node's compare-and-swap record.

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

CREATE TRIGGER features_require_feature_work_item
BEFORE INSERT ON features
WHEN COALESCE((SELECT kind FROM work_items WHERE id = NEW.id), '') != 'feature'
BEGIN
	SELECT RAISE(ABORT, 'feature requires matching feature work item');
END;

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
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

-- task_id is NULL for clean instant rebases, which never create a system
-- rebase task. The partial unique index guarantees at most one running rebase
-- per feature.
CREATE TABLE feature_rebases (
	id TEXT PRIMARY KEY,
	feature_id TEXT NOT NULL REFERENCES features(id) ON DELETE CASCADE,
	task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
	old_tip_sha TEXT NOT NULL,
	target_base TEXT NOT NULL,
	target_base_sha TEXT NOT NULL,
	target_feature_id TEXT REFERENCES features(id) ON DELETE RESTRICT,
	new_tip_sha TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL CHECK (state IN ('running', 'finalized', 'stale', 'failed', 'cancelled')),
	created_at TEXT NOT NULL,
	completed_at TEXT
);

CREATE UNIQUE INDEX idx_feature_rebases_one_running
	ON feature_rebases(feature_id) WHERE state = 'running';

-- Feature and rebase ids allocate from their own per-project sequences,
-- mirroring task ids.
INSERT INTO id_allocators (name, next_number)
VALUES ('feature', 1), ('feature_rebase', 1);

ALTER TABLE tasks
	ADD COLUMN feature_id TEXT REFERENCES features(id) ON DELETE SET NULL;

CREATE INDEX idx_tasks_feature_id ON tasks(feature_id);

-- Extend the flow_nodes kind CHECK with 'finalize_rebase'. SQLite cannot alter
-- a CHECK constraint, so the table is rebuilt. DROP TABLE performs an implicit
-- delete that fires foreign-key actions, and flow_edges references flow_nodes
-- ON DELETE CASCADE, so flow_edges must be rebuilt alongside to keep its rows.
CREATE TABLE flow_nodes_new (
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

INSERT INTO flow_nodes_new (id, flow_id, node_key, name, kind, position, config_json)
SELECT id, flow_id, node_key, name, kind, position, config_json FROM flow_nodes;

CREATE TABLE flow_edges_backup AS
SELECT flow_id, from_node_key, outcome, to_node_key FROM flow_edges;

DROP TABLE flow_edges;
DROP TABLE flow_nodes;

ALTER TABLE flow_nodes_new RENAME TO flow_nodes;

CREATE TABLE flow_edges (
	flow_id TEXT NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
	from_node_key TEXT NOT NULL,
	outcome TEXT NOT NULL CHECK (length(trim(outcome)) > 0),
	to_node_key TEXT NOT NULL,
	PRIMARY KEY (flow_id, from_node_key, outcome),
	FOREIGN KEY (flow_id, from_node_key) REFERENCES flow_nodes(flow_id, node_key) ON DELETE CASCADE,
	FOREIGN KEY (flow_id, to_node_key) REFERENCES flow_nodes(flow_id, node_key) ON DELETE CASCADE
);

INSERT INTO flow_edges (flow_id, from_node_key, outcome, to_node_key)
SELECT flow_id, from_node_key, outcome, to_node_key FROM flow_edges_backup;

DROP TABLE flow_edges_backup;

UPDATE app_metadata
SET value = '0005_features',
	updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
