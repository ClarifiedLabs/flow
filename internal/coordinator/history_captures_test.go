package coordinator

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/blob"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

type historyCaptureTestEnv struct {
	store   *flowdb.Store
	blobs   *blob.Local
	service *HistoryCaptureService
}

func newHistoryCaptureTestEnv(t *testing.T) *historyCaptureTestEnv {
	t.Helper()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "project.db"))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	blobs, err := blob.NewLocal(filepath.Join(t.TempDir(), "blobs"), blob.LocalOptions{})
	if err != nil {
		t.Fatalf("open local blob store: %v", err)
	}
	return &historyCaptureTestEnv{store: store, blobs: blobs, service: NewHistoryCaptureService(store.DB(), blobs)}
}

func baseHistoryReservation() ReserveHistoryCaptureInput {
	return ReserveHistoryCaptureInput{
		ProjectID: "p-history", JobID: "job-1", LeaseID: "lease-1", LeaseAttempt: 1,
		WorkerID: "worker-1", TaskID: "t-1", SessionID: "session-1",
		WorkflowRunID: "wr-1", NodeRunID: "wnr-1", NodeVisit: 1,
		Stage: "implement", Role: "author", HarnessName: "harness", HarnessVersion: "0.4.3",
		ExpectedTranscript: true, ExpectedHarness: true,
	}
}

func reserveHistoryCapture(t *testing.T, service *HistoryCaptureService, input ReserveHistoryCaptureInput) ReserveHistoryCaptureResult {
	t.Helper()
	result, err := service.Reserve(context.Background(), input)
	if err != nil {
		t.Fatalf("reserve history capture: %v", err)
	}
	if !result.Created || result.UploadGrant == "" {
		t.Fatalf("initial reservation = %+v, want created with raw grant", result)
	}
	return result
}

func uploadHistoryBytes(t *testing.T, service *HistoryCaptureService, captureID, grant string, content []byte) blob.Temporary {
	t.Helper()
	ctx := context.Background()
	upload, err := service.BeginUpload(ctx, captureID, grant)
	if err != nil {
		t.Fatalf("begin history upload: %v", err)
	}
	if _, err := upload.Write(content); err != nil {
		t.Fatalf("write history upload: %v", err)
	}
	temporary, err := upload.Complete(ctx)
	if err != nil {
		t.Fatalf("complete history upload: %v", err)
	}
	return temporary
}

func publishHistoryBytes(t *testing.T, service *HistoryCaptureService, captureID, grant string, input PublishHistoryArtifactInput, content []byte) HistoryArtifact {
	t.Helper()
	temporary := uploadHistoryBytes(t, service, captureID, grant, content)
	artifact, err := service.PublishArtifact(context.Background(), captureID, grant, input, temporary)
	if err != nil {
		t.Fatalf("publish history artifact %q: %v", input.LogicalKey, err)
	}
	if artifact.PublicationState != HistoryPublicationCommitted {
		t.Fatalf("artifact publication state = %q, want committed", artifact.PublicationState)
	}
	return artifact
}

func historyDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return stringHex(digest[:])
}

func stringHex(value []byte) string {
	const digits = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for i, octet := range value {
		result[i*2] = digits[octet>>4]
		result[i*2+1] = digits[octet&15]
	}
	return string(result)
}

func transitionHistory(t *testing.T, service *HistoryCaptureService, capture HistoryCapture, to HistoryCaptureState) HistoryCapture {
	t.Helper()
	updated, err := service.Transition(context.Background(), capture.ID, TransitionHistoryCaptureInput{
		To: to, ExpectedVersion: capture.Version, Actor: "worker-1",
	})
	if err != nil {
		t.Fatalf("transition %s -> %s: %v", capture.State, to, err)
	}
	return updated
}

func TestHistoryCaptureMigrationSchemaAndImmutableAttribution(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()

	for _, name := range []string{
		"history_captures", "history_capture_expected_artifacts", "history_artifacts",
		"history_transcript_streams", "history_transcript_segments", "harness_archive_members",
		"history_checkpoint_hints", "history_capture_events", "history_upload_events",
	} {
		var count int
		if err := env.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count=%d err=%v", name, count, err)
		}
	}

	reserved := reserveHistoryCapture(t, env.service, baseHistoryReservation())
	if _, err := env.store.DB().ExecContext(ctx, `UPDATE history_captures SET project_id = 'other' WHERE id = ?`, reserved.Capture.ID); err == nil || !strings.Contains(err.Error(), "attribution is immutable") {
		t.Fatalf("immutable attribution update err = %v", err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `UPDATE history_captures SET error_message = ? WHERE id = ?`, strings.Repeat("x", 1025), reserved.Capture.ID); err == nil {
		t.Fatal("oversized bounded error update unexpectedly succeeded")
	}
	if _, err := env.store.DB().ExecContext(ctx, `DELETE FROM history_captures WHERE id = ?`, reserved.Capture.ID); err == nil {
		t.Fatal("retained history capture delete unexpectedly succeeded")
	}
}

func TestHistoryCaptureConcurrentReservationIsIdempotentAndGrantIsHashOnly(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()

	const callers = 8
	results := make(chan ReserveHistoryCaptureResult, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := env.service.Reserve(ctx, input)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reservation: %v", err)
		}
	}
	created := 0
	captureID := ""
	grant := ""
	for result := range results {
		if captureID == "" {
			captureID = result.Capture.ID
		} else if result.Capture.ID != captureID {
			t.Fatalf("reservation capture id = %q, want %q", result.Capture.ID, captureID)
		}
		if result.Created {
			created++
			grant = result.UploadGrant
		} else if result.UploadGrant != "" {
			t.Fatal("idempotent reservation revealed raw grant")
		}
	}
	if created != 1 || grant == "" {
		t.Fatalf("created reservations=%d grant=%q, want one nonempty grant", created, grant)
	}

	var storedHash string
	if err := env.store.DB().QueryRowContext(ctx, `SELECT upload_grant_hash FROM history_captures WHERE id = ?`, captureID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == grant || storedHash != hashHistoryGrant(grant) || strings.Contains(storedHash, grant) {
		t.Fatalf("stored grant value = %q, raw grant was persisted or hash differs", storedHash)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, captureID, grant); err != nil {
		t.Fatalf("authenticate correct grant: %v", err)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, captureID, "wrong"); !errors.Is(err, ErrHistoryUnauthorized) {
		t.Fatalf("wrong grant err=%v, want unauthorized", err)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, "hc-00000000000000000000000000000000", grant); !errors.Is(err, ErrHistoryUnauthorized) {
		t.Fatalf("unknown capture err=%v, want same unauthorized result", err)
	}

	conflicting := input
	conflicting.Role = "reviewer"
	if _, err := env.service.Reserve(ctx, conflicting); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("changed attribution retry err=%v, want conflict", err)
	}
}

func TestHistoryCaptureTransitionsVersionsAndExecutionVerdictAreIndependent(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	reserved := reserveHistoryCapture(t, env.service, baseHistoryReservation())

	if _, err := env.service.Transition(ctx, reserved.Capture.ID, TransitionHistoryCaptureInput{
		To: HistoryCaptureSealed, ExpectedVersion: reserved.Capture.Version, Actor: "worker",
	}); !errors.Is(err, ErrHistoryInvalidTransition) {
		t.Fatalf("illegal transition err=%v, want invalid transition", err)
	}
	running := transitionHistory(t, env.service, reserved.Capture, HistoryCaptureRunning)
	if _, err := env.service.Transition(ctx, running.ID, TransitionHistoryCaptureInput{
		To: HistoryCaptureQuiescing, ExpectedVersion: running.Version - 1, Actor: "worker",
	}); !errors.Is(err, ErrHistoryVersionConflict) {
		t.Fatalf("stale transition err=%v, want version conflict", err)
	}

	exitCode := 0
	verdict, err := env.service.RecordExecutionVerdict(ctx, running.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionSucceeded, ExitCode: &exitCode,
		ExpectedVersion: running.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("record execution verdict: %v", err)
	}
	if verdict.State != HistoryCaptureRunning || verdict.ExecutionVerdict != HistoryExecutionSucceeded {
		t.Fatalf("capture after verdict = %+v, state and verdict must remain independent", verdict)
	}
	blocked, err := env.service.MarkBlocked(ctx, verdict.ID, verdict.Version, "worker", "store_down", "history storage unavailable")
	if err != nil {
		t.Fatalf("mark blocked: %v", err)
	}
	if blocked.ExecutionVerdict != HistoryExecutionSucceeded || blocked.State != HistoryCaptureBlocked {
		t.Fatalf("blocked capture = %+v, successful verdict was changed", blocked)
	}
	if _, err := env.service.RecordExecutionVerdict(ctx, blocked.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionFailed, ExpectedVersion: blocked.Version, Actor: "worker",
	}); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("changed execution verdict err=%v, want immutable conflict", err)
	}
}

type publishResponseLossStore struct {
	blob.Store
	mu       sync.Mutex
	failOnce bool
}

func (s *publishResponseLossStore) Publish(ctx context.Context, temporary blob.Temporary, key blob.Key) (blob.Object, error) {
	object, err := s.Store.Publish(ctx, temporary, key)
	if err != nil {
		return object, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOnce {
		s.failOnce = false
		return blob.Object{}, errors.New("simulated response loss after immutable publish")
	}
	return object, nil
}

func TestHistoryArtifactImmutableIdempotencyAndPublicationRecovery(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	reserved := reserveHistoryCapture(t, env.service, baseHistoryReservation())
	input := PublishHistoryArtifactInput{
		LogicalKey: "harness/final/root-1", Kind: HistoryArtifactHarnessRoot,
		Phase: HistoryArtifactFinal, ArchiveID: "root-1", MediaType: "application/gzip",
		LogicalSize: 12, EntryCount: 2,
	}
	content := []byte("archive-one")
	temporary := uploadHistoryBytes(t, env.service, reserved.Capture.ID, reserved.UploadGrant, content)
	artifact, err := env.service.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, input, temporary)
	if err != nil {
		t.Fatalf("publish final Harness artifact: %v", err)
	}
	if artifact.SHA256 != historyDigest(content) || artifact.StoredSize != int64(len(content)) {
		t.Fatalf("published metadata = %+v", artifact)
	}
	again, err := env.service.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, input, temporary)
	if err != nil || again.ID != artifact.ID {
		t.Fatalf("same logical key/digest/size retry artifact=%+v err=%v", again, err)
	}

	different := uploadHistoryBytes(t, env.service, reserved.Capture.ID, reserved.UploadGrant, []byte("different"))
	if _, err := env.service.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, input, different); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("different bytes under immutable logical key err=%v, want conflict", err)
	}

	lossy := &publishResponseLossStore{Store: env.blobs, failOnce: true}
	lossyService := NewHistoryCaptureService(env.store.DB(), lossy)
	lossInput := input
	lossInput.LogicalKey = "harness/final/root-2"
	lossInput.ArchiveID = "root-2"
	lossContent := []byte("published-before-response-loss")
	lossTemporary := uploadHistoryBytes(t, lossyService, reserved.Capture.ID, reserved.UploadGrant, lossContent)
	if _, err := lossyService.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, lossInput, lossTemporary); err == nil {
		t.Fatal("simulated publication response loss unexpectedly succeeded")
	}
	pending, err := lossyService.GetArtifact(ctx, reserved.Capture.ID, lossInput.LogicalKey)
	if err != nil || pending.PublicationState != HistoryPublicationPending {
		t.Fatalf("artifact after response loss = %+v err=%v, want relational pending", pending, err)
	}
	recovered, err := lossyService.ReconcileArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, lossInput.LogicalKey)
	if err != nil || recovered.PublicationState != HistoryPublicationCommitted || recovered.SHA256 != historyDigest(lossContent) {
		t.Fatalf("reconciled artifact = %+v err=%v", recovered, err)
	}
}

func TestHistoryTranscriptOrderingSealAndExactExpectedCompletion(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	reserved := reserveHistoryCapture(t, env.service, baseHistoryReservation())
	capture := transitionHistory(t, env.service, reserved.Capture, HistoryCaptureRunning)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureQuiescing)

	segmentInput := func(key string, size int64) PublishHistoryArtifactInput {
		return PublishHistoryArtifactInput{
			LogicalKey: key, Kind: HistoryArtifactTranscriptSegment, Phase: HistoryArtifactFinal,
			MediaType: "application/octet-stream", LogicalSize: size,
		}
	}
	publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, segmentInput("transcript/0/0", 3), []byte("abc"))
	first, err := env.service.RegisterTranscriptSegment(ctx, capture.ID, reserved.UploadGrant, RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: "transcript/0/0", Epoch: 0, Sequence: 0, StartOffset: 0, EndOffset: 3, Encoding: "identity",
	})
	if err != nil || first.StartOffset != 0 || first.EndOffset != 3 {
		t.Fatalf("register first segment = %+v err=%v", first, err)
	}
	publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, segmentInput("transcript/0/1", 3), []byte("def"))
	if _, err := env.service.RegisterTranscriptSegment(ctx, capture.ID, reserved.UploadGrant, RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: "transcript/0/1", Epoch: 0, Sequence: 2, StartOffset: 3, EndOffset: 6, Encoding: "identity",
	}); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("out-of-order sequence err=%v, want conflict", err)
	}
	if _, err := env.service.RegisterTranscriptSegment(ctx, capture.ID, reserved.UploadGrant, RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: "transcript/0/1", Epoch: 0, Sequence: 1, StartOffset: 4, EndOffset: 7, Encoding: "identity",
	}); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("noncontiguous offset err=%v, want conflict", err)
	}
	if _, err := env.service.RegisterTranscriptSegment(ctx, capture.ID, reserved.UploadGrant, RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: "transcript/0/1", Epoch: 0, Sequence: 1, StartOffset: 3, EndOffset: 6, Encoding: "identity",
	}); err != nil {
		t.Fatalf("register second segment: %v", err)
	}
	seal := TranscriptSeal{FinalEpoch: 0, SegmentCount: 2, LogicalLength: 6, SHA256: historyDigest([]byte("abcdef"))}
	badSeal := seal
	badSeal.LogicalLength = 7
	if err := env.service.SealTranscript(ctx, capture.ID, reserved.UploadGrant, badSeal); !errors.Is(err, ErrHistoryIncomplete) {
		t.Fatalf("mismatched seal err=%v, want incomplete", err)
	}
	if err := env.service.SealTranscript(ctx, capture.ID, reserved.UploadGrant, seal); err != nil {
		t.Fatalf("seal transcript: %v", err)
	}

	expected := []FinalArtifactExpectation{{LogicalKey: "harness/final/root-1", Kind: HistoryArtifactHarnessRoot}}
	capture, err = env.service.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts: expected, TranscriptSeal: &seal, ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("declare exact final set: %v", err)
	}
	capture = transitionHistory(t, env.service, capture, HistoryCaptureSealed)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureUploading)
	if _, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker"); !errors.Is(err, ErrHistoryIncomplete) {
		t.Fatalf("complete without exact Harness final err=%v, want incomplete", err)
	}
	publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "harness/final/root-1", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
		ArchiveID: "root-1", MediaType: "application/gzip", LogicalSize: 4, EntryCount: 1,
	}, []byte("root"))
	completed, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil {
		t.Fatalf("complete exact capture: %v", err)
	}
	if completed.State != HistoryCaptureComplete || completed.ExecutionVerdict != HistoryExecutionPending {
		t.Fatalf("completed capture = %+v", completed)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, capture.ID, reserved.UploadGrant); !errors.Is(err, ErrHistoryGrantNoLongerUsable) {
		t.Fatalf("completed upload grant err=%v, want unusable", err)
	}
	if again, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, completed.Version-1, "worker"); err != nil || again.State != HistoryCaptureComplete {
		t.Fatalf("idempotent completion after grant revocation capture=%+v err=%v", again, err)
	}
}

func TestHistoryCheckpointHintsCoalesceAndCheckpointArtifactsNeverCompleteFinalSet(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()
	input.ExpectedTranscript = false
	input.ExpectedHarness = true
	reserved := reserveHistoryCapture(t, env.service, input)
	capture := transitionHistory(t, env.service, reserved.Capture, HistoryCaptureRunning)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureQuiescing)

	for _, hint := range []CheckpointHintInput{
		{SourceEvent: "Stop", Count: 1, DirtyGeneration: 1, WorkerOutcome: "segment_requested"},
		{SourceEvent: "Stop", Count: 9, CoalescedCount: 8, DirtyGeneration: 2, WorkerOutcome: "coalesced"},
	} {
		if _, err := env.service.RecordCheckpointHint(ctx, capture.ID, reserved.UploadGrant, hint); err != nil {
			t.Fatalf("record checkpoint hint: %v", err)
		}
	}
	hint, err := env.service.GetCheckpointHint(ctx, capture.ID, "Stop")
	if err != nil || hint.HintCount != 10 || hint.CoalescedCount != 8 || hint.DirtyGeneration != 2 || hint.Version != 1 {
		t.Fatalf("coalesced hint = %+v err=%v", hint, err)
	}
	var hintRows int
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM history_checkpoint_hints WHERE capture_id = ?`, capture.ID).Scan(&hintRows); err != nil || hintRows != 1 {
		t.Fatalf("checkpoint hint rows=%d err=%v, want one aggregate", hintRows, err)
	}

	checkpointInput := func(generation int64) PublishHistoryArtifactInput {
		return PublishHistoryArtifactInput{
			LogicalKey: "harness/checkpoint/root-1/" + string(rune('0'+generation)),
			Kind:       HistoryArtifactHarnessRoot, Phase: HistoryArtifactCheckpoint,
			CheckpointGeneration: generation, CheckpointTrigger: "Stop", ArchiveID: "root-1",
			MediaType: "application/gzip", LogicalSize: generation, EntryCount: 1,
		}
	}
	first := publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, checkpointInput(1), []byte("one"))
	second := publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, checkpointInput(2), []byte("two"))
	var supersededBy string
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COALESCE(superseded_by_artifact_id, '') FROM history_artifacts WHERE id = ?`, first.ID).Scan(&supersededBy); err != nil || supersededBy != second.ID {
		t.Fatalf("checkpoint 1 superseded_by=%q err=%v, want %q", supersededBy, err, second.ID)
	}

	capture, err = env.service.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts:       []FinalArtifactExpectation{{LogicalKey: "harness/final/root-1", Kind: HistoryArtifactHarnessRoot}},
		ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("declare final set: %v", err)
	}
	capture = transitionHistory(t, env.service, capture, HistoryCaptureSealed)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureUploading)
	if _, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker"); !errors.Is(err, ErrHistoryIncomplete) {
		t.Fatalf("checkpoint satisfied final completeness err=%v, want incomplete", err)
	}
	publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "harness/final/root-1", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
		ArchiveID: "root-1", MediaType: "application/gzip", LogicalSize: 5, EntryCount: 1,
	}, []byte("final"))
	completed, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil || completed.State != HistoryCaptureComplete {
		t.Fatalf("complete after final root capture=%+v err=%v", completed, err)
	}
	if completed.LastCheckpointCommittedGeneration != 2 {
		t.Fatalf("last checkpoint generation=%d, want 2", completed.LastCheckpointCommittedGeneration)
	}
}

func TestHistoryCaptureEventsAreAppendOnlyAndLossWaiverAreAudited(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()
	input.ExpectedTranscript = false
	input.ExpectedHarness = false
	lostReservation := reserveHistoryCapture(t, env.service, input)
	lost, err := env.service.MarkLost(ctx, lostReservation.Capture.ID, lostReservation.Capture.Version, "watchdog", "node_lost", "worker node disappeared")
	if err != nil || lost.State != HistoryCaptureLost {
		t.Fatalf("mark lost capture=%+v err=%v", lost, err)
	}
	recovered, err := env.service.Transition(ctx, lost.ID, TransitionHistoryCaptureInput{
		To: HistoryCaptureUploading, ExpectedVersion: lost.Version, Actor: "worker-recovery",
	})
	if err != nil || recovered.State != HistoryCaptureUploading {
		t.Fatalf("recover lost capture=%+v err=%v", recovered, err)
	}

	waiveInput := input
	waiveInput.JobID, waiveInput.LeaseID, waiveInput.LeaseAttempt = "job-2", "lease-2", 2
	waivedReservation := reserveHistoryCapture(t, env.service, waiveInput)
	waived, err := env.service.Waive(ctx, waivedReservation.Capture.ID, waivedReservation.Capture.Version, "owner", "execution workspace was irrecoverably lost")
	if err != nil || waived.State != HistoryCaptureWaived || waived.WaiverReason == "" {
		t.Fatalf("waive capture=%+v err=%v", waived, err)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, waived.ID, waivedReservation.UploadGrant); !errors.Is(err, ErrHistoryGrantNoLongerUsable) {
		t.Fatalf("waived grant err=%v, want unusable", err)
	}

	events, err := env.service.ListEvents(ctx, waived.ID)
	if err != nil || len(events) != 2 || events[0].EventKind != "reserved" || events[1].EventKind != "waived" {
		t.Fatalf("waiver events=%+v err=%v", events, err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `UPDATE history_capture_events SET actor = 'tampered' WHERE id = ?`, events[0].ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("capture event update err=%v, want append-only rejection", err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `DELETE FROM history_capture_events WHERE id = ?`, events[0].ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("capture event delete err=%v, want append-only rejection", err)
	}

	// Upload events have the same database-level append-only contract.
	var uploadEventID string
	artifactInput := baseHistoryReservation()
	artifactInput.JobID, artifactInput.LeaseID, artifactInput.LeaseAttempt = "job-3", "lease-3", 3
	artifactInput.ExpectedTranscript, artifactInput.ExpectedHarness = false, true
	artifactCapture := reserveHistoryCapture(t, env.service, artifactInput)
	publishHistoryBytes(t, env.service, artifactCapture.Capture.ID, artifactCapture.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "harness/final/root", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
		ArchiveID: "root", MediaType: "application/gzip", LogicalSize: 1, EntryCount: 1,
	}, []byte("x"))
	if err := env.store.DB().QueryRowContext(ctx, `SELECT id FROM history_upload_events WHERE capture_id = ? ORDER BY occurred_at LIMIT 1`, artifactCapture.Capture.ID).Scan(&uploadEventID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `DELETE FROM history_upload_events WHERE id = ?`, uploadEventID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("upload event delete err=%v, want append-only rejection", err)
	}
}

// Compile-time guard that the test response-loss wrapper still uses the complete
// production Store contract rather than a fake subset.
var _ blob.Store = (*publishResponseLossStore)(nil)
var _ io.Writer = (blob.Upload)(nil)
