-- The task-bound console rebase confinement must outlive the initial
-- conflicted-rebase sweep. While a feature_rebases row is running,
-- WorkflowRunService.ScheduleAs calls FeatureService.EnsureRebaseBlock, which
-- links the rebase task as a blocker of any same-feature task scheduled after
-- the rebase started. Persist the API-side restriction on the row so the
-- schedule-time gate consults it too: when restrict_blocked_to is non-empty
-- (the comma-joined task ids a task-bound console may link), a sibling created
-- or reopened mid-rebase never receives a rebase_task blocks relation.

ALTER TABLE feature_rebases
    ADD COLUMN restrict_blocked_to TEXT NOT NULL DEFAULT '';

-- Rows already present when this migration runs predate the confinement and
-- carry no initiator provenance: a task-bound console's live rebase is
-- indistinguishable from an owner or unbound-console rebase. Stamp them with
-- the legacy sentinel so the schedule-time gate (EnsureRebaseBlock) links
-- nothing new for them — the conservative choice that keeps the confinement
-- guarantee (no blocker relation whose endpoints exclude the bound task) for a
-- legacy task-bound rebase. Rows inserted after this migration keep the ''
-- default (owner and unbound project-console rebases gate the whole feature).
UPDATE feature_rebases SET restrict_blocked_to = 'legacy';

UPDATE app_metadata
SET value = '0010_feature_rebase_block_restriction',
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
