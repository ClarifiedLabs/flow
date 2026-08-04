package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/blob"
)

type manifestResumeBarrierStore struct {
	blob.Store
	mu       sync.Mutex
	resumes  int
	released chan struct{}
}

func newManifestResumeBarrierStore(store blob.Store) *manifestResumeBarrierStore {
	return &manifestResumeBarrierStore{Store: store, released: make(chan struct{})}
}

func (s *manifestResumeBarrierStore) Resume(ctx context.Context, id string) (blob.Temporary, error) {
	temporary, err := s.Store.Resume(ctx, id)
	if err != nil {
		return blob.Temporary{}, err
	}
	s.mu.Lock()
	s.resumes++
	if s.resumes == 2 {
		close(s.released)
	}
	released := s.released
	s.mu.Unlock()
	select {
	case <-released:
		return temporary, nil
	case <-ctx.Done():
		return blob.Temporary{}, ctx.Err()
	}
}

func (s *manifestResumeBarrierStore) resumeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumes
}

func TestGenerateCanonicalManifestPublishesSanitizedDeterministicInventory(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()
	input.JobID, input.LeaseID = "job-manifest", "lease-manifest"
	input.ExpectedHarness = false
	input.HarnessName, input.HarnessVersion, input.HarnessSchemaVersion = "", "", 0
	reserved := reserveHistoryCapture(t, env.service, input)
	capture := transitionHistory(t, env.service, reserved.Capture, HistoryCaptureRunning)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureQuiescing)

	if _, err := env.service.GenerateCanonicalManifest(ctx, capture.ID); !errors.Is(err, ErrHistoryIncomplete) {
		t.Fatalf("manifest before transcript seal err = %v, want incomplete", err)
	}
	seal := TranscriptSeal{FinalEpoch: -1, SegmentCount: 0, LogicalLength: 0, SHA256: historyDigest(nil)}
	if err := env.service.SealTranscript(ctx, capture.ID, reserved.UploadGrant, seal); err != nil {
		t.Fatalf("seal empty transcript: %v", err)
	}
	capture, err := env.service.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts: []FinalArtifactExpectation{
			{LogicalKey: "workspace/final", Kind: HistoryArtifactWorkspaceSnapshot},
			{LogicalKey: "manifest/final", Kind: HistoryArtifactManifest},
		},
		TranscriptSeal: &seal, ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("declare expected set: %v", err)
	}
	publishWorkspaceSummary(t, env.service, capture.ID, reserved.UploadGrant)
	exitCode := 17
	capture, err = env.service.RecordExecutionVerdict(ctx, capture.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionFailed, ExitCode: &exitCode, ErrorCode: "command_failed",
		ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("record verdict: %v", err)
	}

	manifest, err := env.service.GenerateCanonicalManifest(ctx, capture.ID)
	if err != nil {
		t.Fatalf("generate canonical manifest: %v", err)
	}
	if manifest.Kind != HistoryArtifactManifest || manifest.LogicalKey != "manifest/final" || manifest.PublicationState != HistoryPublicationCommitted {
		t.Fatalf("manifest artifact = %+v", manifest)
	}
	again, err := env.service.GenerateCanonicalManifest(ctx, capture.ID)
	if err != nil || again.ID != manifest.ID || again.SHA256 != manifest.SHA256 {
		t.Fatalf("manifest retry = %+v err=%v, want same artifact", again, err)
	}
	_, body, err := env.service.OpenArtifact(ctx, capture.ID, manifest.ID)
	if err != nil {
		t.Fatalf("open canonical manifest: %v", err)
	}
	defer body.Close()
	encoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read canonical manifest: %v", err)
	}
	if strings.Contains(string(encoded), "upload_grant") || strings.Contains(string(encoded), "blob_key") || strings.Contains(string(encoded), "temporary_upload_id") {
		t.Fatalf("canonical manifest leaks internal capability/storage metadata: %s", encoded)
	}
	var decoded canonicalHistoryManifest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode canonical manifest: %v", err)
	}
	if decoded.Format != "flow-history-manifest" || decoded.Capture.ExecutionVerdict != HistoryExecutionFailed || decoded.Transcript.SHA256 != seal.SHA256 || len(decoded.Artifacts) != 1 || decoded.Workspace == nil {
		t.Fatalf("canonical manifest = %+v", decoded)
	}

	capture = transitionHistory(t, env.service, capture, HistoryCaptureSealed)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureUploading)
	completed, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil {
		t.Fatalf("complete failed execution capture: %v", err)
	}
	if completed.State != HistoryCaptureComplete || completed.ExecutionVerdict != HistoryExecutionFailed {
		t.Fatalf("completed failed execution capture = %+v", completed)
	}
}

func TestGenerateCanonicalManifestConcurrentCallsDoNotStrandUploadIntent(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := baseHistoryReservation()
	input.JobID, input.LeaseID = "job-manifest-concurrent", "lease-manifest-concurrent"
	input.ExpectedHarness = false
	input.HarnessName, input.HarnessVersion, input.HarnessSchemaVersion = "", "", 0
	reserved := reserveHistoryCapture(t, env.service, input)
	capture := transitionHistory(t, env.service, reserved.Capture, HistoryCaptureRunning)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureQuiescing)
	seal := TranscriptSeal{FinalEpoch: -1, SegmentCount: 0, LogicalLength: 0, SHA256: historyDigest(nil)}
	if err := env.service.SealTranscript(ctx, capture.ID, reserved.UploadGrant, seal); err != nil {
		t.Fatalf("seal empty transcript: %v", err)
	}
	var err error
	capture, err = env.service.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts: []FinalArtifactExpectation{
			{LogicalKey: "workspace/final", Kind: HistoryArtifactWorkspaceSnapshot},
			{LogicalKey: "manifest/final", Kind: HistoryArtifactManifest},
		},
		TranscriptSeal: &seal, ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("declare expected set: %v", err)
	}
	publishWorkspaceSummary(t, env.service, capture.ID, reserved.UploadGrant)
	exitCode := 0
	capture, err = env.service.RecordExecutionVerdict(ctx, capture.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionSucceeded, ExitCode: &exitCode,
		ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("record verdict: %v", err)
	}

	barrier := newManifestResumeBarrierStore(env.blobs)
	service := NewHistoryCaptureService(env.store.DB(), barrier)
	type result struct {
		artifact HistoryArtifact
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			artifact, err := service.GenerateCanonicalManifest(ctx, capture.ID)
			results <- result{artifact: artifact, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent manifest results: first=%+v second=%+v", first, second)
	}
	if barrier.resumeCount() < 2 {
		t.Fatalf("canonical temporary resume count = %d, want at least 2 concurrent uploads", barrier.resumeCount())
	}
	if first.artifact.ID != second.artifact.ID || first.artifact.SHA256 != second.artifact.SHA256 {
		t.Fatalf("concurrent manifests differ: first=%+v second=%+v", first.artifact, second.artifact)
	}
	var manifests, activeIntents int
	if err := env.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM history_artifacts
WHERE capture_id = ? AND logical_key = 'manifest/final' AND kind = 'manifest'`, capture.ID).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM history_upload_intents
WHERE capture_id = ? AND state = 'active'`, capture.ID).Scan(&activeIntents); err != nil {
		t.Fatal(err)
	}
	if manifests != 1 || activeIntents != 0 {
		t.Fatalf("canonical manifests=%d active upload intents=%d, want 1 and 0", manifests, activeIntents)
	}

	capture = transitionHistory(t, env.service, capture, HistoryCaptureSealed)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureUploading)
	completed, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil {
		t.Fatalf("complete capture after concurrent manifest generation: %v", err)
	}
	if completed.State != HistoryCaptureComplete {
		t.Fatalf("completed capture state = %s, want %s", completed.State, HistoryCaptureComplete)
	}
}
