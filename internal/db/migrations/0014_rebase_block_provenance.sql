-- Record only blocker relations created by rebase gating. Actor attribution on
-- work_item_relations is not provenance: unrelated dependencies may also be
-- system-authored and must survive stale-gate reconciliation.
--
-- Existing edges are deliberately not backfilled. Historical rows cannot be
-- distinguished reliably, so unknown provenance fails safe as an ordinary
-- dependency.
CREATE TABLE rebase_block_relations (
    source_item_id TEXT NOT NULL,
    target_item_id TEXT NOT NULL,
    relation_kind TEXT NOT NULL DEFAULT 'blocks' CHECK (relation_kind = 'blocks'),
    created_at TEXT NOT NULL,
    PRIMARY KEY (source_item_id, target_item_id),
    FOREIGN KEY (source_item_id, target_item_id, relation_kind)
        REFERENCES work_item_relations (source_item_id, target_item_id, kind)
        ON DELETE CASCADE
);

UPDATE app_metadata
SET value = '0014_rebase_block_provenance', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
