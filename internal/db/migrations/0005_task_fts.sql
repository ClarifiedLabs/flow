-- 0005_task_fts.sql: full-text search over task title/body.
--
-- Requires the binary to be built with -tags sqlite_fts5 (Makefile, Dockerfile,
-- release scripts all set it). Without the tag CREATE VIRTUAL TABLE ... USING
-- fts5 errors loudly at migration time, which is the intended fail-closed
-- behavior: a binary that cannot search must not open a database that expects
-- the index.
--
-- task_fts is an external-content FTS5 table: the tasks row stays the source
-- of truth and triggers keep the index in sync on insert/update/delete. The
-- rowid matches tasks.rowid so ranked joins are rowid = rowid.

CREATE VIRTUAL TABLE task_fts USING fts5(
    title,
    body,
    content='tasks',
    content_rowid='rowid'
);

-- One-time backfill from existing rows.
INSERT INTO task_fts(rowid, title, body)
SELECT rowid, title, COALESCE(body, '') FROM tasks;

CREATE TRIGGER task_fts_insert AFTER INSERT ON tasks BEGIN
    INSERT INTO task_fts(rowid, title, body)
    VALUES (new.rowid, new.title, COALESCE(new.body, ''));
END;

CREATE TRIGGER task_fts_update AFTER UPDATE OF title, body ON tasks BEGIN
    INSERT INTO task_fts(task_fts, rowid, title, body)
    VALUES ('delete', old.rowid, old.title, COALESCE(old.body, ''));
    INSERT INTO task_fts(rowid, title, body)
    VALUES (new.rowid, new.title, COALESCE(new.body, ''));
END;

CREATE TRIGGER task_fts_delete AFTER DELETE ON tasks BEGIN
    INSERT INTO task_fts(task_fts, rowid, title, body)
    VALUES ('delete', old.rowid, old.title, COALESCE(old.body, ''));
END;

UPDATE app_metadata
SET value = '0005_task_fts', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
