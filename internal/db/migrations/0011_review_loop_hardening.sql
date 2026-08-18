-- Review-loop hardening.
--
-- rebase_publications records publication intent: durable proof linking a
-- rebase reservation to the exact ref update flow intends to perform, written
-- before the push. A feature_rebases row may only reach 'finalized' when the
-- exchange's post-receive spool, drained into git_events, confirms that this
-- specific (old_tip, new_tip) update happened; observation of a moved ref is
-- never proof.
--
-- review_threads.reopen_count counts how often a certified/claimed thread was
-- reopened, and review_threads.disposition records whether the reviewer thinks
-- the concern was introduced by the change under review (blocks) or is
-- pre-existing (does not block; scheduled as a linked follow-up task).

CREATE TABLE rebase_publications (
	id TEXT PRIMARY KEY,
	rebase_id TEXT NOT NULL REFERENCES feature_rebases(id) ON DELETE CASCADE,
	old_tip_sha TEXT NOT NULL,
	new_tip_sha TEXT NOT NULL,
	recorded_at TEXT NOT NULL,
	confirmed_event_hash TEXT REFERENCES git_events(event_hash),
	UNIQUE (rebase_id, new_tip_sha)
);
CREATE INDEX idx_rebase_publications_rebase ON rebase_publications(rebase_id);

ALTER TABLE review_threads ADD COLUMN reopen_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_threads ADD COLUMN disposition TEXT NOT NULL DEFAULT ''
	CHECK (disposition IN ('', 'introduced_by_change', 'preexisting'));

UPDATE app_metadata
SET value = '0011_review_loop_hardening', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
