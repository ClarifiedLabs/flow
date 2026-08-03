package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

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
