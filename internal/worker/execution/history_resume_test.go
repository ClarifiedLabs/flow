package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
)

func TestValidateHistoryResumePayloadRejectsUnsafeOrIncompleteCoordinates(t *testing.T) {
	valid := HistoryResumePayload{
		ID: "hr-0123456789abcdef0123456789abcdef", SourceCaptureID: "hc-source",
		NativeSessionID: "native-child", SessionRelativeDir: "children/native-child",
		HarnessArtifactID: "ha-harness", HarnessSHA256: strings.Repeat("a", 64),
		WorkspaceArtifactID: "ha-workspace", WorkspaceSHA256: strings.Repeat("b", 64),
		RequiredHeadCommit: strings.Repeat("c", 40), SourceHarnessBuild: "0.4.5",
		RequiredHarnessSchemaVersion: 5,
	}
	if err := validateHistoryResumePayload(valid); err != nil {
		t.Fatalf("valid resume payload: %v", err)
	}
	for name, mutate := range map[string]func(*HistoryResumePayload){
		"traversal": func(value *HistoryResumePayload) { value.SessionRelativeDir = "../outside" },
		"absolute":  func(value *HistoryResumePayload) { value.SessionRelativeDir = "/outside" },
		"backslash": func(value *HistoryResumePayload) { value.SessionRelativeDir = `children\\outside` },
		"digest":    func(value *HistoryResumePayload) { value.HarnessSHA256 = strings.Repeat("A", 64) },
		"schema":    func(value *HistoryResumePayload) { value.RequiredHarnessSchemaVersion = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if err := validateHistoryResumePayload(value); err == nil {
				t.Fatal("unsafe resume payload was accepted")
			}
		})
	}
}

func TestHistoryResumePreflightAndRestoreSelectedNativeSession(t *testing.T) {
	ctx := context.Background()
	nativeRoot := t.TempDir()
	state := func(id, agent string) []byte {
		return []byte(fmt.Sprintf(`{"version":5,"id":%q,"agent":%q,"build":{"version":"0.4.5"}}`, id, agent))
	}
	if err := os.WriteFile(filepath.Join(nativeRoot, "state.json"), state("native-root", "author"), 0o600); err != nil {
		t.Fatal(err)
	}
	childDir := filepath.Join(nativeRoot, "children", "native-child")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "state.json"), state("native-child", "explore"), 0o600); err != nil {
		t.Fatal(err)
	}
	var harnessBytes bytes.Buffer
	harnessArtifact, _, err := historyarchive.WriteHarness(ctx, &harnessBytes, nativeRoot, historyarchive.HarnessOptions{
		HarnessBuild: "0.4.5", RootSessionID: "native-root",
	})
	if err != nil {
		t.Fatal(err)
	}

	sourceRepo, remote := createSeedGitRemote(t)
	if err := os.WriteFile(filepath.Join(sourceRepo, "README.md"), []byte("resumed edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRepo, "untracked.txt"), []byte("resume me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var workspaceBytes bytes.Buffer
	workspaceArtifact, workspaceManifest, err := historyarchive.WriteWorkspace(ctx, &workspaceBytes, sourceRepo, historyarchive.WorkspaceOptions{BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}

	var sawLeaseProof bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Flow-History-Resume-Job") != "j-resume" || r.Header.Get("Flow-History-Resume-Lease") != "l-resume" {
			http.Error(w, "missing lease proof", http.StatusForbidden)
			return
		}
		sawLeaseProof = true
		switch {
		case strings.Contains(r.URL.Path, "/ha-harness/"):
			_, _ = w.Write(harnessBytes.Bytes())
		case strings.Contains(r.URL.Path, "/ha-workspace/"):
			_, _ = w.Write(workspaceBytes.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := workerConfig(t.TempDir(), "file:///tmp/exchange.git")
	cfg.CoordinatorURL = server.URL
	cfg.Token = "worker-secret"
	input := RunInput{Config: cfg, Job: Job{ID: "j-resume"}, Lease: Lease{ID: "l-resume", WorkerID: "w-local"}}
	resume := HistoryResumePayload{
		ID: "hr-resume", SourceCaptureID: "hc-source", NativeSessionID: "native-child",
		SessionRelativeDir: "children/native-child", HarnessArtifactID: "ha-harness", HarnessSHA256: harnessArtifact.SHA256,
		WorkspaceArtifactID: "ha-workspace", WorkspaceSHA256: workspaceArtifact.SHA256,
		RequiredHeadCommit: workspaceManifest.HeadCommit, SourceHarnessBuild: "0.4.5",
		RequiredHarnessSchemaVersion: historyarchive.SupportedHarnessNativeSchema,
	}
	archives, err := prepareHistoryResumeArchives(ctx, input, resume, historyAttemptDir(cfg.WorkDir, input.Job.ID, input.Lease.ID))
	if err == nil {
		// The production path creates the attempt directory before preflight.
		archives.cleanup()
		t.Fatal("preflight unexpectedly accepted a missing attempt directory")
	}
	attemptDir := historyAttemptDir(cfg.WorkDir, input.Job.ID, input.Lease.ID)
	if err := os.MkdirAll(attemptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archives, err = prepareHistoryResumeArchives(ctx, input, resume, attemptDir)
	if err != nil {
		t.Fatalf("preflight resume archives: %v", err)
	}
	defer archives.cleanup()
	if !sawLeaseProof {
		t.Fatal("resume artifact requests did not prove the active lease")
	}

	restoredRepo := filepath.Join(t.TempDir(), "restored")
	gitRun(t, filepath.Dir(restoredRepo), "clone", "-b", "main", remote, restoredRepo)
	restoredNative := filepath.Join(attemptDir, "harness-session")
	if err := restoreHistoryResume(ctx, resume, archives, restoredRepo, restoredNative); err != nil {
		t.Fatalf("restore history resume: %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(restoredRepo, "README.md"):                                "resumed edit\n",
		filepath.Join(restoredRepo, "untracked.txt"):                            "resume me\n",
		filepath.Join(restoredNative, "children", "native-child", "state.json"): string(state("native-child", "explore")),
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("restored %s = %q, err=%v, want %q", path, got, err, want)
		}
	}

	failedRepo := filepath.Join(t.TempDir(), "failed")
	gitRun(t, filepath.Dir(failedRepo), "clone", "-b", "main", remote, failedRepo)
	if err := os.WriteFile(filepath.Join(failedRepo, "collision.txt"), []byte("preserve me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failedNative := filepath.Join(attemptDir, "failed-harness-session")
	if err := restoreHistoryResume(ctx, resume, archives, failedRepo, failedNative); err == nil {
		t.Fatal("restore unexpectedly accepted a dirty destination")
	}
	if _, err := os.Lstat(failedNative); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed workspace restore left native session state: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(failedRepo, "collision.txt")); err != nil || string(got) != "preserve me\n" {
		t.Fatalf("failed restore changed destination: %q, %v", got, err)
	}
}

func TestWorkerEnvTargetsSelectedRestoredNativeSession(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "worker")
	input := tmuxInput{
		Config: workerConfig(workDir, "file:///tmp/exchange.git"),
		Job:    Job{ID: "j-resume", Role: RoleAuthor},
		Lease:  Lease{ID: "l-resume", WorkerID: "w-local"},
		Payload: JobPayload{
			AgentHarness:  flowharness.Harness,
			HistoryResume: &HistoryResumePayload{SessionRelativeDir: "children/native-child"},
		},
		Entrypoint: Entrypoint{
			Argv:  []string{`prompt=$(cat); exec harness --session "$FLOW_HARNESS_SESSION" "$prompt"`},
			Shell: true, Harness: flowharness.Harness,
		},
	}
	env := workerEnv(input)
	want := filepath.Join(historyAttemptDir(workDir, input.Job.ID, input.Lease.ID), "harness-session", "children", "native-child")
	if env["FLOW_HARNESS_SESSION"] != want {
		t.Fatalf("FLOW_HARNESS_SESSION = %q, want %q", env["FLOW_HARNESS_SESSION"], want)
	}
}
