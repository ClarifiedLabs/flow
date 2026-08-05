package contract

import "encoding/json"

// HistoryCapture is the owner-visible immutable attribution and mutable capture
// state projection. It intentionally contains no upload grant, blob key,
// temporary upload identifier, or internal filesystem path.
type HistoryCapture struct {
	ID                   string `json:"id"`
	ProjectID            string `json:"project_id"`
	JobID                string `json:"job_id"`
	LeaseID              string `json:"lease_id"`
	LeaseAttempt         int64  `json:"lease_attempt"`
	WorkerID             string `json:"worker_id"`
	TaskID               string `json:"task_id,omitempty"`
	ChangeID             string `json:"change_id,omitempty"`
	SessionID            string `json:"session_id,omitempty"`
	WorkflowRunID        string `json:"workflow_run_id,omitempty"`
	NodeRunID            string `json:"node_run_id,omitempty"`
	NodeVisit            int64  `json:"node_visit,omitempty"`
	Stage                string `json:"stage,omitempty"`
	Role                 string `json:"role"`
	HarnessName          string `json:"harness_name,omitempty"`
	HarnessVersion       string `json:"harness_version,omitempty"`
	HarnessSchemaVersion int    `json:"harness_schema_version,omitempty"`

	ResumedFromCaptureID        string `json:"resumed_from_capture_id,omitempty"`
	ResumedFromHarnessSessionID string `json:"resumed_from_harness_session_id,omitempty"`

	ExpectedTranscript bool   `json:"expected_transcript"`
	ExpectedHarness    bool   `json:"expected_harness"`
	State              string `json:"state"`
	ExecutionVerdict   string `json:"execution_verdict"`
	ExecutionExitCode  *int   `json:"execution_exit_code,omitempty"`
	ExecutionErrorCode string `json:"execution_error_code,omitempty"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	WaiverReason string `json:"waiver_reason,omitempty"`
	Version      int64  `json:"version"`
	Resumable    bool   `json:"resumable"`
	ReservedAt   string `json:"reserved_at"`
	UpdatedAt    string `json:"updated_at"`
	CompletedAt  string `json:"completed_at,omitempty"`
}

type HistoryArtifact struct {
	ID                     string `json:"id"`
	CaptureID              string `json:"capture_id"`
	LogicalKey             string `json:"logical_key"`
	Kind                   string `json:"kind"`
	Phase                  string `json:"phase"`
	CheckpointGeneration   int64  `json:"checkpoint_generation,omitempty"`
	CheckpointTrigger      string `json:"checkpoint_trigger,omitempty"`
	CheckpointStream       string `json:"checkpoint_stream,omitempty"`
	ArchiveID              string `json:"archive_id,omitempty"`
	MediaType              string `json:"media_type"`
	FormatVersion          int    `json:"format_version"`
	SchemaVersion          int    `json:"schema_version"`
	SHA256                 string `json:"sha256"`
	StoredSize             int64  `json:"stored_size"`
	LogicalSize            int64  `json:"logical_size"`
	EntryCount             int64  `json:"entry_count"`
	PublicationState       string `json:"publication_state"`
	SupersededByArtifactID string `json:"superseded_by_artifact_id,omitempty"`
	CommittedAt            string `json:"committed_at,omitempty"`
	CreatedAt              string `json:"created_at"`
	ContentPath            string `json:"content_path,omitempty"`
}

type HistoryHarnessMember struct {
	ArtifactID            string `json:"artifact_id"`
	ArchiveID             string `json:"archive_id"`
	NativeSessionID       string `json:"native_session_id,omitempty"`
	NativeParentSessionID string `json:"native_parent_session_id,omitempty"`
	RelativeMemberPath    string `json:"relative_member_path"`
	MemberKind            string `json:"member_kind"`
	AgentName             string `json:"agent_name,omitempty"`
	Status                string `json:"status,omitempty"`
	Model                 string `json:"model,omitempty"`
	HarnessBuild          string `json:"harness_build,omitempty"`
	ParseStatus           string `json:"parse_status"`
}

type HistoryWorkspaceSummary struct {
	ArtifactID           string `json:"artifact_id"`
	ArchiveSchemaVersion int    `json:"archive_schema_version"`
	Branch               string `json:"branch,omitempty"`
	Detached             bool   `json:"detached"`
	BaseRef              string `json:"base_ref,omitempty"`
	BaseCommit           string `json:"base_commit,omitempty"`
	HeadCommit           string `json:"head_commit"`
	StagedCount          int64  `json:"staged_count"`
	UnstagedCount        int64  `json:"unstaged_count"`
	UntrackedCount       int64  `json:"untracked_count"`
	InventoryDigest      string `json:"inventory_digest"`
	ValidationStatus     string `json:"validation_status"`
}

type HistoryAvailability struct {
	Total     int `json:"total"`
	Complete  int `json:"complete"`
	Resumable int `json:"resumable"`
	Blocked   int `json:"blocked"`
	Lost      int `json:"lost"`
	Waived    int `json:"waived"`
}

type HistoryCapturesResponse struct {
	SnapshotUntil string              `json:"snapshot_until"`
	Captures      []HistoryCapture    `json:"captures"`
	NextCursor    string              `json:"next_cursor,omitempty"`
	Availability  HistoryAvailability `json:"availability"`
}

type HistoryCaptureResponse struct {
	Capture          HistoryCapture           `json:"capture"`
	Artifacts        []HistoryArtifact        `json:"artifacts"`
	HarnessMembers   []HistoryHarnessMember   `json:"harness_members"`
	WorkspaceSummary *HistoryWorkspaceSummary `json:"workspace_summary,omitempty"`
}

type HistoryManifestResponse struct {
	Format        string                   `json:"format"`
	SchemaVersion int                      `json:"schema_version"`
	Capture       HistoryCapture           `json:"capture"`
	Artifacts     []HistoryArtifact        `json:"artifacts"`
	Harness       []HistoryHarnessMember   `json:"harness_members"`
	Workspace     *HistoryWorkspaceSummary `json:"workspace_summary,omitempty"`
}

type HistoryCaptureEvent struct {
	ID             string          `json:"id"`
	CaptureID      string          `json:"capture_id"`
	EventKind      string          `json:"event_kind"`
	FromState      string          `json:"from_state,omitempty"`
	ToState        string          `json:"to_state,omitempty"`
	CaptureVersion int64           `json:"capture_version"`
	Actor          string          `json:"actor"`
	Code           string          `json:"code,omitempty"`
	Details        json.RawMessage `json:"details"`
	OccurredAt     string          `json:"occurred_at"`
}

type HistoryCaptureEventsResponse struct {
	Events []HistoryCaptureEvent `json:"events"`
}

type WaiveHistoryCaptureRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}

type RevokeHistoryUploadGrantRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}

type ResumeHistoryCaptureRequest struct {
	NativeSessionID string `json:"native_session_id,omitempty"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type ResumeHistoryCaptureResponse struct {
	ID                    string `json:"id"`
	SourceCaptureID       string `json:"source_capture_id"`
	SourceNativeSessionID string `json:"source_native_session_id"`
	JobID                 string `json:"job_id"`
	State                 string `json:"state"`
	RequiredHeadCommit    string `json:"required_head_commit"`
	SourceHarnessBuild    string `json:"source_harness_build"`
	RequiredHarnessSchema int    `json:"required_harness_schema_version"`
	Created               bool   `json:"created"`
}

// Worker history publication contracts use the worker credential for identity
// and the per-capture upload grant (Flow-History-Upload-Grant) as a revocable
// capability. Grants are returned only by reservation and never by owner reads.
type ReserveHistoryCaptureRequest struct {
	JobID                       string `json:"job_id"`
	LeaseID                     string `json:"lease_id"`
	SessionID                   string `json:"session_id,omitempty"`
	NodeVisit                   int64  `json:"node_visit,omitempty"`
	Stage                       string `json:"stage,omitempty"`
	HarnessName                 string `json:"harness_name,omitempty"`
	HarnessVersion              string `json:"harness_version,omitempty"`
	HarnessSchemaVersion        int    `json:"harness_schema_version,omitempty"`
	ResumedFromCaptureID        string `json:"resumed_from_capture_id,omitempty"`
	ResumedFromHarnessSessionID string `json:"resumed_from_harness_session_id,omitempty"`
	ExpectedTranscript          bool   `json:"expected_transcript"`
	ExpectedHarness             bool   `json:"expected_harness"`
}

type ReserveHistoryCaptureResponse struct {
	Capture      HistoryCapture `json:"capture"`
	UploadGrant  string         `json:"upload_grant"`
	Created      bool           `json:"created"`
	GrantRotated bool           `json:"grant_rotated"`
}

type HistoryUploadResponse struct {
	TemporaryUploadID string `json:"temporary_upload_id"`
	SHA256            string `json:"sha256"`
	StoredSize        int64  `json:"stored_size"`
}

type PublishHistoryArtifactRequest struct {
	TemporaryUploadID    string `json:"temporary_upload_id"`
	LogicalKey           string `json:"logical_key"`
	Kind                 string `json:"kind"`
	Phase                string `json:"phase"`
	CheckpointGeneration int64  `json:"checkpoint_generation,omitempty"`
	CheckpointTrigger    string `json:"checkpoint_trigger,omitempty"`
	CheckpointStream     string `json:"checkpoint_stream,omitempty"`
	ArchiveID            string `json:"archive_id,omitempty"`
	MediaType            string `json:"media_type"`
	FormatVersion        int    `json:"format_version"`
	SchemaVersion        int    `json:"schema_version"`
	LogicalSize          int64  `json:"logical_size"`
	EntryCount           int64  `json:"entry_count"`
}

type RegisterHistoryTranscriptSegmentRequest struct {
	ArtifactLogicalKey string `json:"artifact_logical_key"`
	Epoch              int64  `json:"epoch"`
	Sequence           int64  `json:"sequence"`
	StartOffset        int64  `json:"start_offset"`
	EndOffset          int64  `json:"end_offset"`
	UncompressedSize   int64  `json:"uncompressed_size"`
	Encoding           string `json:"encoding"`
}

type HistoryTranscriptSeal struct {
	FinalEpoch    int64  `json:"final_epoch"`
	SegmentCount  int64  `json:"segment_count"`
	LogicalLength int64  `json:"logical_length"`
	SHA256        string `json:"sha256"`
}

type HistoryFinalArtifactExpectation struct {
	LogicalKey string `json:"logical_key"`
	Kind       string `json:"kind"`
}

type DeclareHistoryExpectedSetRequest struct {
	Artifacts       []HistoryFinalArtifactExpectation `json:"artifacts"`
	TranscriptSeal  *HistoryTranscriptSeal            `json:"transcript_seal"`
	ExpectedVersion int64                             `json:"expected_version"`
}

type RegisterHistoryWorkspaceSummaryRequest struct {
	ArtifactLogicalKey   string `json:"artifact_logical_key"`
	ArchiveSchemaVersion int    `json:"archive_schema_version"`
	Branch               string `json:"branch,omitempty"`
	Detached             bool   `json:"detached"`
	BaseRef              string `json:"base_ref,omitempty"`
	BaseCommit           string `json:"base_commit,omitempty"`
	HeadCommit           string `json:"head_commit"`
	StagedCount          int64  `json:"staged_count"`
	UnstagedCount        int64  `json:"unstaged_count"`
	UntrackedCount       int64  `json:"untracked_count"`
	InventoryDigest      string `json:"inventory_digest"`
	ValidationStatus     string `json:"validation_status"`
}

type HistoryHarnessMemberInput struct {
	NativeSessionID       string `json:"native_session_id,omitempty"`
	NativeParentSessionID string `json:"native_parent_session_id,omitempty"`
	RelativeMemberPath    string `json:"relative_member_path"`
	MemberKind            string `json:"member_kind"`
	AgentName             string `json:"agent_name,omitempty"`
	Status                string `json:"status,omitempty"`
	Model                 string `json:"model,omitempty"`
	HarnessBuild          string `json:"harness_build,omitempty"`
	ParseStatus           string `json:"parse_status"`
}

type RegisterHistoryHarnessMembersRequest struct {
	ArtifactLogicalKey string                      `json:"artifact_logical_key"`
	Members            []HistoryHarnessMemberInput `json:"members"`
}

type RecordHistoryExecutionVerdictRequest struct {
	Verdict         string `json:"verdict"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	ExpectedVersion int64  `json:"expected_version"`
}

type TransitionHistoryCaptureRequest struct {
	To              string `json:"to"`
	ExpectedVersion int64  `json:"expected_version"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
}

type CompleteHistoryCaptureRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
}
