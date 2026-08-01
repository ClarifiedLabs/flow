-- Durable, per-project history capture metadata. Attribution is deliberately
-- snapshotted instead of foreign-keyed to mutable jobs, leases, sessions, tasks,
-- and workflow rows: ordinary source-record deletion must not erase history.
CREATE TABLE history_captures (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'hc-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    project_id TEXT NOT NULL CHECK (length(trim(project_id)) BETWEEN 1 AND 255),
    job_id TEXT NOT NULL CHECK (length(trim(job_id)) BETWEEN 1 AND 255),
    lease_id TEXT NOT NULL CHECK (length(trim(lease_id)) BETWEEN 1 AND 255),
    lease_attempt INTEGER NOT NULL CHECK (lease_attempt > 0),
    worker_id TEXT NOT NULL CHECK (length(trim(worker_id)) BETWEEN 1 AND 255),
    task_id TEXT NOT NULL DEFAULT '' CHECK (length(task_id) <= 255),
    session_id TEXT NOT NULL DEFAULT '' CHECK (length(session_id) <= 255),
    workflow_run_id TEXT NOT NULL DEFAULT '' CHECK (length(workflow_run_id) <= 255),
    node_run_id TEXT NOT NULL DEFAULT '' CHECK (length(node_run_id) <= 255),
    node_visit INTEGER CHECK (node_visit IS NULL OR node_visit > 0),
    stage TEXT NOT NULL DEFAULT '' CHECK (length(stage) <= 128),
    role TEXT NOT NULL CHECK (length(trim(role)) BETWEEN 1 AND 64),
    harness_name TEXT NOT NULL DEFAULT '' CHECK (length(harness_name) <= 128),
    harness_version TEXT NOT NULL DEFAULT '' CHECK (length(harness_version) <= 128),
    expected_transcript INTEGER NOT NULL CHECK (expected_transcript IN (0, 1)),
    expected_harness INTEGER NOT NULL CHECK (expected_harness IN (0, 1)),

    state TEXT NOT NULL DEFAULT 'reserved' CHECK (state IN (
        'reserved', 'running', 'quiescing', 'sealed', 'uploading',
        'complete', 'blocked', 'lost', 'waived'
    )),
    execution_verdict TEXT NOT NULL DEFAULT 'pending' CHECK (execution_verdict IN (
        'pending', 'succeeded', 'failed', 'cancelled', 'crashed'
    )),
    execution_exit_code INTEGER,
    execution_error_code TEXT NOT NULL DEFAULT '' CHECK (length(execution_error_code) <= 64),
    execution_recorded_at TEXT,

    upload_grant_hash TEXT NOT NULL UNIQUE
        CHECK (length(upload_grant_hash) = 64
            AND upload_grant_hash NOT GLOB '*[^0-9a-f]*'),
    upload_grant_generation INTEGER NOT NULL DEFAULT 1
        CHECK (upload_grant_generation >= 1),
    upload_grant_rotated_at TEXT,
    upload_grant_revoked_at TEXT,

    expected_set_declared_at TEXT,
    expected_final_artifact_count INTEGER CHECK (
        expected_final_artifact_count IS NULL OR expected_final_artifact_count >= 0
    ),
    expected_transcript_epoch INTEGER CHECK (
        expected_transcript_epoch IS NULL OR expected_transcript_epoch >= 0
    ),
    expected_transcript_segment_count INTEGER CHECK (
        expected_transcript_segment_count IS NULL OR expected_transcript_segment_count >= 0
    ),
    expected_transcript_length INTEGER CHECK (
        expected_transcript_length IS NULL OR expected_transcript_length >= 0
    ),
    expected_transcript_sha256 TEXT CHECK (
        expected_transcript_sha256 IS NULL OR (
            length(expected_transcript_sha256) = 64
            AND expected_transcript_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),

    last_hint_at TEXT,
    checkpoint_hint_count INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint_hint_count >= 0),
    checkpoint_coalesced_count INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint_coalesced_count >= 0),
    checkpoint_dirty_generation INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint_dirty_generation >= 0),
    last_checkpoint_attempt_generation INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_attempt_generation >= 0),
    last_checkpoint_committed_generation INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_committed_generation >= 0),
    last_checkpoint_bytes INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_bytes >= 0),
    last_checkpoint_entries INTEGER NOT NULL DEFAULT 0 CHECK (last_checkpoint_entries >= 0),

    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    error_message TEXT NOT NULL DEFAULT '' CHECK (length(error_message) <= 1024),
    waiver_reason TEXT NOT NULL DEFAULT '' CHECK (length(waiver_reason) <= 1024),
    zero_harness_root_reason TEXT NOT NULL DEFAULT '' CHECK (length(zero_harness_root_reason) <= 1024),
    version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
    reserved_at TEXT NOT NULL,
    running_at TEXT,
    quiescing_at TEXT,
    sealed_at TEXT,
    uploading_at TEXT,
    completed_at TEXT,
    blocked_at TEXT,
    lost_at TEXT,
    waived_at TEXT,
    updated_at TEXT NOT NULL,

    UNIQUE (job_id, lease_id),
    UNIQUE (job_id, lease_attempt),
    CHECK ((execution_verdict = 'pending') = (execution_recorded_at IS NULL)),
    CHECK (execution_verdict != 'succeeded' OR execution_exit_code IS NULL OR execution_exit_code = 0),
    CHECK ((expected_set_declared_at IS NULL) = (expected_final_artifact_count IS NULL)),
    CHECK (expected_set_declared_at IS NOT NULL OR (
        expected_transcript_epoch IS NULL
        AND expected_transcript_segment_count IS NULL
        AND expected_transcript_length IS NULL
        AND expected_transcript_sha256 IS NULL
    )),
    CHECK (state != 'running' OR running_at IS NOT NULL),
    CHECK (state != 'quiescing' OR quiescing_at IS NOT NULL),
    CHECK (state != 'sealed' OR sealed_at IS NOT NULL),
    CHECK (state != 'uploading' OR uploading_at IS NOT NULL),
    CHECK (state != 'complete' OR completed_at IS NOT NULL),
    CHECK (state != 'blocked' OR blocked_at IS NOT NULL),
    CHECK (state != 'lost' OR lost_at IS NOT NULL),
    CHECK (state != 'waived' OR (waived_at IS NOT NULL AND length(trim(waiver_reason)) > 0)),
    CHECK (state NOT IN ('complete', 'waived') OR upload_grant_revoked_at IS NOT NULL)
);

CREATE INDEX idx_history_captures_task
    ON history_captures(task_id, reserved_at DESC, id DESC) WHERE task_id <> '';
CREATE INDEX idx_history_captures_job
    ON history_captures(job_id, lease_attempt DESC);
CREATE INDEX idx_history_captures_session
    ON history_captures(session_id, reserved_at DESC) WHERE session_id <> '';
CREATE INDEX idx_history_captures_workflow
    ON history_captures(workflow_run_id, reserved_at DESC) WHERE workflow_run_id <> '';
CREATE INDEX idx_history_captures_pending
    ON history_captures(state, updated_at)
    WHERE state NOT IN ('complete', 'waived');

-- Snapshot attribution and expected coverage flags never change after reserve.
CREATE TRIGGER history_captures_immutable_attribution
BEFORE UPDATE ON history_captures
WHEN NEW.id IS NOT OLD.id
    OR NEW.project_id IS NOT OLD.project_id
    OR NEW.job_id IS NOT OLD.job_id
    OR NEW.lease_id IS NOT OLD.lease_id
    OR NEW.lease_attempt IS NOT OLD.lease_attempt
    OR NEW.worker_id IS NOT OLD.worker_id
    OR NEW.task_id IS NOT OLD.task_id
    OR NEW.session_id IS NOT OLD.session_id
    OR NEW.workflow_run_id IS NOT OLD.workflow_run_id
    OR NEW.node_run_id IS NOT OLD.node_run_id
    OR NEW.node_visit IS NOT OLD.node_visit
    OR NEW.stage IS NOT OLD.stage
    OR NEW.role IS NOT OLD.role
    OR NEW.harness_name IS NOT OLD.harness_name
    OR NEW.harness_version IS NOT OLD.harness_version
    OR NEW.expected_transcript IS NOT OLD.expected_transcript
    OR NEW.expected_harness IS NOT OLD.expected_harness
BEGIN
    SELECT RAISE(ABORT, 'history capture attribution is immutable');
END;

-- Upload grants may rotate only as one atomic generation step. The service
-- authenticates the producer identity before issuing this update; this trigger
-- prevents accidental hash replacement that omits the durable audit metadata.
CREATE TRIGGER history_captures_grant_rotation_guard
BEFORE UPDATE ON history_captures
WHEN NEW.upload_grant_hash IS NOT OLD.upload_grant_hash
    AND NOT (
        OLD.upload_grant_revoked_at IS NULL
        AND NEW.upload_grant_revoked_at IS NULL
        AND NEW.upload_grant_generation = OLD.upload_grant_generation + 1
        AND NEW.upload_grant_rotated_at IS NOT NULL
        AND NEW.upload_grant_rotated_at IS NOT OLD.upload_grant_rotated_at
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history upload grant rotation');
END;

CREATE TRIGGER history_captures_grant_generation_guard
BEFORE UPDATE ON history_captures
WHEN NEW.upload_grant_hash IS OLD.upload_grant_hash
    AND (
        NEW.upload_grant_generation IS NOT OLD.upload_grant_generation
        OR NEW.upload_grant_rotated_at IS NOT OLD.upload_grant_rotated_at
    )
BEGIN
    SELECT RAISE(ABORT, 'history upload grant generation requires rotation');
END;

CREATE TRIGGER history_captures_state_transition_guard
BEFORE UPDATE ON history_captures
WHEN NEW.state IS NOT OLD.state
    AND NOT (
        (OLD.state = 'reserved' AND NEW.state = 'running')
        OR (OLD.state = 'running' AND NEW.state = 'quiescing')
        OR (OLD.state = 'quiescing' AND NEW.state = 'sealed')
        OR (OLD.state = 'sealed' AND NEW.state = 'uploading')
        OR (NEW.state IN ('blocked', 'lost') AND OLD.state NOT IN ('complete', 'waived'))
        OR (OLD.state IN ('blocked', 'lost') AND NEW.state IN ('running', 'quiescing', 'sealed', 'uploading'))
        OR (NEW.state = 'complete' AND OLD.state IN ('uploading', 'sealed', 'blocked', 'lost'))
        OR (NEW.state = 'waived' AND OLD.state != 'complete')
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture state transition');
END;

CREATE TRIGGER history_captures_version_guard
BEFORE UPDATE ON history_captures
WHEN NEW.version < OLD.version
    OR NEW.version > OLD.version + 1
    OR (NEW.state IS NOT OLD.state AND NEW.version != OLD.version + 1)
    OR (NEW.execution_verdict IS NOT OLD.execution_verdict AND NEW.version != OLD.version + 1)
    OR (NEW.expected_set_declared_at IS NOT OLD.expected_set_declared_at AND NEW.version != OLD.version + 1)
    OR (NEW.upload_grant_hash IS NOT OLD.upload_grant_hash AND NEW.version != OLD.version + 1)
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture version transition');
END;

CREATE TRIGGER history_captures_execution_guard
BEFORE UPDATE ON history_captures
WHEN (NEW.execution_verdict IS NOT OLD.execution_verdict AND (
        OLD.execution_verdict != 'pending'
        OR NEW.execution_verdict = 'pending'
        OR NEW.execution_recorded_at IS NULL
    ))
    OR (NEW.execution_verdict IS OLD.execution_verdict AND (
        NEW.execution_exit_code IS NOT OLD.execution_exit_code
        OR NEW.execution_error_code IS NOT OLD.execution_error_code
        OR NEW.execution_recorded_at IS NOT OLD.execution_recorded_at
    ))
BEGIN
    SELECT RAISE(ABORT, 'invalid history execution verdict transition');
END;

CREATE TRIGGER history_captures_expected_set_guard
BEFORE UPDATE ON history_captures
WHEN OLD.expected_set_declared_at IS NOT NULL AND (
        NEW.expected_set_declared_at IS NOT OLD.expected_set_declared_at
        OR NEW.expected_final_artifact_count IS NOT OLD.expected_final_artifact_count
        OR NEW.expected_transcript_epoch IS NOT OLD.expected_transcript_epoch
        OR NEW.expected_transcript_segment_count IS NOT OLD.expected_transcript_segment_count
        OR NEW.expected_transcript_length IS NOT OLD.expected_transcript_length
        OR NEW.expected_transcript_sha256 IS NOT OLD.expected_transcript_sha256
        OR NEW.zero_harness_root_reason IS NOT OLD.zero_harness_root_reason
    )
BEGIN
    SELECT RAISE(ABORT, 'history capture expected set is immutable');
END;

CREATE TRIGGER history_captures_lifecycle_timestamp_guard
BEFORE UPDATE ON history_captures
WHEN (OLD.running_at IS NOT NULL AND NEW.running_at IS NOT OLD.running_at)
    OR (OLD.quiescing_at IS NOT NULL AND NEW.quiescing_at IS NOT OLD.quiescing_at)
    OR (OLD.sealed_at IS NOT NULL AND NEW.sealed_at IS NOT OLD.sealed_at)
    OR (OLD.uploading_at IS NOT NULL AND NEW.uploading_at IS NOT OLD.uploading_at)
    OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS NOT OLD.completed_at)
    OR (OLD.blocked_at IS NOT NULL AND NEW.blocked_at IS NOT OLD.blocked_at)
    OR (OLD.lost_at IS NOT NULL AND NEW.lost_at IS NOT OLD.lost_at)
    OR (OLD.waived_at IS NOT NULL AND NEW.waived_at IS NOT OLD.waived_at)
    OR (OLD.running_at IS NULL AND NEW.running_at IS NOT NULL AND NEW.state != 'running')
    OR (OLD.quiescing_at IS NULL AND NEW.quiescing_at IS NOT NULL AND NEW.state != 'quiescing')
    OR (OLD.sealed_at IS NULL AND NEW.sealed_at IS NOT NULL AND NEW.state != 'sealed')
    OR (OLD.uploading_at IS NULL AND NEW.uploading_at IS NOT NULL AND NEW.state != 'uploading')
    OR (OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL AND NEW.state != 'complete')
    OR (OLD.blocked_at IS NULL AND NEW.blocked_at IS NOT NULL AND NEW.state != 'blocked')
    OR (OLD.lost_at IS NULL AND NEW.lost_at IS NOT NULL AND NEW.state != 'lost')
    OR (OLD.waived_at IS NULL AND NEW.waived_at IS NOT NULL AND NEW.state != 'waived')
    OR (OLD.upload_grant_revoked_at IS NULL AND NEW.upload_grant_revoked_at IS NOT NULL
        AND NEW.state NOT IN ('complete', 'waived'))
    OR (NEW.state = 'running' AND NEW.running_at IS NULL)
    OR (NEW.state = 'quiescing' AND NEW.quiescing_at IS NULL)
    OR (NEW.state = 'sealed' AND NEW.sealed_at IS NULL)
    OR (NEW.state = 'uploading' AND NEW.uploading_at IS NULL)
    OR (NEW.state = 'complete' AND (NEW.completed_at IS NULL OR NEW.upload_grant_revoked_at IS NULL))
    OR (NEW.state = 'blocked' AND NEW.blocked_at IS NULL)
    OR (NEW.state = 'lost' AND NEW.lost_at IS NULL)
    OR (NEW.state = 'waived' AND (NEW.waived_at IS NULL OR NEW.upload_grant_revoked_at IS NULL))
BEGIN
    SELECT RAISE(ABORT, 'invalid history capture lifecycle timestamps');
END;

CREATE TRIGGER history_captures_no_delete
BEFORE DELETE ON history_captures
BEGIN
    SELECT RAISE(ABORT, 'history captures are retained');
END;

-- Exact final non-transcript artifact set declared at the seal boundary.
CREATE TABLE history_capture_expected_artifacts (
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    logical_key TEXT NOT NULL CHECK (length(trim(logical_key)) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('harness_root', 'manifest')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (capture_id, logical_key)
);

CREATE TRIGGER history_capture_expected_artifacts_no_update
BEFORE UPDATE ON history_capture_expected_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable');
END;
CREATE TRIGGER history_capture_expected_artifacts_no_delete
BEFORE DELETE ON history_capture_expected_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history capture expected artifacts are immutable');
END;

CREATE TABLE history_artifacts (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'ha-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    logical_key TEXT NOT NULL CHECK (length(trim(logical_key)) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('transcript_segment', 'harness_root', 'manifest')),
    phase TEXT NOT NULL CHECK (phase IN ('checkpoint', 'final')),
    checkpoint_generation INTEGER,
    checkpoint_stream TEXT NOT NULL DEFAULT '' CHECK (length(checkpoint_stream) <= 255),
    checkpoint_trigger TEXT NOT NULL DEFAULT '' CHECK (length(checkpoint_trigger) <= 64),
    archive_id TEXT NOT NULL DEFAULT '' CHECK (length(archive_id) <= 255),
    media_type TEXT NOT NULL CHECK (length(trim(media_type)) BETWEEN 1 AND 255),
    format_version INTEGER NOT NULL DEFAULT 1 CHECK (format_version > 0),
    schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    logical_size INTEGER NOT NULL CHECK (logical_size >= 0),
    entry_count INTEGER NOT NULL DEFAULT 0 CHECK (entry_count >= 0),
    publication_state TEXT NOT NULL CHECK (publication_state IN ('pending', 'committed')),
    temporary_upload_id TEXT NOT NULL DEFAULT '' CHECK (length(temporary_upload_id) <= 255),
    blob_key TEXT NOT NULL UNIQUE CHECK (length(blob_key) = 65),
    superseded_by_artifact_id TEXT REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    superseded_at TEXT,
    pending_at TEXT NOT NULL,
    committed_at TEXT,
    reconcile_attempted_at TEXT,
    created_at TEXT NOT NULL,

    UNIQUE (capture_id, logical_key),
    CHECK ((phase = 'checkpoint') = (checkpoint_generation IS NOT NULL)),
    CHECK (checkpoint_generation IS NULL OR checkpoint_generation > 0),
    CHECK ((phase = 'checkpoint' AND length(trim(checkpoint_stream)) > 0)
        OR (phase = 'final' AND checkpoint_stream = '')),
    CHECK (phase = 'checkpoint' OR checkpoint_trigger = ''),
    CHECK (kind != 'transcript_segment' OR (phase = 'final' AND checkpoint_generation IS NULL)),
    CHECK ((publication_state = 'committed') = (committed_at IS NOT NULL)),
    CHECK (publication_state != 'pending' OR length(temporary_upload_id) > 0),
    CHECK ((superseded_by_artifact_id IS NULL) = (superseded_at IS NULL)),
    CHECK (superseded_by_artifact_id IS NULL OR superseded_by_artifact_id != id)
);

CREATE INDEX idx_history_artifacts_capture_kind
    ON history_artifacts(capture_id, phase, kind, checkpoint_generation, logical_key);
CREATE UNIQUE INDEX idx_history_artifacts_checkpoint_stream_generation
    ON history_artifacts(capture_id, kind, checkpoint_stream, checkpoint_generation)
    WHERE phase = 'checkpoint';
CREATE INDEX idx_history_artifacts_pending
    ON history_artifacts(publication_state, reconcile_attempted_at, pending_at);
CREATE INDEX idx_history_artifacts_temporary
    ON history_artifacts(temporary_upload_id) WHERE publication_state = 'pending';

CREATE TRIGGER history_artifacts_immutable_metadata
BEFORE UPDATE ON history_artifacts
WHEN NEW.id IS NOT OLD.id
    OR NEW.capture_id IS NOT OLD.capture_id
    OR NEW.logical_key IS NOT OLD.logical_key
    OR NEW.kind IS NOT OLD.kind
    OR NEW.phase IS NOT OLD.phase
    OR NEW.checkpoint_generation IS NOT OLD.checkpoint_generation
    OR NEW.checkpoint_stream IS NOT OLD.checkpoint_stream
    OR NEW.checkpoint_trigger IS NOT OLD.checkpoint_trigger
    OR NEW.archive_id IS NOT OLD.archive_id
    OR NEW.media_type IS NOT OLD.media_type
    OR NEW.format_version IS NOT OLD.format_version
    OR NEW.schema_version IS NOT OLD.schema_version
    OR NEW.sha256 IS NOT OLD.sha256
    OR NEW.stored_size IS NOT OLD.stored_size
    OR NEW.logical_size IS NOT OLD.logical_size
    OR NEW.entry_count IS NOT OLD.entry_count
    OR NEW.temporary_upload_id IS NOT OLD.temporary_upload_id
    OR NEW.blob_key IS NOT OLD.blob_key
    OR NEW.pending_at IS NOT OLD.pending_at
    OR NEW.created_at IS NOT OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'history artifact metadata is immutable');
END;

CREATE TRIGGER history_artifacts_publication_guard
BEFORE UPDATE ON history_artifacts
WHEN NEW.publication_state IS NOT OLD.publication_state
    AND NOT (
        OLD.publication_state = 'pending'
        AND NEW.publication_state = 'committed'
        AND OLD.committed_at IS NULL
        AND NEW.committed_at IS NOT NULL
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact publication transition');
END;

CREATE TRIGGER history_artifacts_committed_at_guard
BEFORE UPDATE ON history_artifacts
WHEN NEW.publication_state IS OLD.publication_state
    AND NEW.committed_at IS NOT OLD.committed_at
BEGIN
    SELECT RAISE(ABORT, 'history artifact committed timestamp is immutable');
END;

CREATE TRIGGER history_artifacts_supersession_guard
BEFORE UPDATE ON history_artifacts
WHEN (OLD.superseded_by_artifact_id IS NOT NULL AND (
        NEW.superseded_by_artifact_id IS NULL
        OR NEW.superseded_at IS NULL
        OR ((NEW.superseded_by_artifact_id IS NOT OLD.superseded_by_artifact_id
                OR NEW.superseded_at IS NOT OLD.superseded_at)
            AND NOT EXISTS (
                SELECT 1 FROM history_artifacts final_manifest
                WHERE final_manifest.id = NEW.superseded_by_artifact_id
                  AND final_manifest.capture_id = NEW.capture_id
                  AND final_manifest.kind = 'manifest'
                  AND final_manifest.phase = 'final'
                  AND final_manifest.publication_state = 'committed'
            ))
    ))
    OR (OLD.superseded_by_artifact_id IS NULL AND NEW.superseded_by_artifact_id IS NOT NULL
        AND NEW.superseded_at IS NULL)
    OR (OLD.superseded_by_artifact_id IS NULL AND NEW.superseded_by_artifact_id IS NULL
        AND NEW.superseded_at IS NOT OLD.superseded_at)
BEGIN
    SELECT RAISE(ABORT, 'invalid history artifact supersession transition');
END;

CREATE TRIGGER history_artifacts_no_delete
BEFORE DELETE ON history_artifacts
BEGIN
    SELECT RAISE(ABORT, 'history artifacts are retained');
END;

-- One raw stream per capture. The service enforces contiguous epoch/sequence and
-- offset ordering inside its BEGIN IMMEDIATE registration transaction.
CREATE TABLE history_transcript_streams (
    capture_id TEXT PRIMARY KEY REFERENCES history_captures(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'sealed')),
    segment_count INTEGER NOT NULL DEFAULT 0 CHECK (segment_count >= 0),
    logical_length INTEGER NOT NULL DEFAULT 0 CHECK (logical_length >= 0),
    last_epoch INTEGER,
    last_sequence INTEGER,
    stream_sha256 TEXT CHECK (
        stream_sha256 IS NULL OR (length(stream_sha256) = 64 AND stream_sha256 NOT GLOB '*[^0-9a-f]*')
    ),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    sealed_at TEXT,
    CHECK ((state = 'sealed') = (sealed_at IS NOT NULL)),
    CHECK ((segment_count = 0) = (last_epoch IS NULL AND last_sequence IS NULL)),
    CHECK ((state = 'sealed') = (stream_sha256 IS NOT NULL))
);

CREATE TABLE history_transcript_segments (
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    epoch INTEGER NOT NULL CHECK (epoch >= 0),
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    start_offset INTEGER NOT NULL CHECK (start_offset >= 0),
    end_offset INTEGER NOT NULL CHECK (end_offset > start_offset),
    uncompressed_size INTEGER NOT NULL CHECK (uncompressed_size > 0),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    raw_sha256 TEXT NOT NULL CHECK (length(raw_sha256) = 64 AND raw_sha256 NOT GLOB '*[^0-9a-f]*'),
    encoding TEXT NOT NULL CHECK (encoding IN ('identity', 'gzip')),
    artifact_id TEXT NOT NULL UNIQUE REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    sealed_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (capture_id, epoch, sequence),
    UNIQUE (capture_id, start_offset),
    UNIQUE (capture_id, end_offset),
    CHECK (uncompressed_size = end_offset - start_offset)
);

CREATE INDEX idx_history_transcript_segments_offsets
    ON history_transcript_segments(capture_id, start_offset, end_offset);

CREATE TRIGGER history_transcript_segments_no_update
BEFORE UPDATE ON history_transcript_segments
BEGIN
    SELECT RAISE(ABORT, 'history transcript segments are immutable');
END;
CREATE TRIGGER history_transcript_segments_no_delete
BEFORE DELETE ON history_transcript_segments
BEGIN
    SELECT RAISE(ABORT, 'history transcript segments are immutable');
END;
CREATE TRIGGER history_transcript_streams_no_reopen
BEFORE UPDATE ON history_transcript_streams
WHEN OLD.state = 'sealed' AND (
    NEW.state IS NOT OLD.state
    OR NEW.segment_count IS NOT OLD.segment_count
    OR NEW.logical_length IS NOT OLD.logical_length
    OR NEW.last_epoch IS NOT OLD.last_epoch
    OR NEW.last_sequence IS NOT OLD.last_sequence
    OR NEW.stream_sha256 IS NOT OLD.stream_sha256
    OR NEW.sealed_at IS NOT OLD.sealed_at
)
BEGIN
    SELECT RAISE(ABORT, 'sealed history transcript stream is immutable');
END;

-- Bounded index into an opaque Harness archive; it is attribution metadata, not
-- extracted archive content.
CREATE TABLE harness_archive_members (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'hm-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    artifact_id TEXT NOT NULL REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    archive_id TEXT NOT NULL CHECK (length(trim(archive_id)) BETWEEN 1 AND 255),
    native_session_id TEXT NOT NULL DEFAULT '' CHECK (length(native_session_id) <= 255),
    native_parent_session_id TEXT NOT NULL DEFAULT '' CHECK (length(native_parent_session_id) <= 255),
    relative_member_path TEXT NOT NULL CHECK (length(trim(relative_member_path)) BETWEEN 1 AND 4096),
    member_kind TEXT NOT NULL CHECK (member_kind IN ('root', 'delegated_child')),
    agent_name TEXT NOT NULL DEFAULT '' CHECK (length(agent_name) <= 255),
    status TEXT NOT NULL DEFAULT '' CHECK (length(status) <= 128),
    model TEXT NOT NULL DEFAULT '' CHECK (length(model) <= 255),
    harness_build TEXT NOT NULL DEFAULT '' CHECK (length(harness_build) <= 255),
    parse_status TEXT NOT NULL CHECK (parse_status IN ('parsed', 'partial', 'missing', 'invalid', 'unsupported')),
    created_at TEXT NOT NULL,
    UNIQUE (capture_id, archive_id, relative_member_path),
    UNIQUE (artifact_id, relative_member_path)
);

CREATE INDEX idx_harness_archive_members_native_session
    ON harness_archive_members(capture_id, native_session_id)
    WHERE native_session_id <> '';

-- Completed private uploads are registered before they leave the service. This
-- keeps active/outboxed temporaries in the reconciliation protection set even
-- before an artifact publication row exists.
CREATE TABLE history_upload_intents (
    temporary_upload_id TEXT PRIMARY KEY
        CHECK (length(temporary_upload_id) = 32
            AND temporary_upload_id NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    stored_size INTEGER NOT NULL CHECK (stored_size >= 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'consumed', 'abandoned')),
    artifact_id TEXT UNIQUE REFERENCES history_artifacts(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    consumed_at TEXT,
    abandoned_at TEXT,
    CHECK (
        (state = 'active' AND artifact_id IS NULL AND consumed_at IS NULL AND abandoned_at IS NULL)
        OR (state = 'consumed' AND artifact_id IS NOT NULL AND consumed_at IS NOT NULL AND abandoned_at IS NULL)
        OR (state = 'abandoned' AND artifact_id IS NULL AND consumed_at IS NULL AND abandoned_at IS NOT NULL)
    )
);

CREATE INDEX idx_history_upload_intents_capture_state
    ON history_upload_intents(capture_id, state, heartbeat_at);
CREATE INDEX idx_history_upload_intents_active_heartbeat
    ON history_upload_intents(heartbeat_at) WHERE state = 'active';

CREATE TRIGGER history_upload_intents_update_guard
BEFORE UPDATE ON history_upload_intents
WHEN NEW.temporary_upload_id IS NOT OLD.temporary_upload_id
    OR NEW.capture_id IS NOT OLD.capture_id
    OR NEW.sha256 IS NOT OLD.sha256
    OR NEW.stored_size IS NOT OLD.stored_size
    OR NEW.created_at IS NOT OLD.created_at
    OR OLD.state != 'active'
    OR NOT (
        (NEW.state = 'active'
            AND NEW.artifact_id IS NULL
            AND NEW.consumed_at IS NULL
            AND NEW.abandoned_at IS NULL
            AND NEW.heartbeat_at >= OLD.heartbeat_at)
        OR (NEW.state = 'consumed'
            AND NEW.artifact_id IS NOT NULL
            AND NEW.consumed_at IS NOT NULL
            AND NEW.abandoned_at IS NULL)
        OR (NEW.state = 'abandoned'
            AND NEW.artifact_id IS NULL
            AND NEW.consumed_at IS NULL
            AND NEW.abandoned_at IS NOT NULL)
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid history upload intent transition');
END;

CREATE TRIGGER history_upload_intents_no_delete
BEFORE DELETE ON history_upload_intents
BEGIN
    SELECT RAISE(ABORT, 'history upload intents are retained');
END;

-- One aggregate per capture/source event absorbs abusive hook bursts. Counts and
-- the capture projection retain useful observability without one row per hint.
CREATE TABLE history_checkpoint_hints (
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    source_event TEXT NOT NULL CHECK (length(trim(source_event)) BETWEEN 1 AND 64),
    first_received_at TEXT NOT NULL,
    last_received_at TEXT NOT NULL,
    hint_count INTEGER NOT NULL CHECK (hint_count > 0),
    coalesced_count INTEGER NOT NULL DEFAULT 0 CHECK (coalesced_count >= 0 AND coalesced_count <= hint_count),
    dirty_generation INTEGER NOT NULL DEFAULT 0 CHECK (dirty_generation >= 0),
    worker_outcome TEXT NOT NULL CHECK (worker_outcome IN (
        'pending', 'coalesced', 'segment_requested', 'checkpoint_started',
        'checkpoint_committed', 'failed', 'ignored'
    )),
    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
    PRIMARY KEY (capture_id, source_event)
);

-- Capture/upload events are audit feeds. Triggers make append-only a database
-- invariant even if a future caller bypasses the coordinator service.
CREATE TABLE history_capture_events (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'he-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    event_kind TEXT NOT NULL CHECK (length(trim(event_kind)) BETWEEN 1 AND 64),
    from_state TEXT NOT NULL DEFAULT '' CHECK (length(from_state) <= 32),
    to_state TEXT NOT NULL DEFAULT '' CHECK (length(to_state) <= 32),
    capture_version INTEGER NOT NULL CHECK (capture_version >= 0),
    actor TEXT NOT NULL CHECK (length(trim(actor)) BETWEEN 1 AND 255),
    code TEXT NOT NULL DEFAULT '' CHECK (length(code) <= 64),
    details_json TEXT NOT NULL DEFAULT '{}' CHECK (length(details_json) <= 4096 AND json_valid(details_json)),
    occurred_at TEXT NOT NULL
);

CREATE INDEX idx_history_capture_events_feed
    ON history_capture_events(capture_id, occurred_at, id);

CREATE TABLE history_upload_events (
    id TEXT PRIMARY KEY
        CHECK (length(id) = 35 AND substr(id, 1, 3) = 'hu-'
            AND substr(id, 4) NOT GLOB '*[^0-9a-f]*'),
    capture_id TEXT NOT NULL REFERENCES history_captures(id) ON DELETE RESTRICT,
    artifact_id TEXT NOT NULL DEFAULT '' CHECK (length(artifact_id) <= 255),
    logical_key TEXT NOT NULL DEFAULT '' CHECK (length(logical_key) <= 512),
    event_kind TEXT NOT NULL CHECK (event_kind IN (
        'pending', 'publish_started', 'published', 'committed',
        'reconciled', 'failed', 'conflict'
    )),
    attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt > 0),
    error_code TEXT NOT NULL DEFAULT '' CHECK (length(error_code) <= 64),
    details_json TEXT NOT NULL DEFAULT '{}' CHECK (length(details_json) <= 4096 AND json_valid(details_json)),
    occurred_at TEXT NOT NULL
);

CREATE INDEX idx_history_upload_events_feed
    ON history_upload_events(capture_id, occurred_at, id);
CREATE INDEX idx_history_upload_events_artifact
    ON history_upload_events(artifact_id, occurred_at) WHERE artifact_id <> '';

CREATE TRIGGER history_capture_events_no_update
BEFORE UPDATE ON history_capture_events
BEGIN
    SELECT RAISE(ABORT, 'history capture events are append-only');
END;
CREATE TRIGGER history_capture_events_no_delete
BEFORE DELETE ON history_capture_events
BEGIN
    SELECT RAISE(ABORT, 'history capture events are append-only');
END;
CREATE TRIGGER history_upload_events_no_update
BEFORE UPDATE ON history_upload_events
BEGIN
    SELECT RAISE(ABORT, 'history upload events are append-only');
END;
CREATE TRIGGER history_upload_events_no_delete
BEFORE DELETE ON history_upload_events
BEGIN
    SELECT RAISE(ABORT, 'history upload events are append-only');
END;

UPDATE app_metadata
SET value = '0007_history_captures',
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE key = 'schema_version';
