// Package historycapture implements the durable worker-side history outbox and
// the protocol-7 history publisher.
package historycapture

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
)

const (
	stateFormatVersion = 1
	stateFileName      = "state.json"
	workspaceFileName  = "workspace.tar.gz"
	harnessFileName    = "harness.tar.gz"

	statusReserved  = "reserved"
	statusFinalized = "finalized"
	statusStaged    = "staged"
	statusComplete  = "complete"
)

var (
	ErrInvalidOutbox = errors.New("invalid history capture outbox")
	ErrLimitExceeded = errors.New("history capture outbox limit exceeded")
	ErrNotFound      = errors.New("history capture outbox entry not found")
	ErrNotStaged     = errors.New("history capture has not been staged")
)

// Options bounds local durable history data. MaxOutstandingEntries counts
// non-complete captures, while MaxOutstandingBytes counts their staged payload
// bytes. Completed entries are retained but do not consume those limits.
type Options struct {
	Dir                   string
	SegmentBytes          int64
	ArchiveLimits         historyarchive.Limits
	MaxOutstandingBytes   int64
	MaxOutstandingEntries int
	// SensitiveDataKey is ephemeral key material used to derive the in-memory
	// AES-GCM key for durable reservation-time sensitive values and terminal
	// credentials. It is never retained in Options or persisted.
	SensitiveDataKey []byte
}

// CompletedRetention deterministically bounds scrubbed completion tombstones.
// MaxEntries must be positive. CompletedBefore optionally removes tombstones
// whose coordinator completion timestamp is older than the cutoff in addition
// to enforcing MaxEntries.
type CompletedRetention struct {
	MaxEntries      int
	CompletedBefore time.Time
}

// OptionsFromConfig maps the coordinator-compatible resolved history limits to
// worker outbox options. MaxOutstandingUploads is used as the capture-count
// bound; callers may override it when constructing Options directly.
func OptionsFromConfig(dir string, transcript config.ResolvedHistoryTranscript, archive config.ResolvedHistoryArchive) Options {
	return Options{
		Dir:          dir,
		SegmentBytes: transcript.SegmentBytes,
		ArchiveLimits: historyarchive.Limits{
			MaxStoredBytes: archive.MaxStoredBytes, MaxLogicalBytes: archive.MaxLogicalBytes,
			MaxFileBytes: archive.MaxFileBytes, MaxEntries: archive.MaxEntries, MaxPathBytes: archive.MaxPathBytes,
		},
		MaxOutstandingBytes: archive.MaxOutstandingBytes, MaxOutstandingEntries: archive.MaxOutstandingUploads,
	}
}

// Client is exactly the protocol-7 worker history surface used by Publisher.
// *client.Client satisfies this interface.
type Client interface {
	UploadHistoryArtifactBytes(context.Context, string, string, io.Reader) (contract.HistoryUploadResponse, error)
	AbandonHistoryArtifactUpload(context.Context, string, string, string) error
	PublishHistoryArtifact(context.Context, string, string, contract.PublishHistoryArtifactRequest) (contract.HistoryArtifact, error)
	RegisterHistoryTranscriptSegment(context.Context, string, string, contract.RegisterHistoryTranscriptSegmentRequest) error
	SealHistoryTranscript(context.Context, string, string, contract.HistoryTranscriptSeal) error
	DeclareHistoryExpectedSet(context.Context, string, string, contract.DeclareHistoryExpectedSetRequest) (contract.HistoryCapture, error)
	RegisterHistoryWorkspaceSummary(context.Context, string, string, contract.RegisterHistoryWorkspaceSummaryRequest) (contract.HistoryWorkspaceSummary, error)
	RegisterHistoryHarnessMembers(context.Context, string, string, contract.RegisterHistoryHarnessMembersRequest) error
	RecordHistoryExecutionVerdict(context.Context, string, string, contract.RecordHistoryExecutionVerdictRequest) (contract.HistoryCapture, error)
	TransitionHistoryCapture(context.Context, string, string, contract.TransitionHistoryCaptureRequest) (contract.HistoryCapture, error)
	GenerateHistoryManifest(context.Context, string, string) (contract.HistoryArtifact, error)
	CompleteHistoryCapture(context.Context, string, string, int64) (contract.HistoryCapture, error)
}

// SourcePaths are captured immediately after reservation. NativeSessionRoot is
// an explicit Flow-managed Harness output root, never a parent session store.
type SourcePaths struct {
	Worktree          string `json:"worktree"`
	Transcript        string `json:"transcript"`
	NativeSessionRoot string `json:"native_session_root,omitempty"`
	NativeSessionID   string `json:"native_session_id,omitempty"`
}

// Reservation is recorded immediately after ReserveHistoryCapture returns.
type Reservation struct {
	Response contract.ReserveHistoryCaptureResponse
	Sources  SourcePaths
	// SensitiveValues are normalized, encrypted, and retained only until staging
	// succeeds so restart recovery can scan attempt-specific credentials.
	SensitiveValues [][]byte
}

// Final records the immutable execution result staged for publication.
type Final struct {
	Verdict          string
	ExitCode         *int
	ErrorCode        string
	WorkspaceBaseRef string
	// Terminal is durably checkpointed with the execution verdict so replay can
	// synchronize the job/session lifecycle only after capture completion.
	Terminal *TerminalAction
	// SensitiveValues are scanned by archive writers but are never persisted.
	SensitiveValues [][]byte
}

const (
	TerminalReleaseLease = "release_lease"
	TerminalSessionExit  = "session_process_exit"
	TerminalConsoleExit  = "release_console"
)

// TerminalAction is the post-publication coordinator mutation for a captured
// attempt. SessionToken is ephemeral and is never serialized; the outbox keeps
// console credentials in a separate authenticated-encryption envelope.
type TerminalAction struct {
	Kind         string `json:"kind"`
	FinalState   string `json:"final_state,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	SessionToken string `json:"-"`
	ExitCode     int    `json:"exit_code,omitempty"`
}

// Entry is a copy of the durable state suitable for integration and tests. The
// upload grant is intentionally included because replay requires it; callers
// must treat Entry as secret-bearing and must not log it.
type Entry struct {
	FormatVersion                int                                              `json:"format_version"`
	Protocol                     string                                           `json:"protocol"`
	Status                       string                                           `json:"status"`
	Capture                      contract.HistoryCapture                          `json:"capture"`
	UploadGrant                  string                                           `json:"upload_grant"`
	RemoteVersion                int64                                            `json:"remote_version"`
	RemoteState                  string                                           `json:"remote_state"`
	Sources                      SourcePaths                                      `json:"sources"`
	SensitiveValuesCiphertext    *sensitiveValuesCiphertext                       `json:"sensitive_values_ciphertext,omitempty"`
	TerminalCredentialCiphertext *terminalCredentialCiphertext                    `json:"terminal_credential_ciphertext,omitempty"`
	Final                        stagedFinal                                      `json:"final"`
	Terminal                     *TerminalAction                                  `json:"terminal,omitempty"`
	Artifacts                    []artifactRecord                                 `json:"artifacts,omitempty"`
	TranscriptSeal               *contract.HistoryTranscriptSeal                  `json:"transcript_seal,omitempty"`
	WorkspaceSummary             *contract.RegisterHistoryWorkspaceSummaryRequest `json:"workspace_summary,omitempty"`
	HarnessMembers               *contract.RegisterHistoryHarnessMembersRequest   `json:"harness_members,omitempty"`
	ExpectedSet                  *contract.DeclareHistoryExpectedSetRequest       `json:"expected_set,omitempty"`
	Progress                     publishProgress                                  `json:"progress"`
}

type sensitiveValuesCiphertext struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type terminalCredentialCiphertext struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type stagedFinal struct {
	Verdict          string `json:"verdict,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	WorkspaceBaseRef string `json:"workspace_base_ref,omitempty"`
}

type artifactRecord struct {
	Path                string                                            `json:"path"`
	SHA256              string                                            `json:"sha256"`
	StoredSize          int64                                             `json:"stored_size"`
	Publish             contract.PublishHistoryArtifactRequest            `json:"publish"`
	Segment             *contract.RegisterHistoryTranscriptSegmentRequest `json:"segment,omitempty"`
	TemporaryUploadID   string                                            `json:"temporary_upload_id,omitempty"`
	PublishedArtifactID string                                            `json:"published_artifact_id,omitempty"`
	Registered          bool                                              `json:"registered"`
}

type publishProgress struct {
	TranscriptSealed  bool `json:"transcript_sealed"`
	ExpectedDeclared  bool `json:"expected_declared"`
	VerdictRecorded   bool `json:"verdict_recorded"`
	ManifestGenerated bool `json:"manifest_generated"`
	Completed         bool `json:"completed"`
}
