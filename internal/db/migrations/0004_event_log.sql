-- Unified project event log: one durable, totally ordered stream of the
-- state changes agents care about (task lifecycle, relations, epic/feature
-- transitions, session status, git pushes). Consumers page forward with the
-- monotonically increasing seq cursor (WHERE seq > ?).

CREATE TABLE event_log (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    occurred_at TEXT NOT NULL,
    kind TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    task_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL DEFAULT '',
    change_id TEXT NOT NULL DEFAULT '',
    payload TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_event_log_task ON event_log(task_id, seq);
CREATE INDEX idx_event_log_kind ON event_log(kind, seq);

UPDATE app_metadata
SET value = '0004_event_log', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
