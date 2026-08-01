package coordinator

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	t.Cleanup(func() { _ = blobs.Close() })
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

func publishCanonicalManifestBytes(t *testing.T, service *HistoryCaptureService, captureID string, input PublishHistoryArtifactInput, content []byte) HistoryArtifact {
	t.Helper()
	upload, err := service.BeginCoordinatorUpload(context.Background(), captureID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write(content); err != nil {
		t.Fatal(err)
	}
	temporary, err := upload.Complete(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := service.PublishCanonicalManifest(context.Background(), captureID, input, temporary)
	if err != nil {
		t.Fatalf("publish canonical manifest: %v", err)
	}
	return artifact
}

func gzipHistoryBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
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
	created, rotated := 0, 0
	captureID := ""
	var grants []string
	for result := range results {
		if captureID == "" {
			captureID = result.Capture.ID
		} else if result.Capture.ID != captureID {
			t.Fatalf("reservation capture id = %q, want %q", result.Capture.ID, captureID)
		}
		if result.Created {
			created++
		} else if result.GrantRotated {
			rotated++
		}
		if result.UploadGrant == "" {
			t.Fatal("reservation retry did not reissue a grant")
		}
		grants = append(grants, result.UploadGrant)
	}
	if created != 1 || rotated != callers-1 {
		t.Fatalf("created=%d rotated=%d, want 1/%d", created, rotated, callers-1)
	}

	var storedHash string
	if err := env.store.DB().QueryRowContext(ctx, `SELECT upload_grant_hash FROM history_captures WHERE id = ?`, captureID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	current := ""
	for _, grant := range grants {
		if storedHash == hashHistoryGrant(grant) {
			current = grant
			continue
		}
		if err := env.service.AuthenticateUploadGrant(ctx, captureID, grant); !errors.Is(err, ErrHistoryUnauthorized) {
			t.Fatalf("superseded grant err=%v, want unauthorized", err)
		}
	}
	if current == "" || storedHash == current || strings.Contains(storedHash, current) {
		t.Fatalf("stored grant value = %q, no current hashed grant", storedHash)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, captureID, current); err != nil {
		t.Fatalf("authenticate current grant: %v", err)
	}
	var rotations int
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM history_capture_events WHERE capture_id = ? AND event_kind = 'upload_grant_rotated'`, captureID).Scan(&rotations); err != nil || rotations != callers-1 {
		t.Fatalf("rotation events=%d err=%v", rotations, err)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, captureID, "wrong"); !errors.Is(err, ErrHistoryUnauthorized) {
		t.Fatalf("wrong grant err=%v, want unauthorized", err)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, "hc-00000000000000000000000000000000", current); !errors.Is(err, ErrHistoryUnauthorized) {
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
	if retry, err := env.service.MarkBlocked(ctx, verdict.ID, verdict.Version, "worker", "store_down", "history storage unavailable"); err != nil || retry.Version != blocked.Version {
		t.Fatalf("same transition response-loss retry=%+v err=%v", retry, err)
	}
	if _, err := env.service.MarkBlocked(ctx, verdict.ID, verdict.Version, "worker", "store_down", "different"); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("conflicting achieved transition err=%v, want conflict", err)
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

type blockingOpenStore struct {
	blob.Store
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (s *blockingOpenStore) blockNextOpen() (<-chan struct{}, chan<- struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered = make(chan struct{})
	s.release = make(chan struct{})
	return s.entered, s.release
}

func (s *blockingOpenStore) Open(ctx context.Context, key blob.Key) (io.ReadCloser, error) {
	s.mu.Lock()
	entered, release := s.entered, s.release
	if entered != nil {
		s.entered, s.release = nil, nil
	}
	s.mu.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.Open(ctx, key)
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

type failingPublishStore struct{ blob.Store }

func (s failingPublishStore) Publish(context.Context, blob.Temporary, blob.Key) (blob.Object, error) {
	return blob.Object{}, errors.New("simulated persistent publication failure")
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
	if _, err := lossyService.MarkLost(ctx, reserved.Capture.ID, reserved.Capture.Version, "watchdog", "worker_lost", "worker disappeared"); err != nil {
		t.Fatalf("mark worker lost: %v", err)
	}
	summary, err := lossyService.ReconcilePendingArtifacts(ctx, reserved.Capture.ID, 10)
	if err != nil || summary.Examined != 1 || summary.Committed != 1 {
		t.Fatalf("grantless reconciliation summary=%+v err=%v", summary, err)
	}
	recovered, err := lossyService.GetArtifact(ctx, reserved.Capture.ID, lossInput.LogicalKey)
	if err != nil || recovered.PublicationState != HistoryPublicationCommitted || recovered.SHA256 != historyDigest(lossContent) {
		t.Fatalf("reconciled artifact = %+v err=%v", recovered, err)
	}
}

func TestHistoryPendingReconciliationRotatesFailuresForFairness(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	service := NewHistoryCaptureService(env.store.DB(), failingPublishStore{Store: env.blobs})
	reserved := reserveHistoryCapture(t, service, baseHistoryReservation())
	for index, key := range []string{"harness/final/one", "harness/final/two", "harness/final/three"} {
		temporary := uploadHistoryBytes(t, service, reserved.Capture.ID, reserved.UploadGrant, []byte{byte('a' + index)})
		_, err := service.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
			LogicalKey: key, Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
			ArchiveID: key, MediaType: "application/octet-stream", LogicalSize: 1, EntryCount: 1,
		}, temporary)
		if err == nil {
			t.Fatalf("publication %s unexpectedly succeeded", key)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		summary, err := service.ReconcilePendingArtifacts(ctx, reserved.Capture.ID, 1)
		if err == nil || summary.Examined != 1 || len(summary.Failures) != 1 {
			t.Fatalf("reconcile attempt %d summary=%+v err=%v", attempt, summary, err)
		}
	}
	var attempted int
	if err := env.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM history_artifacts
WHERE capture_id = ? AND reconcile_attempted_at IS NOT NULL`, reserved.Capture.ID).Scan(&attempted); err != nil {
		t.Fatal(err)
	}
	if attempted != 2 {
		t.Fatalf("reconciliation attempted %d distinct pending artifacts, want 2", attempted)
	}
}

func TestHistoryPendingPublicationRemainsOutstandingForQuotas(t *testing.T) {
	tests := []struct {
		name       string
		maxUploads int
		maxBytes   int64
	}{
		{name: "count", maxUploads: 1, maxBytes: 10},
		{name: "bytes", maxUploads: 2, maxBytes: 1},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newHistoryCaptureTestEnv(t)
			ctx := context.Background()
			service := NewHistoryCaptureServiceWithOptions(env.store.DB(), failingPublishStore{Store: env.blobs}, HistoryCaptureServiceOptions{
				MaxOutstandingUploadsPerCapture:     test.maxUploads,
				MaxOutstandingUploadBytesPerCapture: test.maxBytes,
			})
			input := baseHistoryReservation()
			input.JobID = fmt.Sprintf("job-pending-quota-%d", index)
			input.LeaseID = fmt.Sprintf("lease-pending-quota-%d", index)
			input.LeaseAttempt = int64(index + 20)
			reserved := reserveHistoryCapture(t, service, input)
			first := uploadHistoryBytes(t, service, reserved.Capture.ID, reserved.UploadGrant, []byte("a"))
			_, err := service.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
				LogicalKey: "harness/final/pending", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
				ArchiveID: "pending", MediaType: "application/octet-stream", LogicalSize: 1,
			}, first)
			if err == nil {
				t.Fatal("persistent publication failure unexpectedly succeeded")
			}
			var intentState, publicationState string
			if err := env.store.DB().QueryRowContext(ctx, `
SELECT intent.state, artifact.publication_state
FROM history_upload_intents intent
JOIN history_artifacts artifact ON artifact.id = intent.artifact_id
WHERE intent.temporary_upload_id = ?`, first.ID).Scan(&intentState, &publicationState); err != nil {
				t.Fatal(err)
			}
			if intentState != "consumed" || publicationState != "pending" {
				t.Fatalf("pending publication state = intent:%q artifact:%q", intentState, publicationState)
			}

			second, err := service.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := second.Write([]byte("b")); err != nil {
				t.Fatal(err)
			}
			if _, err := second.Complete(ctx); !errors.Is(err, ErrHistoryUploadTooLarge) {
				t.Fatalf("second completed upload err = %v, want outstanding quota", err)
			}
		})
	}
}

func TestHarnessArchiveMemberRegistrationEnforcesDeclaredAndConfiguredLimits(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	service := NewHistoryCaptureServiceWithOptions(env.store.DB(), env.blobs, HistoryCaptureServiceOptions{
		MaxArchiveEntries: 3, MaxArchivePathBytes: 256,
	})
	reserved := reserveHistoryCapture(t, service, baseHistoryReservation())
	publishHistoryBytes(t, service, reserved.Capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "harness/final/indexed", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
		ArchiveID: "indexed", MediaType: "application/octet-stream", LogicalSize: 4, EntryCount: 1,
	}, []byte("root"))
	member := HarnessArchiveMemberInput{
		RelativeMemberPath: "sessions/root.json", MemberKind: "root", Status: "complete", ParseStatus: "parsed",
	}
	if err := service.RegisterHarnessArchiveMembers(ctx, reserved.Capture.ID, reserved.UploadGrant, "harness/final/indexed", []HarnessArchiveMemberInput{member}); err != nil {
		t.Fatalf("register declared member: %v", err)
	}
	second := member
	second.RelativeMemberPath = "sessions/second.json"
	if err := service.RegisterHarnessArchiveMembers(ctx, reserved.Capture.ID, reserved.UploadGrant, "harness/final/indexed", []HarnessArchiveMemberInput{second}); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("incremental registration beyond declared count err = %v", err)
	}
	if err := service.RegisterHarnessArchiveMembers(ctx, reserved.Capture.ID, reserved.UploadGrant, "harness/final/indexed", []HarnessArchiveMemberInput{member, second}); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("bulk registration beyond declared count err = %v", err)
	}
	longPath := member
	longPath.RelativeMemberPath = strings.Repeat("x", 257)
	if err := service.RegisterHarnessArchiveMembers(ctx, reserved.Capture.ID, reserved.UploadGrant, "harness/final/indexed", []HarnessArchiveMemberInput{longPath}); err == nil || !strings.Contains(err.Error(), "member path") {
		t.Fatalf("overlong configured member path err = %v", err)
	}
	var registered int
	if err := env.store.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM harness_archive_members WHERE capture_id = ?`, reserved.Capture.ID).Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if registered != 1 {
		t.Fatalf("registered archive members = %d, want 1", registered)
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

	expected := []FinalArtifactExpectation{
		{LogicalKey: "harness/final/root-1", Kind: HistoryArtifactHarnessRoot},
		{LogicalKey: "manifest/final", Kind: HistoryArtifactManifest},
	}
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
	publishCanonicalManifestBytes(t, env.service, capture.ID, PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: HistoryArtifactManifest, Phase: HistoryArtifactFinal,
		MediaType: "application/json", LogicalSize: 2,
	}, []byte("{}"))
	exitCode := 0
	capture, err = env.service.RecordExecutionVerdict(ctx, capture.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionSucceeded, ExitCode: &exitCode, ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("record succeeded verdict: %v", err)
	}
	completed, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil {
		t.Fatalf("complete exact capture: %v", err)
	}
	if completed.State != HistoryCaptureComplete || completed.ExecutionVerdict != HistoryExecutionSucceeded {
		t.Fatalf("completed capture = %+v", completed)
	}
	if err := env.service.AuthenticateUploadGrant(ctx, capture.ID, reserved.UploadGrant); !errors.Is(err, ErrHistoryGrantNoLongerUsable) {
		t.Fatalf("completed upload grant err=%v, want unusable", err)
	}
	if again, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, completed.Version-1, "worker"); err != nil || again.State != HistoryCaptureComplete {
		t.Fatalf("idempotent completion after grant revocation capture=%+v err=%v", again, err)
	}
}

func TestHistoryTranscriptBlobReadsDoNotHoldProjectWriteReservation(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	reserved := reserveHistoryCapture(t, env.service, baseHistoryReservation())
	content := []byte("slow transcript")
	publishHistoryBytes(t, env.service, reserved.Capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "transcript/slow", Kind: HistoryArtifactTranscriptSegment, Phase: HistoryArtifactFinal,
		MediaType: "application/octet-stream", LogicalSize: int64(len(content)),
	}, content)

	blocking := &blockingOpenStore{Store: env.blobs}
	service := NewHistoryCaptureService(env.store.DB(), blocking)
	assertWriteProceeds := func(name string, operation func() error) {
		t.Helper()
		entered, release := blocking.blockNextOpen()
		done := make(chan error, 1)
		go func() { done <- operation() }()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not reach blob read", name)
		}
		writeDone := make(chan error, 1)
		go func() {
			input := baseHistoryReservation()
			input.JobID, input.LeaseID, input.LeaseAttempt = "job-write-"+name, "lease-write-"+name, 20
			_, err := env.service.Reserve(ctx, input)
			writeDone <- err
		}()
		var writeErr error
		writeTimedOut := false
		select {
		case writeErr = <-writeDone:
		case <-time.After(time.Second):
			writeTimedOut = true
		}
		close(release)
		if writeTimedOut {
			t.Fatalf("unrelated project write blocked by %s blob read", name)
		}
		if writeErr != nil {
			t.Fatalf("unrelated project write during %s: %v", name, writeErr)
		}
		if err := <-done; err != nil {
			t.Fatalf("%s after release: %v", name, err)
		}
	}

	assertWriteProceeds("register", func() error {
		_, err := service.RegisterTranscriptSegment(ctx, reserved.Capture.ID, reserved.UploadGrant, RegisterTranscriptSegmentInput{
			ArtifactLogicalKey: "transcript/slow", Epoch: 0, Sequence: 0,
			StartOffset: 0, EndOffset: int64(len(content)), Encoding: "identity",
		})
		return err
	})
	assertWriteProceeds("seal", func() error {
		return service.SealTranscript(ctx, reserved.Capture.ID, reserved.UploadGrant, TranscriptSeal{
			FinalEpoch: 0, SegmentCount: 1, LogicalLength: int64(len(content)), SHA256: historyDigest(content),
		})
	})
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
			CheckpointGeneration: generation, CheckpointTrigger: "Stop", CheckpointStream: "root-1", ArchiveID: "root-1",
			MediaType: "application/gzip", LogicalSize: generation, EntryCount: 1,
		}
	}
	first := publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, checkpointInput(1), []byte("one"))
	otherInput := checkpointInput(2)
	otherInput.LogicalKey, otherInput.CheckpointStream, otherInput.ArchiveID = "harness/checkpoint/root-2/2", "root-2", "root-2"
	other := publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, otherInput, []byte("other"))
	second := publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, checkpointInput(2), []byte("two"))
	var supersededBy string
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COALESCE(superseded_by_artifact_id, '') FROM history_artifacts WHERE id = ?`, first.ID).Scan(&supersededBy); err != nil || supersededBy != second.ID {
		t.Fatalf("checkpoint 1 superseded_by=%q err=%v, want %q", supersededBy, err, second.ID)
	}
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COALESCE(superseded_by_artifact_id, '') FROM history_artifacts WHERE id = ?`, other.ID).Scan(&supersededBy); err != nil || supersededBy != "" {
		t.Fatalf("interleaved root checkpoint was incorrectly superseded: %q err=%v", supersededBy, err)
	}

	capture, err = env.service.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts: []FinalArtifactExpectation{
			{LogicalKey: "harness/final/root-1", Kind: HistoryArtifactHarnessRoot},
			{LogicalKey: "manifest/final", Kind: HistoryArtifactManifest},
		},
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
	manifest := publishCanonicalManifestBytes(t, env.service, capture.ID, PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: HistoryArtifactManifest, Phase: HistoryArtifactFinal,
		MediaType: "application/json", LogicalSize: 2,
	}, []byte("{}"))
	exitCode := 0
	capture, err = env.service.RecordExecutionVerdict(ctx, capture.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionSucceeded, ExitCode: &exitCode, ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil || completed.State != HistoryCaptureComplete {
		t.Fatalf("complete after final root capture=%+v err=%v", completed, err)
	}
	if completed.LastCheckpointCommittedGeneration != 2 {
		t.Fatalf("last checkpoint generation=%d, want 2", completed.LastCheckpointCommittedGeneration)
	}
	for _, checkpointID := range []string{first.ID, other.ID, second.ID} {
		if err := env.store.DB().QueryRowContext(ctx, `SELECT superseded_by_artifact_id FROM history_artifacts WHERE id = ?`, checkpointID).Scan(&supersededBy); err != nil || supersededBy != manifest.ID {
			t.Fatalf("checkpoint %s final supersession=%q err=%v", checkpointID, supersededBy, err)
		}
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

func TestHistorySQLiteGuardsRejectLifecycleAndTerminalMetadataCorruption(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()
	input.ExpectedTranscript, input.ExpectedHarness = false, false
	reserved := reserveHistoryCapture(t, env.service, input)
	captureID := reserved.Capture.ID
	now := "2026-07-31T12:00:00Z"

	for name, statement := range map[string]string{
		"state jump":       `UPDATE history_captures SET state = 'complete', completed_at = '` + now + `', upload_grant_revoked_at = '` + now + `', version = version + 1 WHERE id = '` + captureID + `'`,
		"version jump":     `UPDATE history_captures SET version = version + 2 WHERE id = '` + captureID + `'`,
		"future timestamp": `UPDATE history_captures SET sealed_at = '` + now + `' WHERE id = '` + captureID + `'`,
	} {
		if _, err := env.store.DB().ExecContext(ctx, statement); err == nil {
			t.Fatalf("SQLite guard accepted %s", name)
		}
	}
	if _, err := env.store.DB().ExecContext(ctx, `
UPDATE history_captures
SET expected_set_declared_at = ?, expected_final_artifact_count = 0,
    version = version + 1, updated_at = ?
WHERE id = ?`, now, now, captureID); err != nil {
		t.Fatalf("declare expected-set projection directly: %v", err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `
UPDATE history_captures
SET expected_final_artifact_count = 1, version = version + 1, updated_at = ?
WHERE id = ?`, now, captureID); err == nil {
		t.Fatal("SQLite guard accepted expected-set mutation after declaration")
	}
	capture, err := env.service.Get(ctx, captureID)
	if err != nil {
		t.Fatal(err)
	}
	capture, err = env.service.RecordExecutionVerdict(ctx, captureID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionFailed, ErrorCode: "failed", ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("record execution verdict: %v", err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `
UPDATE history_captures
SET execution_error_code = 'rewritten', version = version + 1, updated_at = ?
WHERE id = ?`, now, captureID); err == nil {
		t.Fatal("SQLite guard accepted terminal execution verdict rewrite")
	}

	artifact := publishHistoryBytes(t, env.service, captureID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "harness/final/guard", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
		ArchiveID: "guard", MediaType: "application/octet-stream", LogicalSize: 1, EntryCount: 1,
	}, []byte("x"))
	if _, err := env.store.DB().ExecContext(ctx, `
UPDATE history_artifacts
SET publication_state = 'pending', committed_at = NULL
WHERE id = ?`, artifact.ID); err == nil {
		t.Fatal("SQLite guard accepted committed-to-pending artifact transition")
	}
}

func TestHistoryUploadIntentLifecycleLossWaiverAndCeilings(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()
	input.ExpectedTranscript, input.ExpectedHarness = false, false
	reserved := reserveHistoryCapture(t, env.service, input)
	temporary := uploadHistoryBytes(t, env.service, reserved.Capture.ID, reserved.UploadGrant, []byte("outbox"))
	if err := env.service.HeartbeatUpload(ctx, reserved.Capture.ID, reserved.UploadGrant, temporary.ID); err != nil {
		t.Fatalf("heartbeat active upload: %v", err)
	}
	lost, err := env.service.MarkLost(ctx, reserved.Capture.ID, reserved.Capture.Version, "watchdog", "worker_lost", "gone")
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := env.store.DB().QueryRowContext(ctx, `SELECT state FROM history_upload_intents WHERE temporary_upload_id = ?`, temporary.ID).Scan(&state); err != nil || state != "active" {
		t.Fatalf("lost intent state=%q err=%v, want active", state, err)
	}
	if _, err := env.blobs.Resume(ctx, temporary.ID); err != nil {
		t.Fatalf("lost upload was not preserved: %v", err)
	}
	waived, err := env.service.Waive(ctx, lost.ID, lost.Version, "owner", "explicitly discard outbox")
	if err != nil || waived.State != HistoryCaptureWaived {
		t.Fatalf("waive capture=%+v err=%v", waived, err)
	}
	if err := env.store.DB().QueryRowContext(ctx, `SELECT state FROM history_upload_intents WHERE temporary_upload_id = ?`, temporary.ID).Scan(&state); err != nil || state != "abandoned" {
		t.Fatalf("waived intent state=%q err=%v", state, err)
	}
	if _, err := env.blobs.Resume(ctx, temporary.ID); !errors.Is(err, blob.ErrNotFound) && !errors.Is(err, blob.ErrUploadAborted) {
		t.Fatalf("waived temporary still resumable: %v", err)
	}

	limitedInput := input
	limitedInput.JobID, limitedInput.LeaseID, limitedInput.LeaseAttempt = "job-limit", "lease-limit", 9
	limited := NewHistoryCaptureServiceWithOptions(env.store.DB(), env.blobs, HistoryCaptureServiceOptions{
		MaxUploadBytes: 3, MaxTranscriptSegmentBytes: 3, MaxArtifactsPerCapture: 1, MaxCheckpointsPerCapture: 1,
		MaxOutstandingUploadsPerCapture: 1, MaxOutstandingUploadBytesPerCapture: 3,
		MaxArchiveEntries: 1, MaxArchiveLogicalBytes: 3,
	})
	limitedReservation := reserveHistoryCapture(t, limited, limitedInput)
	upload, err := limited.BeginUpload(ctx, limitedReservation.Capture.ID, limitedReservation.UploadGrant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write([]byte("four")); !errors.Is(err, ErrHistoryUploadTooLarge) {
		t.Fatalf("oversized upload err=%v", err)
	}
	if _, err := upload.Complete(ctx); !errors.Is(err, ErrHistoryUploadTooLarge) {
		t.Fatalf("complete oversized upload err=%v", err)
	}
	outstanding := uploadHistoryBytes(t, limited, limitedReservation.Capture.ID, limitedReservation.UploadGrant, []byte("one"))
	secondUpload, err := limited.BeginUpload(ctx, limitedReservation.Capture.ID, limitedReservation.UploadGrant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondUpload.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	if _, err := secondUpload.Complete(ctx); !errors.Is(err, ErrHistoryUploadTooLarge) {
		t.Fatalf("outstanding upload count/bytes err=%v, want quota error", err)
	}
	if err := limited.AbandonUpload(ctx, limitedReservation.Capture.ID, limitedReservation.UploadGrant, outstanding.ID); err != nil {
		t.Fatal(err)
	}
	archiveTooLarge := PublishHistoryArtifactInput{
		LogicalKey: "checkpoint/archive-too-large", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactCheckpoint,
		CheckpointGeneration: 1, CheckpointStream: "large", MediaType: "application/octet-stream", LogicalSize: 3, EntryCount: 2,
	}
	archiveTemporary := uploadHistoryBytes(t, limited, limitedReservation.Capture.ID, limitedReservation.UploadGrant, []byte("one"))
	if _, err := limited.PublishArtifact(ctx, limitedReservation.Capture.ID, limitedReservation.UploadGrant, archiveTooLarge, archiveTemporary); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("archive entry ceiling err=%v", err)
	}
	if err := limited.AbandonUpload(ctx, limitedReservation.Capture.ID, limitedReservation.UploadGrant, archiveTemporary.ID); err != nil {
		t.Fatal(err)
	}
	checkpoint := PublishHistoryArtifactInput{
		LogicalKey: "checkpoint/one", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactCheckpoint,
		CheckpointGeneration: 1, CheckpointStream: "root", MediaType: "application/octet-stream", LogicalSize: 3,
	}
	publishHistoryBytes(t, limited, limitedReservation.Capture.ID, limitedReservation.UploadGrant, checkpoint, []byte("one"))
	secondTemporary := uploadHistoryBytes(t, limited, limitedReservation.Capture.ID, limitedReservation.UploadGrant, []byte("two"))
	checkpoint.LogicalKey, checkpoint.CheckpointGeneration = "checkpoint/two", 2
	if _, err := limited.PublishArtifact(ctx, limitedReservation.Capture.ID, limitedReservation.UploadGrant, checkpoint, secondTemporary); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("artifact/checkpoint ceiling err=%v", err)
	}

	orderingInput := input
	orderingInput.JobID, orderingInput.LeaseID, orderingInput.LeaseAttempt = "job-order", "lease-order", 10
	ordering := NewHistoryCaptureServiceWithOptions(env.store.DB(), env.blobs, HistoryCaptureServiceOptions{
		MaxUploadBytes: 3, MaxArtifactsPerCapture: 10, MaxCheckpointsPerCapture: 10,
	})
	orderingReservation := reserveHistoryCapture(t, ordering, orderingInput)
	orderedCheckpoint := checkpoint
	orderedCheckpoint.LogicalKey, orderedCheckpoint.CheckpointGeneration = "checkpoint/two-first", 2
	publishHistoryBytes(t, ordering, orderingReservation.Capture.ID, orderingReservation.UploadGrant, orderedCheckpoint, []byte("two"))
	orderedCheckpoint.LogicalKey, orderedCheckpoint.CheckpointGeneration = "checkpoint/one-late", 1
	late := uploadHistoryBytes(t, ordering, orderingReservation.Capture.ID, orderingReservation.UploadGrant, []byte("one"))
	if _, err := ordering.PublishArtifact(ctx, orderingReservation.Capture.ID, orderingReservation.UploadGrant, orderedCheckpoint, late); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("non-increasing checkpoint generation err=%v", err)
	}
}

func TestHistoryTranscriptStrictGzipRawDigestAndServerSeal(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()
	input.ExpectedHarness = false
	reserved := reserveHistoryCapture(t, env.service, input)
	raw := []byte("raw transcript")
	compressed := gzipHistoryBytes(t, raw)
	publishHistoryBytes(t, env.service, reserved.Capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "transcript/gzip", Kind: HistoryArtifactTranscriptSegment, Phase: HistoryArtifactFinal,
		MediaType: "application/gzip", LogicalSize: int64(len(raw)),
	}, compressed)
	segment, err := env.service.RegisterTranscriptSegment(ctx, reserved.Capture.ID, reserved.UploadGrant, RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: "transcript/gzip", Epoch: 0, Sequence: 0, StartOffset: 0, EndOffset: int64(len(raw)), Encoding: "gzip",
	})
	if err != nil || segment.RawSHA256 != historyDigest(raw) {
		t.Fatalf("gzip segment=%+v err=%v", segment, err)
	}
	clearRawDigestAsLegacy := func() {
		t.Helper()
		connection, err := env.store.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var triggerSQL string
		if err := connection.QueryRowContext(ctx, `
SELECT sql FROM sqlite_master
WHERE type = 'trigger' AND name = 'history_transcript_segments_no_update'`).Scan(&triggerSQL); err != nil {
			t.Fatalf("read transcript immutability trigger: %v", err)
		}
		if _, err := connection.ExecContext(ctx, `DROP TRIGGER history_transcript_segments_no_update`); err != nil {
			t.Fatalf("drop transcript immutability trigger: %v", err)
		}
		if _, err := connection.ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx, `
UPDATE history_transcript_segments SET raw_sha256 = '' WHERE capture_id = ?`, reserved.Capture.ID); err != nil {
			t.Fatalf("simulate pre-0008 transcript segment: %v", err)
		}
		if _, err := connection.ExecContext(ctx, `PRAGMA ignore_check_constraints = OFF`); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx, triggerSQL); err != nil {
			t.Fatalf("restore transcript immutability trigger: %v", err)
		}
		if err := connection.Close(); err != nil {
			t.Fatalf("close legacy transcript setup connection: %v", err)
		}
	}
	clearRawDigestAsLegacy()
	legacyRetry, err := env.service.RegisterTranscriptSegment(ctx, reserved.Capture.ID, reserved.UploadGrant, RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: "transcript/gzip", Epoch: 0, Sequence: 0, StartOffset: 0, EndOffset: int64(len(raw)), Encoding: "gzip",
	})
	if err != nil || legacyRetry.RawSHA256 != historyDigest(raw) {
		t.Fatalf("verify legacy segment retry=%+v err=%v", legacyRetry, err)
	}
	clearRawDigestAsLegacy()
	badSeal := TranscriptSeal{FinalEpoch: 0, SegmentCount: 1, LogicalLength: int64(len(raw)), SHA256: historyDigest([]byte("producer lie"))}
	if err := env.service.SealTranscript(ctx, reserved.Capture.ID, reserved.UploadGrant, badSeal); !errors.Is(err, ErrHistoryIncomplete) {
		t.Fatalf("producer digest lie err=%v", err)
	}
	seal := badSeal
	seal.SHA256 = historyDigest(raw)
	if err := env.service.SealTranscript(ctx, reserved.Capture.ID, reserved.UploadGrant, seal); err != nil {
		t.Fatalf("server stream seal: %v", err)
	}
	var backfilledRawDigest string
	if err := env.store.DB().QueryRowContext(ctx, `
SELECT raw_sha256 FROM history_transcript_segments WHERE capture_id = ?`, reserved.Capture.ID).Scan(&backfilledRawDigest); err != nil {
		t.Fatal(err)
	}
	if backfilledRawDigest != historyDigest(raw) {
		t.Fatalf("sealed legacy segment raw digest = %q", backfilledRawDigest)
	}

	badInput := input
	badInput.JobID, badInput.LeaseID, badInput.LeaseAttempt = "job-gzip-bad", "lease-gzip-bad", 10
	bad := reserveHistoryCapture(t, env.service, badInput)
	withTrailing := append(gzipHistoryBytes(t, []byte("a")), gzipHistoryBytes(t, []byte("b"))...)
	publishHistoryBytes(t, env.service, bad.Capture.ID, bad.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "transcript/multi", Kind: HistoryArtifactTranscriptSegment, Phase: HistoryArtifactFinal,
		MediaType: "application/gzip", LogicalSize: 1,
	}, withTrailing)
	if _, err := env.service.RegisterTranscriptSegment(ctx, bad.Capture.ID, bad.UploadGrant, RegisterTranscriptSegmentInput{
		ArtifactLogicalKey: "transcript/multi", Epoch: 0, Sequence: 0, StartOffset: 0, EndOffset: 1, Encoding: "gzip",
	}); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("multi-member gzip err=%v", err)
	}

	emptyInput := input
	emptyInput.JobID, emptyInput.LeaseID, emptyInput.LeaseAttempt = "job-empty", "lease-empty", 11
	empty := reserveHistoryCapture(t, env.service, emptyInput)
	emptySeal := TranscriptSeal{FinalEpoch: -1, SHA256: historyDigest(nil)}
	if err := env.service.SealTranscript(ctx, empty.Capture.ID, empty.UploadGrant, emptySeal); err != nil {
		t.Fatalf("seal canonical empty transcript: %v", err)
	}
}

func TestHistoryCompletionManifestVerdictZeroRootsAndActiveIntent(t *testing.T) {
	env := newHistoryCaptureTestEnv(t)
	ctx := context.Background()
	input := baseHistoryReservation()
	input.ExpectedTranscript = false
	reserved := reserveHistoryCapture(t, env.service, input)
	capture := transitionHistory(t, env.service, reserved.Capture, HistoryCaptureRunning)
	capture, err := env.service.RecordExecutionVerdict(ctx, capture.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionFailed, ErrorCode: "startup", ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	capture = transitionHistory(t, env.service, capture, HistoryCaptureQuiescing)
	capture, err = env.service.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts:             []FinalArtifactExpectation{{LogicalKey: "manifest/final", Kind: HistoryArtifactManifest}},
		ZeroHarnessRootReason: "Harness failed before creating its session root", ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatalf("declare audited zero-root startup failure: %v", err)
	}
	workerTemporary := uploadHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, []byte("{}"))
	if _, err := env.service.PublishArtifact(ctx, capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: HistoryArtifactManifest, Phase: HistoryArtifactFinal,
		MediaType: "application/json", LogicalSize: 2,
	}, workerTemporary); !errors.Is(err, ErrHistoryUnauthorized) {
		t.Fatalf("worker manifest publication err=%v", err)
	}
	if err := env.service.AbandonUpload(ctx, capture.ID, reserved.UploadGrant, workerTemporary.ID); err != nil {
		t.Fatalf("abandon rejected worker manifest temporary: %v", err)
	}
	publishCanonicalManifestBytes(t, env.service, capture.ID, PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: HistoryArtifactManifest, Phase: HistoryArtifactFinal,
		MediaType: "application/json", LogicalSize: 2,
	}, []byte("{}"))
	capture = transitionHistory(t, env.service, capture, HistoryCaptureSealed)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureUploading)
	active := uploadHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, []byte("outbox"))
	if _, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker"); !errors.Is(err, ErrHistoryPublicationPending) {
		t.Fatalf("complete with active intent err=%v", err)
	}
	if err := env.service.AbandonUpload(ctx, capture.ID, reserved.UploadGrant, active.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil || completed.State != HistoryCaptureComplete || completed.ZeroHarnessRootReason == "" {
		t.Fatalf("zero-root completion=%+v err=%v", completed, err)
	}

	pendingInput := input
	pendingInput.ExpectedHarness = false
	pendingInput.JobID, pendingInput.LeaseID, pendingInput.LeaseAttempt = "job-pending", "lease-pending", 12
	pending := reserveHistoryCapture(t, env.service, pendingInput)
	pendingCapture := transitionHistory(t, env.service, pending.Capture, HistoryCaptureRunning)
	pendingCapture = transitionHistory(t, env.service, pendingCapture, HistoryCaptureQuiescing)
	pendingCapture, err = env.service.DeclareExpectedSet(ctx, pendingCapture.ID, pending.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts:       []FinalArtifactExpectation{{LogicalKey: "manifest/final", Kind: HistoryArtifactManifest}},
		ExpectedVersion: pendingCapture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	publishCanonicalManifestBytes(t, env.service, pendingCapture.ID, PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: HistoryArtifactManifest, Phase: HistoryArtifactFinal,
		MediaType: "application/json", LogicalSize: 2,
	}, []byte("{}"))
	pendingCapture = transitionHistory(t, env.service, pendingCapture, HistoryCaptureSealed)
	pendingCapture = transitionHistory(t, env.service, pendingCapture, HistoryCaptureUploading)
	if _, err := env.service.Complete(ctx, pendingCapture.ID, pending.UploadGrant, pendingCapture.Version, "worker"); !errors.Is(err, ErrHistoryIncomplete) {
		t.Fatalf("pending-verdict completion err=%v", err)
	}
}

// Compile-time guard that the test response-loss wrapper still uses the complete
// production Store contract rather than a fake subset.
var _ blob.Store = (*publishResponseLossStore)(nil)
var _ io.Writer = (blob.Upload)(nil)
