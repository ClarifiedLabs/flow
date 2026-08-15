-- One-shot guards that keep watchdog-observed output produced while an agent is
-- parking from immediately undoing an authoritative human-wait signal.

CREATE TABLE session_human_wait_latches (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    created_at TEXT NOT NULL
);

UPDATE app_metadata
SET value = '0007_session_human_wait_latches', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
