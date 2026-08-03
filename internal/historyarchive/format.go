// Package historyarchive implements deterministic, bounded history archives.
package historyarchive

import (
	"errors"
	"fmt"
)

const (
	HarnessManifestName          = "harness-manifest.json"
	WorkspaceManifestName        = "workspace-manifest.json"
	HarnessFormatVersion         = 1
	HarnessSchemaVersion         = 1
	WorkspaceFormatVersion       = 1
	WorkspaceSchemaVersion       = 1
	SupportedHarnessNativeSchema = 5
)

var (
	ErrLimitExceeded     = errors.New("history archive limit exceeded")
	ErrInvalidArchive    = errors.New("invalid history archive")
	ErrUnsafePath        = errors.New("unsafe archive path")
	ErrDigestMismatch    = errors.New("history archive digest mismatch")
	ErrUnsupported       = errors.New("unsupported history archive")
	ErrSensitiveContent  = errors.New("sensitive content in history archive")
	ErrDestinationExists = errors.New("history archive destination exists")
)

// Limits are enforced during capture, inspection, and extraction. All fields
// must be positive; use DefaultLimits for production-safe defaults.
type Limits struct {
	MaxStoredBytes  int64
	MaxLogicalBytes int64
	MaxFileBytes    int64
	MaxEntries      int
	MaxPathBytes    int
}

func DefaultLimits() Limits {
	return Limits{MaxStoredBytes: 1 << 30, MaxLogicalBytes: 2 << 30, MaxFileBytes: 512 << 20, MaxEntries: 100000, MaxPathBytes: 4 << 10}
}

func (l Limits) validate() error {
	if l.MaxStoredBytes <= 0 || l.MaxLogicalBytes <= 0 || l.MaxFileBytes <= 0 || l.MaxEntries <= 0 || l.MaxPathBytes <= 0 {
		return fmt.Errorf("%w: every limit must be positive", ErrLimitExceeded)
	}
	return nil
}

type ArchiveKind string

const (
	ArchiveHarness   ArchiveKind = "harness"
	ArchiveWorkspace ArchiveKind = "workspace"
)

type FileType string

const (
	FileRegular FileType = "regular"
	FileSymlink FileType = "symlink"
)

// File describes one logical archive file. Blob is the content-addressed tar
// member for a regular file; links are represented only as validated metadata.
type File struct {
	Path       string   `json:"path"`
	Type       FileType `json:"type"`
	Mode       uint32   `json:"mode"`
	Size       int64    `json:"size,omitempty"`
	SHA256     string   `json:"sha256,omitempty"`
	Blob       string   `json:"blob,omitempty"`
	LinkTarget string   `json:"link_target,omitempty"`
}

type HarnessMember struct {
	NativeSessionID       string `json:"native_session_id"`
	NativeParentSessionID string `json:"native_parent_session_id,omitempty"`
	RelativeMemberPath    string `json:"relative_member_path"`
	MemberKind            string `json:"member_kind"`
	AgentName             string `json:"agent_name,omitempty"`
	Status                string `json:"status,omitempty"`
	Model                 string `json:"model,omitempty"`
	HarnessBuild          string `json:"harness_build"`
	NativeSchemaVersion   int    `json:"native_schema_version"`
	ParseStatus           string `json:"parse_status"`
}

type HarnessManifest struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"format_version"`
	SchemaVersion int             `json:"schema_version"`
	HarnessBuild  string          `json:"harness_build"`
	RootSessionID string          `json:"root_session_id"`
	Members       []HarnessMember `json:"members"`
	Files         []File          `json:"files"`
}

type Patch struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Blob   string `json:"blob"`
}

type IndexEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Object string `json:"object"`
	Stage  int    `json:"stage"`
}

type WorkspaceManifest struct {
	Format           string       `json:"format"`
	FormatVersion    int          `json:"format_version"`
	SchemaVersion    int          `json:"schema_version"`
	HeadCommit       string       `json:"head_commit"`
	Branch           string       `json:"branch,omitempty"`
	Detached         bool         `json:"detached"`
	BaseRef          string       `json:"base_ref,omitempty"`
	BaseCommit       string       `json:"base_commit"`
	ObjectFormat     string       `json:"object_format"`
	RepositoryFormat string       `json:"repository_format"`
	Staged           Patch        `json:"staged_patch"`
	Unstaged         Patch        `json:"unstaged_patch"`
	StagedPaths      []string     `json:"staged_paths"`
	UnstagedPaths    []string     `json:"unstaged_paths"`
	Index            []IndexEntry `json:"index"`
	Untracked        []File       `json:"untracked"`
	InventoryDigest  string       `json:"inventory_digest"`
}

// Artifact is returned by deterministic writers.
type Artifact struct {
	Kind         ArchiveKind
	SHA256       string
	StoredBytes  int64
	LogicalBytes int64
	EntryCount   int
}

// Inspection contains validated, derived archive metadata. Exactly one
// manifest pointer is non-nil.
type Inspection struct {
	Artifact
	Harness   *HarnessManifest
	Workspace *WorkspaceManifest
}
