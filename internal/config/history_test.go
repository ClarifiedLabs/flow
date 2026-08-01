package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryConfigurationDefaults(t *testing.T) {
	dataDir := t.TempDir()
	coordinator, err := (CoordinatorHistoryConfig{}).Resolve(dataDir)
	if err != nil {
		t.Fatalf("resolve coordinator history defaults: %v", err)
	}
	if coordinator.Blob.Backend != HistoryBlobBackendLocal || coordinator.Blob.LocalPath != filepath.Join(dataDir, "history", "blobs") {
		t.Fatalf("default blob = %+v", coordinator.Blob)
	}
	if coordinator.Blob.MaxRangeBytes != 8<<20 || coordinator.Reconciliation.Interval != 15*time.Minute || coordinator.Reconciliation.TemporaryGrace != 24*time.Hour || coordinator.Reconciliation.OrphanGrace != 7*24*time.Hour {
		t.Fatalf("default coordinator history = %+v", coordinator)
	}
	if coordinator.Transcript.SegmentBytes != 4<<20 || coordinator.Transcript.FlushInterval != 30*time.Second {
		t.Fatalf("default transcript = %+v", coordinator.Transcript)
	}
	if coordinator.Archive.MaxStoredBytes != 512<<20 || coordinator.Archive.MaxLogicalBytes != 2<<30 || coordinator.Archive.MaxFileBytes != 1<<30 || coordinator.Archive.MaxEntries != 100000 || coordinator.Archive.MaxPathBytes != 4<<10 || coordinator.Archive.MaxOutstandingBytes != 2<<30 || coordinator.Archive.MaxOutstandingUploads != 32 {
		t.Fatalf("default archive = %+v", coordinator.Archive)
	}

	workDir := filepath.Join(t.TempDir(), "worker")
	worker, err := (WorkerHistoryConfig{}).Resolve(workDir)
	if err != nil {
		t.Fatalf("resolve worker history defaults: %v", err)
	}
	if worker.OutboxPath != filepath.Join(workDir, "history-outbox") || worker.ReplayInterval != 30*time.Second || !worker.MandatoryFinal || !worker.StopWakeup {
		t.Fatalf("default worker history = %+v", worker)
	}
	if worker.LiveCheckpoints.Enabled || worker.LiveCheckpoints.Debounce != 30*time.Second || worker.LiveCheckpoints.RateInterval != 5*time.Minute || worker.LiveCheckpoints.MaxPerCapture != 24 || worker.LiveCheckpoints.CumulativeStoredBytes != 2<<30 {
		t.Fatalf("default live checkpoints = %+v", worker.LiveCheckpoints)
	}
}

func TestCoordinatorHistoryRejectsUnsafeStorageAndRelationships(t *testing.T) {
	tests := []struct {
		name string
		cfg  CoordinatorHistoryConfig
		want string
	}{
		{name: "http endpoint", cfg: CoordinatorHistoryConfig{Blob: HistoryBlobConfig{Backend: "s3", S3: HistoryS3Config{Region: "us-east-1", Bucket: "private", Endpoint: "http://minio:9000"}}}, want: "requires TLS"},
		{name: "allow http without endpoint", cfg: CoordinatorHistoryConfig{Blob: HistoryBlobConfig{Backend: "s3", S3: HistoryS3Config{Region: "us-east-1", Bucket: "private", AllowHTTP: true}}}, want: "requires an explicit"},
		{name: "sse kms without key", cfg: CoordinatorHistoryConfig{Blob: HistoryBlobConfig{Backend: "s3", S3: HistoryS3Config{Region: "us-east-1", Bucket: "private", Encryption: "sse-kms"}}}, want: "kms_key_id is required"},
		{name: "sse s3 with key", cfg: CoordinatorHistoryConfig{Blob: HistoryBlobConfig{Backend: "s3", S3: HistoryS3Config{Region: "us-east-1", Bucket: "private", Encryption: "sse-s3", KMSKeyID: "secret"}}}, want: "require sse-kms"},
		{name: "short temporary grace", cfg: CoordinatorHistoryConfig{Reconciliation: HistoryReconciliationConfig{Interval: "1h", TemporaryGrace: "30m"}}, want: "temporary_grace must be >= interval"},
		{name: "archive relationship", cfg: CoordinatorHistoryConfig{Archive: HistoryArchiveConfig{MaxStoredBytes: "3GiB", MaxLogicalBytes: "2GiB"}}, want: "must not exceed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.cfg.Resolve(t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestWorkerHistoryRejectsUnsafeFinalizationAndCheckpointSettings(t *testing.T) {
	disabled := false
	tests := []struct {
		name string
		cfg  WorkerHistoryConfig
		want string
	}{
		{name: "mandatory final disabled", cfg: WorkerHistoryConfig{MandatoryFinal: &disabled}, want: "cannot be disabled"},
		{name: "stop wakeup disabled", cfg: WorkerHistoryConfig{StopWakeup: &disabled}, want: "cannot be disabled"},
		{name: "checkpoint rate", cfg: WorkerHistoryConfig{LiveCheckpoints: WorkerLiveCheckpointConfig{Debounce: "10m", RateInterval: "5m"}}, want: "rate_interval must be >= debounce"},
		{name: "checkpoint cumulative budget", cfg: WorkerHistoryConfig{LiveCheckpoints: WorkerLiveCheckpointConfig{CumulativeStoredBytes: "1MiB"}}, want: "conflict with archive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.cfg.Resolve(t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve error = %v, want containing %q", err, test.want)
			}
		})
	}
}
