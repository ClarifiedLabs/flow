package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	HistoryBlobBackendLocal = "local"
	HistoryBlobBackendS3    = "s3"
	HistorySSES3            = "sse-s3"
	HistorySSEKMS           = "sse-kms"
)

// CoordinatorHistoryConfig configures durable history storage and the bounded
// metadata-driven reconciliation pass. Committed history has no normal TTL.
type CoordinatorHistoryConfig struct {
	Blob           HistoryBlobConfig           `json:"blob" yaml:"blob"`
	Reconciliation HistoryReconciliationConfig `json:"reconciliation" yaml:"reconciliation"`
	Transcript     HistoryTranscriptConfig     `json:"transcript" yaml:"transcript"`
	Archive        HistoryArchiveConfig        `json:"archive" yaml:"archive"`
}

type HistoryBlobConfig struct {
	Backend       string          `json:"backend" yaml:"backend"`
	LocalPath     string          `json:"local_path" yaml:"local_path"`
	MaxRangeBytes string          `json:"max_range_bytes" yaml:"max_range_bytes"`
	S3            HistoryS3Config `json:"s3" yaml:"s3"`
}

type HistoryS3Config struct {
	Region     string `json:"region" yaml:"region"`
	Bucket     string `json:"bucket" yaml:"bucket"`
	Prefix     string `json:"prefix" yaml:"prefix"`
	Endpoint   string `json:"endpoint" yaml:"endpoint"`
	PathStyle  bool   `json:"path_style" yaml:"path_style"`
	AllowHTTP  bool   `json:"allow_http" yaml:"allow_http"`
	Encryption string `json:"encryption" yaml:"encryption"`
	KMSKeyID   string `json:"kms_key_id" yaml:"kms_key_id"`
	BucketKey  bool   `json:"bucket_key" yaml:"bucket_key"`
}

type HistoryReconciliationConfig struct {
	Interval       string `json:"interval" yaml:"interval"`
	TemporaryGrace string `json:"temporary_grace" yaml:"temporary_grace"`
	OrphanGrace    string `json:"orphan_grace" yaml:"orphan_grace"`
	BatchSize      int    `json:"batch_size" yaml:"batch_size"`
}

type HistoryTranscriptConfig struct {
	SegmentBytes  string `json:"segment_bytes" yaml:"segment_bytes"`
	FlushInterval string `json:"flush_interval" yaml:"flush_interval"`
}

type HistoryArchiveConfig struct {
	MaxStoredBytes        string `json:"max_stored_bytes" yaml:"max_stored_bytes"`
	MaxLogicalBytes       string `json:"max_logical_bytes" yaml:"max_logical_bytes"`
	MaxFileBytes          string `json:"max_file_bytes" yaml:"max_file_bytes"`
	MaxEntries            int    `json:"max_entries" yaml:"max_entries"`
	MaxPathBytes          int    `json:"max_path_bytes" yaml:"max_path_bytes"`
	MaxOutstandingBytes   string `json:"max_outstanding_bytes" yaml:"max_outstanding_bytes"`
	MaxOutstandingUploads int    `json:"max_outstanding_uploads" yaml:"max_outstanding_uploads"`
}

type ResolvedCoordinatorHistory struct {
	Blob           ResolvedHistoryBlob
	Reconciliation ResolvedHistoryReconciliation
	Transcript     ResolvedHistoryTranscript
	Archive        ResolvedHistoryArchive
}

type ResolvedHistoryBlob struct {
	Backend       string
	LocalPath     string
	MaxRangeBytes int64
	S3            ResolvedHistoryS3
}

type ResolvedHistoryS3 struct {
	Region, Bucket, Prefix, Endpoint string
	PathStyle, AllowHTTP, BucketKey  bool
	Encryption, KMSKeyID             string
}

type ResolvedHistoryReconciliation struct {
	Interval, TemporaryGrace, OrphanGrace time.Duration
	BatchSize                             int
}

type ResolvedHistoryTranscript struct {
	SegmentBytes  int64
	FlushInterval time.Duration
}

type ResolvedHistoryArchive struct {
	MaxStoredBytes, MaxLogicalBytes, MaxFileBytes, MaxOutstandingBytes int64
	MaxEntries, MaxPathBytes, MaxOutstandingUploads                    int
}

func (c CoordinatorHistoryConfig) Resolve(dataDir string) (ResolvedCoordinatorHistory, error) {
	blobConfig, err := c.Blob.resolve(dataDir)
	if err != nil {
		return ResolvedCoordinatorHistory{}, err
	}
	reconciliation, err := c.Reconciliation.resolve()
	if err != nil {
		return ResolvedCoordinatorHistory{}, err
	}
	transcript, err := c.Transcript.resolve()
	if err != nil {
		return ResolvedCoordinatorHistory{}, err
	}
	archive, err := c.Archive.resolve()
	if err != nil {
		return ResolvedCoordinatorHistory{}, err
	}
	return ResolvedCoordinatorHistory{Blob: blobConfig, Reconciliation: reconciliation, Transcript: transcript, Archive: archive}, nil
}

func (c HistoryBlobConfig) resolve(dataDir string) (ResolvedHistoryBlob, error) {
	backend := strings.ToLower(strings.TrimSpace(c.Backend))
	if backend == "" {
		backend = HistoryBlobBackendLocal
	}
	maximum, err := resolveHistoryBytes(c.MaxRangeBytes, 8<<20, "coordinator history.blob.max_range_bytes")
	if err != nil {
		return ResolvedHistoryBlob{}, err
	}
	resolved := ResolvedHistoryBlob{Backend: backend, MaxRangeBytes: maximum}
	switch backend {
	case HistoryBlobBackendLocal:
		path := strings.TrimSpace(c.LocalPath)
		if path == "" {
			path = filepath.Join(dataDir, "history", "blobs")
		}
		if historyS3Configured(c.S3) {
			return ResolvedHistoryBlob{}, errors.New("coordinator history.blob.s3 settings require backend s3")
		}
		resolved.LocalPath = cleanRequiredPath(path)
	case HistoryBlobBackendS3:
		if strings.TrimSpace(c.LocalPath) != "" {
			return ResolvedHistoryBlob{}, errors.New("coordinator history.blob.local_path cannot be set with backend s3")
		}
		s3, err := resolveHistoryS3(c.S3)
		if err != nil {
			return ResolvedHistoryBlob{}, err
		}
		resolved.S3 = s3
	default:
		return ResolvedHistoryBlob{}, fmt.Errorf("coordinator history.blob.backend must be %q or %q", HistoryBlobBackendLocal, HistoryBlobBackendS3)
	}
	return resolved, nil
}

func historyS3Configured(c HistoryS3Config) bool {
	return c.Region != "" || c.Bucket != "" || c.Prefix != "" || c.Endpoint != "" || c.PathStyle || c.AllowHTTP || c.Encryption != "" || c.KMSKeyID != "" || c.BucketKey
}

func resolveHistoryS3(c HistoryS3Config) (ResolvedHistoryS3, error) {
	resolved := ResolvedHistoryS3{
		Region: strings.TrimSpace(c.Region), Bucket: strings.TrimSpace(c.Bucket), Prefix: strings.Trim(strings.TrimSpace(c.Prefix), "/"),
		Endpoint: strings.TrimSpace(c.Endpoint), PathStyle: c.PathStyle, AllowHTTP: c.AllowHTTP,
		Encryption: strings.ToLower(strings.TrimSpace(c.Encryption)), KMSKeyID: strings.TrimSpace(c.KMSKeyID), BucketKey: c.BucketKey,
	}
	if resolved.Region == "" || resolved.Bucket == "" {
		return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 region and bucket are required")
	}
	if strings.ContainsAny(resolved.Bucket, "/\\\r\n\x00") || strings.ContainsAny(resolved.Prefix, "\\\r\n\x00") {
		return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 bucket or prefix is unsafe")
	}
	if resolved.Endpoint != "" {
		u, err := url.Parse(resolved.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 endpoint must be an origin URL without credentials, path, query, or fragment")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && resolved.AllowHTTP) {
			return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 endpoint requires TLS unless allow_http is explicitly enabled")
		}
	} else if resolved.AllowHTTP {
		return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 allow_http requires an explicit http endpoint")
	}
	if resolved.Encryption == "" {
		resolved.Encryption = HistorySSES3
	}
	switch resolved.Encryption {
	case HistorySSES3:
		if resolved.KMSKeyID != "" || resolved.BucketKey {
			return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 kms_key_id and bucket_key require sse-kms")
		}
	case HistorySSEKMS:
		if resolved.KMSKeyID == "" {
			return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 kms_key_id is required for sse-kms")
		}
	default:
		return ResolvedHistoryS3{}, errors.New("coordinator history.blob.s3 encryption must be sse-s3 or sse-kms")
	}
	return resolved, nil
}

func (c HistoryReconciliationConfig) resolve() (ResolvedHistoryReconciliation, error) {
	interval, err := resolveHistoryDuration(c.Interval, 15*time.Minute, "coordinator history.reconciliation.interval")
	if err != nil {
		return ResolvedHistoryReconciliation{}, err
	}
	temporaryGrace, err := resolveHistoryDuration(c.TemporaryGrace, 24*time.Hour, "coordinator history.reconciliation.temporary_grace")
	if err != nil {
		return ResolvedHistoryReconciliation{}, err
	}
	orphanGrace, err := resolveHistoryDuration(c.OrphanGrace, 7*24*time.Hour, "coordinator history.reconciliation.orphan_grace")
	if err != nil {
		return ResolvedHistoryReconciliation{}, err
	}
	if temporaryGrace < interval {
		return ResolvedHistoryReconciliation{}, errors.New("coordinator history.reconciliation.temporary_grace must be >= interval")
	}
	if orphanGrace < temporaryGrace {
		return ResolvedHistoryReconciliation{}, errors.New("coordinator history.reconciliation.orphan_grace must be >= temporary_grace")
	}
	batch := c.BatchSize
	if batch == 0 {
		batch = 1000
	}
	if batch < 1 || batch > 10000 {
		return ResolvedHistoryReconciliation{}, errors.New("coordinator history.reconciliation.batch_size must be between 1 and 10000")
	}
	return ResolvedHistoryReconciliation{Interval: interval, TemporaryGrace: temporaryGrace, OrphanGrace: orphanGrace, BatchSize: batch}, nil
}

func (c HistoryTranscriptConfig) resolve() (ResolvedHistoryTranscript, error) {
	bytes, err := resolveHistoryBytes(c.SegmentBytes, 4<<20, "history.transcript.segment_bytes")
	if err != nil {
		return ResolvedHistoryTranscript{}, err
	}
	flush, err := resolveHistoryDuration(c.FlushInterval, 30*time.Second, "history.transcript.flush_interval")
	if err != nil {
		return ResolvedHistoryTranscript{}, err
	}
	if bytes < 64<<10 || bytes > 16<<20 {
		return ResolvedHistoryTranscript{}, errors.New("history.transcript.segment_bytes must be between 64KiB and 16MiB")
	}
	if flush > 15*time.Minute {
		return ResolvedHistoryTranscript{}, errors.New("history.transcript.flush_interval must not exceed 15m")
	}
	return ResolvedHistoryTranscript{SegmentBytes: bytes, FlushInterval: flush}, nil
}

func (c HistoryArchiveConfig) resolve() (ResolvedHistoryArchive, error) {
	stored, err := resolveHistoryBytes(c.MaxStoredBytes, 512<<20, "history.archive.max_stored_bytes")
	if err != nil {
		return ResolvedHistoryArchive{}, err
	}
	logical, err := resolveHistoryBytes(c.MaxLogicalBytes, 2<<30, "history.archive.max_logical_bytes")
	if err != nil {
		return ResolvedHistoryArchive{}, err
	}
	file, err := resolveHistoryBytes(c.MaxFileBytes, 1<<30, "history.archive.max_file_bytes")
	if err != nil {
		return ResolvedHistoryArchive{}, err
	}
	entries := c.MaxEntries
	if entries == 0 {
		entries = 100000
	}
	pathBytes := c.MaxPathBytes
	if pathBytes == 0 {
		pathBytes = 4 << 10
	}
	outstandingBytes, err := resolveHistoryBytes(c.MaxOutstandingBytes, 2<<30, "history.archive.max_outstanding_bytes")
	if err != nil {
		return ResolvedHistoryArchive{}, err
	}
	outstandingUploads := c.MaxOutstandingUploads
	if outstandingUploads == 0 {
		outstandingUploads = 32
	}
	if file > logical || stored > logical {
		return ResolvedHistoryArchive{}, errors.New("history archive stored and per-file limits must not exceed the logical-byte limit")
	}
	if outstandingBytes < stored {
		return ResolvedHistoryArchive{}, errors.New("history archive max_outstanding_bytes must be >= max_stored_bytes")
	}
	if entries < 1 || entries > 1000000 || pathBytes < 256 || pathBytes > 32<<10 || outstandingUploads < 1 || outstandingUploads > 10000 {
		return ResolvedHistoryArchive{}, errors.New("history archive entries/path/outstanding-upload limits are outside safe bounds")
	}
	return ResolvedHistoryArchive{
		MaxStoredBytes: stored, MaxLogicalBytes: logical, MaxFileBytes: file,
		MaxEntries: entries, MaxPathBytes: pathBytes,
		MaxOutstandingBytes: outstandingBytes, MaxOutstandingUploads: outstandingUploads,
	}, nil
}

// WorkerHistoryConfig is parsed and held by flow-worker in Slice 1. Capture
// execution is intentionally not enabled by merely loading these settings.
type WorkerHistoryConfig struct {
	Transcript      HistoryTranscriptConfig    `json:"transcript" yaml:"transcript"`
	Archive         HistoryArchiveConfig       `json:"archive" yaml:"archive"`
	Outbox          WorkerHistoryOutboxConfig  `json:"outbox" yaml:"outbox"`
	MandatoryFinal  *bool                      `json:"mandatory_final_capture" yaml:"mandatory_final_capture"`
	StopWakeup      *bool                      `json:"stop_transcript_wakeup" yaml:"stop_transcript_wakeup"`
	LiveCheckpoints WorkerLiveCheckpointConfig `json:"live_checkpoints" yaml:"live_checkpoints"`
}

type WorkerHistoryOutboxConfig struct {
	Path           string `json:"path" yaml:"path"`
	ReplayInterval string `json:"replay_interval" yaml:"replay_interval"`
}

type WorkerLiveCheckpointConfig struct {
	Enabled               bool   `json:"enabled" yaml:"enabled"`
	Debounce              string `json:"debounce" yaml:"debounce"`
	RateInterval          string `json:"rate_interval" yaml:"rate_interval"`
	DirtyBytes            string `json:"dirty_bytes" yaml:"dirty_bytes"`
	MaxPerCapture         int    `json:"max_per_capture" yaml:"max_per_capture"`
	CumulativeStoredBytes string `json:"cumulative_stored_bytes" yaml:"cumulative_stored_bytes"`
}

type ResolvedWorkerHistory struct {
	Transcript      ResolvedHistoryTranscript
	Archive         ResolvedHistoryArchive
	OutboxPath      string
	ReplayInterval  time.Duration
	MandatoryFinal  bool
	StopWakeup      bool
	LiveCheckpoints ResolvedWorkerLiveCheckpoints
}

type ResolvedWorkerLiveCheckpoints struct {
	Enabled                           bool
	Debounce, RateInterval            time.Duration
	DirtyBytes, CumulativeStoredBytes int64
	MaxPerCapture                     int
}

func (c WorkerHistoryConfig) Resolve(workDir string) (ResolvedWorkerHistory, error) {
	transcript, err := c.Transcript.resolve()
	if err != nil {
		return ResolvedWorkerHistory{}, fmt.Errorf("worker %w", err)
	}
	archive, err := c.Archive.resolve()
	if err != nil {
		return ResolvedWorkerHistory{}, fmt.Errorf("worker %w", err)
	}
	outbox := strings.TrimSpace(c.Outbox.Path)
	if outbox == "" {
		outbox = filepath.Join(workDir, "history-outbox")
	}
	outbox = cleanRequiredPath(outbox)
	jobs := filepath.Join(cleanRequiredPath(workDir), "jobs")
	if withinPath(outbox, jobs) {
		return ResolvedWorkerHistory{}, errors.New("worker history.outbox.path must be outside work_dir/jobs")
	}
	replay, err := resolveHistoryDuration(c.Outbox.ReplayInterval, 30*time.Second, "worker history.outbox.replay_interval")
	if err != nil {
		return ResolvedWorkerHistory{}, err
	}
	mandatory := true
	if c.MandatoryFinal != nil {
		mandatory = *c.MandatoryFinal
	}
	if !mandatory {
		return ResolvedWorkerHistory{}, errors.New("worker history.mandatory_final_capture cannot be disabled")
	}
	stop := true
	if c.StopWakeup != nil {
		stop = *c.StopWakeup
	}
	if !stop {
		return ResolvedWorkerHistory{}, errors.New("worker history.stop_transcript_wakeup cannot be disabled")
	}
	checkpoints, err := c.LiveCheckpoints.resolve(archive)
	if err != nil {
		return ResolvedWorkerHistory{}, err
	}
	return ResolvedWorkerHistory{Transcript: transcript, Archive: archive, OutboxPath: outbox, ReplayInterval: replay, MandatoryFinal: mandatory, StopWakeup: stop, LiveCheckpoints: checkpoints}, nil
}

func (c WorkerLiveCheckpointConfig) resolve(archive ResolvedHistoryArchive) (ResolvedWorkerLiveCheckpoints, error) {
	debounce, err := resolveHistoryDuration(c.Debounce, 30*time.Second, "worker history.live_checkpoints.debounce")
	if err != nil {
		return ResolvedWorkerLiveCheckpoints{}, err
	}
	rate, err := resolveHistoryDuration(c.RateInterval, 5*time.Minute, "worker history.live_checkpoints.rate_interval")
	if err != nil {
		return ResolvedWorkerLiveCheckpoints{}, err
	}
	if rate < debounce {
		return ResolvedWorkerLiveCheckpoints{}, errors.New("worker history.live_checkpoints.rate_interval must be >= debounce")
	}
	dirty, err := resolveHistoryBytes(c.DirtyBytes, 4<<20, "worker history.live_checkpoints.dirty_bytes")
	if err != nil {
		return ResolvedWorkerLiveCheckpoints{}, err
	}
	budget, err := resolveHistoryBytes(c.CumulativeStoredBytes, 2<<30, "worker history.live_checkpoints.cumulative_stored_bytes")
	if err != nil {
		return ResolvedWorkerLiveCheckpoints{}, err
	}
	max := c.MaxPerCapture
	if max == 0 {
		max = 24
	}
	if max < 1 || max > 10000 {
		return ResolvedWorkerLiveCheckpoints{}, errors.New("worker history.live_checkpoints.max_per_capture must be between 1 and 10000")
	}
	if dirty > archive.MaxLogicalBytes || budget < archive.MaxStoredBytes {
		return ResolvedWorkerLiveCheckpoints{}, errors.New("worker history live-checkpoint dirty/budget limits conflict with archive limits")
	}
	return ResolvedWorkerLiveCheckpoints{Enabled: c.Enabled, Debounce: debounce, RateInterval: rate, DirtyBytes: dirty, MaxPerCapture: max, CumulativeStoredBytes: budget}, nil
}

func resolveHistoryDuration(value string, fallback time.Duration, key string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	return d, nil
}

func resolveHistoryBytes(value string, fallback int64, key string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := parseWorkerByteSize(value, strings.TrimPrefix(key, "worker "))
	if err != nil {
		return 0, err
	}
	if parsed == 0 || parsed > math.MaxInt64 {
		return 0, fmt.Errorf("%s must be between 1 byte and %d bytes", key, int64(math.MaxInt64))
	}
	return int64(parsed), nil
}

func withinPath(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
