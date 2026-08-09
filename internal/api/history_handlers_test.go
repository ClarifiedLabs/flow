package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestHistoryOwnerAPIListsPaginatesFiltersAndDoesNotLeakInternals(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	for _, jobID := range []string{"history-job-a", "history-job-b", "history-job-c"} {
		reserveAPIHistoryCapture(t, fixture, jobID)
	}
	basePath := "/v2/projects/" + fixture.Project.ID + "/history/captures"

	var first contract.HistoryCapturesResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, basePath+"?limit=2", nil, http.StatusOK, &first)
	if len(first.Captures) != 2 || first.NextCursor == "" || first.SnapshotUntil == "" || first.Availability.Total != 3 {
		t.Fatalf("first history page = %+v, want two captures, a cursor, and aggregate availability", first)
	}
	var second contract.HistoryCapturesResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, basePath+"?limit=2&cursor="+url.QueryEscape(first.NextCursor), nil, http.StatusOK, &second)
	if len(second.Captures) != 1 || second.NextCursor != "" || second.Availability.Total != 3 {
		t.Fatalf("second history page = %+v, want one terminal page with stable aggregate availability", second)
	}
	seen := map[string]bool{}
	for _, capture := range append(first.Captures, second.Captures...) {
		if seen[capture.ID] {
			t.Fatalf("capture %s appeared on both keyset pages", capture.ID)
		}
		seen[capture.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("history page union = %d captures, want 3", len(seen))
	}

	var filtered contract.HistoryCapturesResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, basePath+"?job_id=history-job-b", nil, http.StatusOK, &filtered)
	if len(filtered.Captures) != 1 || filtered.Captures[0].JobID != "history-job-b" {
		t.Fatalf("filtered captures = %+v", filtered.Captures)
	}
	body, err := json.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"upload_grant", "blob_key", "temporary_upload_id", fixture.DataDir} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("history response leaks %q: %s", forbidden, body)
		}
	}

	doJSONRequest(t, fixture.Server, http.MethodGet, basePath+"?unknown=true", nil, http.StatusBadRequest, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, basePath, nil, http.StatusForbidden, nil)
}

func TestHistoryOwnerAPIEventsWaiveAndRevoke(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	reserved := reserveAPIHistoryCapture(t, fixture, "history-owner-actions")
	basePath := "/v2/projects/" + fixture.Project.ID + "/history/captures/" + reserved.Capture.ID

	var events contract.HistoryCaptureEventsResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, basePath+"/events", nil, http.StatusOK, &events)
	if len(events.Events) != 1 || events.Events[0].EventKind != "reserved" || !json.Valid(events.Events[0].Details) {
		t.Fatalf("initial events = %+v", events.Events)
	}
	var revoked contract.HistoryCapture
	doJSONRequest(t, fixture.Server, http.MethodPost, basePath+"/upload-grant/revoke", contract.RevokeHistoryUploadGrantRequest{
		ExpectedVersion: reserved.Capture.Version, Reason: "operator revoked worker publication",
	}, http.StatusOK, &revoked)
	if revoked.ID != reserved.Capture.ID || revoked.State != string(reserved.Capture.State) || revoked.Version != reserved.Capture.Version+1 {
		t.Fatalf("revoked capture = %+v", revoked)
	}
	if err := fixture.Bundle.HistoryCaptures.AuthenticateUploadGrant(context.Background(), reserved.Capture.ID, reserved.UploadGrant); !errors.Is(err, coordinator.ErrHistoryGrantNoLongerUsable) {
		t.Fatalf("revoked upload grant err = %v", err)
	}
	doJSONRequest(t, fixture.Server, http.MethodGet, basePath+"/events", nil, http.StatusOK, &events)
	if len(events.Events) != 2 || events.Events[1].EventKind != "upload_grant_revoked" || !strings.Contains(string(events.Events[1].Details), "operator revoked") {
		t.Fatalf("revocation events = %+v", events.Events)
	}

	waiveReserved := reserveAPIHistoryCapture(t, fixture, "history-owner-waive")
	waivePath := "/v2/projects/" + fixture.Project.ID + "/history/captures/" + waiveReserved.Capture.ID
	var waived contract.HistoryCapture
	doJSONRequest(t, fixture.Server, http.MethodPost, waivePath+"/waive", contract.WaiveHistoryCaptureRequest{
		ExpectedVersion: waiveReserved.Capture.Version, Reason: "source workspace irrecoverable",
	}, http.StatusOK, &waived)
	if waived.State != string(coordinator.HistoryCaptureWaived) || waived.WaiverReason != "source workspace irrecoverable" {
		t.Fatalf("waived capture = %+v", waived)
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, basePath+"/events", nil, http.StatusForbidden, nil)
}

func TestHistoryOwnerAPIDetailAndArtifactRangeStreaming(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	reserved := reserveAPIHistoryCapture(t, fixture, "history-content")
	content := []byte("0123456789")
	artifact := publishAPIHistoryArtifact(t, fixture, reserved, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "transcript/000001", Kind: coordinator.HistoryArtifactTranscriptSegment,
		Phase: coordinator.HistoryArtifactFinal, MediaType: "text/plain; charset=utf-8",
		LogicalSize: int64(len(content)), EntryCount: 1,
	}, content, false)
	basePath := "/v2/projects/" + fixture.Project.ID + "/history/captures/" + reserved.Capture.ID

	var detail contract.HistoryCaptureResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, basePath, nil, http.StatusOK, &detail)
	if detail.Capture.ID != reserved.Capture.ID || len(detail.Artifacts) != 1 || detail.Artifacts[0].ID != artifact.ID {
		t.Fatalf("history detail = %+v", detail)
	}
	if !strings.HasPrefix(detail.Artifacts[0].ContentPath, "/v2/projects/"+fixture.Project.ID+"/") {
		t.Fatalf("content path = %q, want project-scoped path", detail.Artifacts[0].ContentPath)
	}

	contentPath := basePath + "/artifacts/" + artifact.ID + "/content"
	recorder := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, contentPath, nil)
	request.Header.Set("Range", "bytes=2-5")
	fixture.Server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "2345" {
		t.Fatalf("range response = %d %q, want 206 %q", recorder.Code, recorder.Body.String(), "2345")
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := recorder.Header().Get("ETag"); got != `"`+artifact.SHA256+`"` {
		t.Fatalf("ETag = %q", got)
	}

	recorder = httptest.NewRecorder()
	request = authorizedRequest(http.MethodHead, contentPath, nil)
	fixture.Server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD response = %d len=%d headers=%v", recorder.Code, recorder.Body.Len(), recorder.Header())
	}

	recorder = httptest.NewRecorder()
	request = authorizedRequest(http.MethodGet, contentPath, nil)
	request.Header.Set("Range", "bytes=99-100")
	fixture.Server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestedRangeNotSatisfiable || recorder.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("unsatisfiable range = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestHistoryManifestEndpointStreamsCoordinatorPublishedBytes(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	reserved := reserveAPIHistoryCapture(t, fixture, "history-manifest")
	manifest := []byte(`{"format":"flow-history-manifest","schema_version":1}`)
	publishAPIHistoryArtifact(t, fixture, reserved, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: coordinator.HistoryArtifactManifest,
		Phase: coordinator.HistoryArtifactFinal, MediaType: "application/json",
		LogicalSize: int64(len(manifest)), EntryCount: 1,
	}, manifest, true)

	path := "/v2/projects/" + fixture.Project.ID + "/history/captures/" + reserved.Capture.ID + "/manifest"
	recorder := httptest.NewRecorder()
	fixture.Server.ServeHTTP(recorder, authorizedRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != string(manifest) {
		t.Fatalf("manifest response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestHistoryResumeAPIIsIdempotentAndArtifactsRequireSelectedActiveLease(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()
	source, harnessArtifact, unselectedArtifact := seedAPIHistoryResumeSource(t, fixture)
	resumePath := "/v2/projects/" + fixture.Project.ID + "/history/captures/" + source.ID + "/resume"

	var created contract.ResumeHistoryCaptureResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, resumePath, contract.ResumeHistoryCaptureRequest{
		NativeSessionID: "native-child", IdempotencyKey: "resume-api-once",
	}, http.StatusCreated, &created)
	if !created.Created || created.SourceCaptureID != source.ID || created.SourceNativeSessionID != "native-child" ||
		created.SourceHarnessBuild != "0.4.5" || created.RequiredHarnessSchema != 5 || created.JobID == "" {
		t.Fatalf("created resume = %+v", created)
	}
	var replayed contract.ResumeHistoryCaptureResponse
	doJSONRequest(t, fixture.Server, http.MethodPost, resumePath, contract.ResumeHistoryCaptureRequest{
		NativeSessionID: "native-child", IdempotencyKey: "resume-api-once",
	}, http.StatusOK, &replayed)
	if replayed.Created || replayed.ID != created.ID || replayed.JobID != created.JobID {
		t.Fatalf("idempotent resume = %+v, want IDs from %+v", replayed, created)
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, resumePath,
		contract.ResumeHistoryCaptureRequest{IdempotencyKey: "worker-denied"}, http.StatusForbidden, nil)

	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID: "w-local", Labels: map[string]string{"agent.harness.harness": "true"},
	}); err != nil {
		t.Fatalf("register resume worker: %v", err)
	}
	claim, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID: "w-local", LeaseDuration: time.Minute,
	})
	if err != nil || !ok || claim.Job.ID != created.JobID {
		t.Fatalf("claim resume job = %+v ok=%t err=%v", claim, ok, err)
	}
	artifactPath := "/v2/history/captures/" + source.ID + "/artifacts/" + harnessArtifact.ID + "/resume-content"
	requestArtifact := func(path, jobID, leaseID string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer worker-token")
		request.Header.Set(protocolHeader, contract.ProtocolVersion)
		request.Header.Set(historyResumeJobHeader, jobID)
		request.Header.Set(historyResumeLeaseHeader, leaseID)
		recorder := httptest.NewRecorder()
		fixture.Server.ServeHTTP(recorder, request)
		return recorder
	}
	allowed := requestArtifact(artifactPath, claim.Job.ID, claim.Lease.ID)
	if allowed.Code != http.StatusOK || allowed.Body.String() != "harness" {
		t.Fatalf("selected resume artifact = %d %q", allowed.Code, allowed.Body.String())
	}
	unselectedPath := "/v2/history/captures/" + source.ID + "/artifacts/" + unselectedArtifact.ID + "/resume-content"
	if denied := requestArtifact(unselectedPath, claim.Job.ID, claim.Lease.ID); denied.Code != http.StatusForbidden {
		t.Fatalf("unselected artifact status = %d body=%s", denied.Code, denied.Body.String())
	}
	if denied := requestArtifact(artifactPath, claim.Job.ID, "wrong-lease"); denied.Code != http.StatusForbidden {
		t.Fatalf("wrong lease status = %d body=%s", denied.Code, denied.Body.String())
	}
}

func TestWorkerHistoryAPIReservesFromLeaseAndPublishesWithCapability(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID: "w-local"}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role: flowworker.RoleCI, CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{"stage": "verify", "blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claim, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID: "w-local", LeaseDuration: time.Minute,
	})
	if err != nil || !ok {
		t.Fatalf("claim job: ok=%v err=%v", ok, err)
	}
	if claim.Job.ID != job.ID {
		t.Fatalf("claimed job = %s, want %s", claim.Job.ID, job.ID)
	}

	var reserved contract.ReserveHistoryCaptureResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/history/captures", contract.ReserveHistoryCaptureRequest{
		JobID: job.ID, LeaseID: claim.Lease.ID, ExpectedTranscript: true,
	}, http.StatusCreated, &reserved)
	if reserved.UploadGrant == "" || reserved.Capture.JobID != job.ID || reserved.Capture.WorkerID != "w-local" || reserved.Capture.Stage != "verify" {
		t.Fatalf("history reservation = %+v", reserved)
	}

	uploadPath := "/v2/history/captures/" + reserved.Capture.ID + "/uploads"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, uploadPath, strings.NewReader("worker-history"))
	request.Header.Set("Authorization", "Bearer worker-token")
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set(historyUploadGrantHeader, reserved.UploadGrant)
	fixture.Server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("history upload status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var upload contract.HistoryUploadResponse
	if err := json.NewDecoder(recorder.Body).Decode(&upload); err != nil {
		t.Fatalf("decode upload: %v", err)
	}

	abandonedUpload := uploadAPIHistoryBytes(t, fixture, coordinator.ReserveHistoryCaptureResult{
		Capture: coordinator.HistoryCapture{ID: reserved.Capture.ID}, UploadGrant: reserved.UploadGrant,
	}, "abandon-me")
	abandonPath := uploadPath + "/" + abandonedUpload.TemporaryUploadID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodDelete, abandonPath, nil,
		http.StatusNoContent, nil, historyUploadGrantHeader, reserved.UploadGrant)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodDelete, abandonPath, nil,
		http.StatusNoContent, nil, historyUploadGrantHeader, reserved.UploadGrant)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodDelete, abandonPath, nil,
		http.StatusNotFound, nil, historyUploadGrantHeader, reserved.UploadGrant)

	var artifact contract.HistoryArtifact
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost,
		"/v2/history/captures/"+reserved.Capture.ID+"/artifacts",
		contract.PublishHistoryArtifactRequest{
			TemporaryUploadID: upload.TemporaryUploadID, LogicalKey: "transcript/000001",
			Kind: string(coordinator.HistoryArtifactTranscriptSegment), Phase: string(coordinator.HistoryArtifactFinal),
			MediaType: "text/plain", FormatVersion: 1, SchemaVersion: 1,
			LogicalSize: int64(len("worker-history")), EntryCount: 1,
		}, http.StatusCreated, &artifact, historyUploadGrantHeader, reserved.UploadGrant)
	if artifact.SHA256 != upload.SHA256 || artifact.PublicationState != string(coordinator.HistoryPublicationCommitted) {
		t.Fatalf("published history artifact = %+v", artifact)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, uploadPath, strings.NewReader("denied"))
	request.Header.Set("Authorization", "Bearer worker-token")
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set(historyUploadGrantHeader, "wrong")
	fixture.Server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong history grant status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost,
		"/v2/history/captures/"+reserved.Capture.ID+"/verdict",
		contract.RecordHistoryExecutionVerdictRequest{Verdict: "succeeded", ExpectedVersion: reserved.Capture.Version},
		http.StatusUnauthorized, nil, historyUploadGrantHeader, "wrong")

	ownerPath := "/v2/projects/" + fixture.Project.ID + "/history/captures/" + reserved.Capture.ID + "/artifacts/" + artifact.ID + "/content"
	recorder = httptest.NewRecorder()
	fixture.Server.ServeHTTP(recorder, authorizedRequest(http.MethodGet, ownerPath, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "worker-history" {
		t.Fatalf("owner history read = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestWorkerHistoryAPIPublishesCompleteNonHarnessCaptureIdempotently(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	reserved := reserveAPIHistoryCapture(t, fixture, "history-complete-api")
	basePath := "/v2/history/captures/" + reserved.Capture.ID
	uploading := reserved.Capture
	for _, state := range []string{"running", "quiescing", "sealed", "uploading"} {
		doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/transition",
			contract.TransitionHistoryCaptureRequest{To: state, ExpectedVersion: uploading.Version},
			http.StatusOK, &uploading, historyUploadGrantHeader, reserved.UploadGrant)
	}
	emptySHA256 := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	seal := contract.HistoryTranscriptSeal{FinalEpoch: -1, SegmentCount: 0, LogicalLength: 0, SHA256: emptySHA256}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/transcript-seal", seal,
		http.StatusCreated, nil, historyUploadGrantHeader, reserved.UploadGrant)

	workspace := "deterministic workspace archive"
	upload := uploadAPIHistoryBytes(t, fixture, reserved, workspace)
	var workspaceArtifact contract.HistoryArtifact
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/artifacts",
		contract.PublishHistoryArtifactRequest{
			TemporaryUploadID: upload.TemporaryUploadID, LogicalKey: "workspace/final",
			Kind: "workspace_snapshot", Phase: "final", MediaType: "application/x-tar",
			FormatVersion: 1, SchemaVersion: 1, LogicalSize: int64(len(workspace)), EntryCount: 1,
		}, http.StatusCreated, &workspaceArtifact, historyUploadGrantHeader, reserved.UploadGrant)
	if workspaceArtifact.ID == "" || workspaceArtifact.SHA256 != upload.SHA256 {
		t.Fatalf("workspace artifact = %+v, upload = %+v", workspaceArtifact, upload)
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/workspace-summary",
		contract.RegisterHistoryWorkspaceSummaryRequest{
			ArtifactLogicalKey: "workspace/final", ArchiveSchemaVersion: 1, Branch: "main",
			HeadCommit: strings.Repeat("a", 40), InventoryDigest: strings.Repeat("b", 64), ValidationStatus: "valid",
		}, http.StatusCreated, nil, historyUploadGrantHeader, reserved.UploadGrant)

	var declared contract.HistoryCapture
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/expected-set",
		contract.DeclareHistoryExpectedSetRequest{
			Artifacts: []contract.HistoryFinalArtifactExpectation{
				{LogicalKey: "workspace/final", Kind: "workspace_snapshot"},
				{LogicalKey: "manifest/final", Kind: "manifest"},
			},
			TranscriptSeal: &seal, ExpectedVersion: uploading.Version,
		}, http.StatusOK, &declared, historyUploadGrantHeader, reserved.UploadGrant)
	var verdict contract.HistoryCapture
	exitCode := 0
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/verdict",
		contract.RecordHistoryExecutionVerdictRequest{Verdict: "succeeded", ExitCode: &exitCode, ExpectedVersion: declared.Version},
		http.StatusOK, &verdict, historyUploadGrantHeader, reserved.UploadGrant)

	var firstManifest, retriedManifest contract.HistoryArtifact
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/manifest", nil,
		http.StatusCreated, &firstManifest, historyUploadGrantHeader, reserved.UploadGrant)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/manifest", nil,
		http.StatusCreated, &retriedManifest, historyUploadGrantHeader, reserved.UploadGrant)
	if firstManifest.ID == "" || retriedManifest.ID != firstManifest.ID || retriedManifest.SHA256 != firstManifest.SHA256 {
		t.Fatalf("manifest retry changed identity: first=%+v retry=%+v", firstManifest, retriedManifest)
	}

	var completed, retried contract.HistoryCapture
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/complete",
		contract.CompleteHistoryCaptureRequest{ExpectedVersion: verdict.Version}, http.StatusOK, &completed,
		historyUploadGrantHeader, reserved.UploadGrant)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, basePath+"/complete",
		contract.CompleteHistoryCaptureRequest{ExpectedVersion: verdict.Version}, http.StatusOK, &retried,
		historyUploadGrantHeader, reserved.UploadGrant)
	if completed.State != "complete" || retried.ID != completed.ID || retried.Version != completed.Version {
		t.Fatalf("completion retry changed result: first=%+v retry=%+v", completed, retried)
	}
}

func seedAPIHistoryResumeSource(t *testing.T, fixture testFixture) (coordinator.HistoryCapture, coordinator.HistoryArtifact, coordinator.HistoryArtifact) {
	t.Helper()
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Resume API source"})
	if err != nil {
		t.Fatal(err)
	}
	const (
		changeID = "ch-history-resume-api"
		head     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, 'task/history-resume-api', 'main', ?, ?, ?)`, changeID, task.ID, head, now, now); err != nil {
		t.Fatalf("insert resume source change: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, ChangeID: historyTestStringPointer(changeID), Role: flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{
			"branch": "task/history-resume-api", "base": "main", "head_sha": head,
			"entrypoint": map[string]any{"argv": []any{"harness"}, "harness": "harness"}, "agent_harness": "harness", "phase_index": 0, "final_phase": true},
	})
	if err != nil {
		t.Fatalf("enqueue resume source job: %v", err)
	}
	reserved, err := fixture.Bundle.HistoryCaptures.Reserve(ctx, coordinator.ReserveHistoryCaptureInput{
		ProjectID: fixture.Project.ID, JobID: job.ID, LeaseID: "lease-history-resume-api", LeaseAttempt: 1,
		WorkerID: "w-local", TaskID: task.ID, ChangeID: changeID, Role: string(flowworker.RoleAuthor),
		HarnessName: "harness", HarnessSchemaVersion: 5,
		ExpectedTranscript: true, ExpectedHarness: true,
	})
	if err != nil {
		t.Fatalf("reserve resume source history: %v", err)
	}
	capture := reserved.Capture
	transition := func(to coordinator.HistoryCaptureState) {
		t.Helper()
		capture, err = fixture.Bundle.HistoryCaptures.Transition(ctx, capture.ID, coordinator.TransitionHistoryCaptureInput{
			To: to, ExpectedVersion: capture.Version, Actor: "worker:w-local",
		})
		if err != nil {
			t.Fatalf("transition resume source to %s: %v", to, err)
		}
	}
	transition(coordinator.HistoryCaptureRunning)
	harnessArtifact := publishAPIHistoryArtifact(t, fixture, reserved, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "harness/final/root", Kind: coordinator.HistoryArtifactHarnessRoot, Phase: coordinator.HistoryArtifactFinal,
		ArchiveID: "native-root", MediaType: "application/x-tar", LogicalSize: 7, EntryCount: 2,
	}, []byte("harness"), false)
	if err := fixture.Bundle.HistoryCaptures.RegisterHarnessArchiveMembers(ctx, capture.ID, reserved.UploadGrant, "harness/final/root", []coordinator.HarnessArchiveMemberInput{
		{NativeSessionID: "native-root", RelativeMemberPath: "state.json", MemberKind: "root", HarnessBuild: "0.4.5", ParseStatus: "parsed"},
		{NativeSessionID: "native-child", NativeParentSessionID: "native-root", RelativeMemberPath: "children/native-child/state.json", MemberKind: "delegated_child", HarnessBuild: "0.4.5", ParseStatus: "parsed"},
	}); err != nil {
		t.Fatalf("register resume Harness members: %v", err)
	}
	publishAPIHistoryArtifact(t, fixture, reserved, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "workspace/final", Kind: coordinator.HistoryArtifactWorkspaceSnapshot, Phase: coordinator.HistoryArtifactFinal,
		ArchiveID: "workspace", MediaType: "application/x-tar", LogicalSize: 9, EntryCount: 1,
	}, []byte("workspace"), false)
	if _, err := fixture.Bundle.HistoryCaptures.RegisterWorkspaceSummary(ctx, capture.ID, reserved.UploadGrant, coordinator.RegisterHistoryWorkspaceSummaryInput{
		ArtifactLogicalKey: "workspace/final", ArchiveSchemaVersion: 1, Branch: "task/history-resume-api", BaseRef: "main",
		BaseCommit: head, HeadCommit: head, InventoryDigest: strings.Repeat("c", 64), ValidationStatus: "valid",
	}); err != nil {
		t.Fatalf("register resume workspace summary: %v", err)
	}
	manifestArtifact := publishAPIHistoryArtifact(t, fixture, reserved, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: coordinator.HistoryArtifactManifest, Phase: coordinator.HistoryArtifactFinal,
		MediaType: "application/json", LogicalSize: 2,
	}, []byte("{}"), true)
	exitCode := 0
	capture, err = fixture.Bundle.HistoryCaptures.RecordExecutionVerdict(ctx, capture.ID, coordinator.RecordHistoryExecutionVerdictInput{
		Verdict: coordinator.HistoryExecutionSucceeded, ExitCode: &exitCode, ExpectedVersion: capture.Version, Actor: "worker:w-local",
	})
	if err != nil {
		t.Fatalf("record resume source verdict: %v", err)
	}
	transition(coordinator.HistoryCaptureQuiescing)
	seal := coordinator.TranscriptSeal{FinalEpoch: -1, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
	if err := fixture.Bundle.HistoryCaptures.SealTranscript(ctx, capture.ID, reserved.UploadGrant, seal); err != nil {
		t.Fatalf("seal resume source transcript: %v", err)
	}
	capture, err = fixture.Bundle.HistoryCaptures.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, coordinator.DeclareHistoryExpectedSetInput{
		Artifacts: []coordinator.FinalArtifactExpectation{
			{LogicalKey: "harness/final/root", Kind: coordinator.HistoryArtifactHarnessRoot},
			{LogicalKey: "workspace/final", Kind: coordinator.HistoryArtifactWorkspaceSnapshot},
			{LogicalKey: "manifest/final", Kind: coordinator.HistoryArtifactManifest},
		}, TranscriptSeal: &seal, ExpectedVersion: capture.Version, Actor: "worker:w-local",
	})
	if err != nil {
		t.Fatalf("declare resume source expected set: %v", err)
	}
	transition(coordinator.HistoryCaptureSealed)
	transition(coordinator.HistoryCaptureUploading)
	capture, err = fixture.Bundle.HistoryCaptures.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker:w-local")
	if err != nil {
		t.Fatalf("complete resume source: %v", err)
	}
	if _, err := fixture.Workers.CancelLiveJobsForTask(ctx, task.ID, flowworker.RoleAuthor); err != nil {
		t.Fatalf("cancel source job: %v", err)
	}
	return capture, harnessArtifact, manifestArtifact
}

func historyTestStringPointer(value string) *string { return &value }

func uploadAPIHistoryBytes(t *testing.T, fixture testFixture, reserved coordinator.ReserveHistoryCaptureResult, content string) contract.HistoryUploadResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v2/history/captures/"+reserved.Capture.ID+"/uploads", strings.NewReader(content))
	request.Header.Set("Authorization", "Bearer worker-token")
	request.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	request.Header.Set(historyUploadGrantHeader, reserved.UploadGrant)
	recorder := httptest.NewRecorder()
	fixture.Server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("history upload status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response contract.HistoryUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode history upload: %v", err)
	}
	return response
}

func reserveAPIHistoryCapture(t *testing.T, fixture testFixture, jobID string) coordinator.ReserveHistoryCaptureResult {
	t.Helper()
	reserved, err := fixture.Bundle.HistoryCaptures.Reserve(context.Background(), coordinator.ReserveHistoryCaptureInput{
		ProjectID: fixture.Project.ID, JobID: jobID, LeaseID: "lease-" + jobID,
		LeaseAttempt: 1, WorkerID: "w-local", Role: "author", ExpectedTranscript: true,
	})
	if err != nil {
		t.Fatalf("reserve history capture: %v", err)
	}
	return reserved
}

func publishAPIHistoryArtifact(t *testing.T, fixture testFixture, reserved coordinator.ReserveHistoryCaptureResult, input coordinator.PublishHistoryArtifactInput, content []byte, canonical bool) coordinator.HistoryArtifact {
	t.Helper()
	ctx := context.Background()
	upload, err := fixture.Bundle.HistoryCaptures.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
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
	var artifact coordinator.HistoryArtifact
	if canonical {
		artifact, err = fixture.Bundle.HistoryCaptures.PublishCanonicalManifest(ctx, reserved.Capture.ID, input, temporary)
	} else {
		artifact, err = fixture.Bundle.HistoryCaptures.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, input, temporary)
	}
	if err != nil {
		t.Fatalf("publish history artifact: %v", err)
	}
	return artifact
}
