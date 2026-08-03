ALTER TABLE feature_creation_intents
	ADD COLUMN relation_payload_json TEXT NOT NULL DEFAULT '[]'
	CHECK (json_valid(relation_payload_json));

ALTER TABLE feature_creation_intents
	ADD COLUMN ref_created_by_intent BOOLEAN NOT NULL DEFAULT FALSE
	CHECK (ref_created_by_intent IN (FALSE, TRUE));

UPDATE app_metadata
SET value = '0012_feature_creation_relation_payload', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
