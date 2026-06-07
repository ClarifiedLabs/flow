package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

type recordedHookRequest struct {
	Method string
	Path   string
	Body   string
}

type hookTestServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedHookRequest
}

func (h *hookTestServer) recorded() []recordedHookRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedHookRequest, len(h.requests))
	copy(out, h.requests)
	return out
}

func (h *hookTestServer) sawPath(method, suffix string) bool {
	for _, req := range h.recorded() {
		if req.Method == method && strings.HasSuffix(req.Path, suffix) {
			return true
		}
	}
	return false
}

func newHookTestServer(t *testing.T, threads []coordinator.ReviewThread) *hookTestServer {
	t.Helper()
	hs := &hookTestServer{}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hs.mu.Lock()
		hs.requests = append(hs.requests, recordedHookRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		hs.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/threads"):
			_ = json.NewEncoder(w).Encode(map[string]any{"threads": threads})
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(hs.Close)
	return hs
}

func claimedThread(id string, state coordinator.ReviewThreadState, claimCommit string) coordinator.ReviewThread {
	thread := coordinator.ReviewThread{ID: id, ChangeID: "ch-1", State: state}
	if strings.TrimSpace(claimCommit) != "" {
		sha := claimCommit
		thread.ClaimCommitSHA = &sha
	}
	return thread
}

func chdirTempGitRepo(t *testing.T, subject string) {
	t.Helper()
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q")
	runTestGit(t, dir, "config", "user.email", "flow@example.com")
	runTestGit(t, dir, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runTestGit(t, dir, "add", ".")
	runTestGit(t, dir, "commit", "-q", "-m", subject)

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// --- pre-push -------------------------------------------------------------

func TestHookPrepushExitsZeroWithoutSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	withStdin(t, "", func() {
		if code := run([]string{"hook", "claude", "prepush"}, &stdout, &stderr); code != 0 {
			t.Fatalf("prepush exit = %d, want 0; stderr=%q", code, stderr.String())
		}
	})
}

func TestHookPrepushAlwaysExitsZeroWhenCoordinatorUnreachable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "tok")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	// A port that refuses connections fast so the hook never blocks the push.
	t.Setenv("FLOW_COORDINATOR_URL", "http://127.0.0.1:1")

	var stdout, stderr bytes.Buffer
	withStdin(t, "", func() {
		if code := run([]string{"hook", "claude", "prepush"}, &stdout, &stderr); code != 0 {
			t.Fatalf("prepush exit = %d, want 0 even when coordinator is unreachable; stderr=%q", code, stderr.String())
		}
	})
}

func TestHookPrepushCapturesAndSteers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	chdirTempGitRepo(t, "Fix the parser")
	server := newHookTestServer(t, []coordinator.ReviewThread{
		claimedThread("t-open", coordinator.ThreadOpen, ""),
		claimedThread("t-reopened", coordinator.ThreadReopened, ""),
		claimedThread("t-claimed", coordinator.ThreadClaimed, ""),
		claimedThread("t-certified", coordinator.ThreadCertified, ""),
	})
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "tok")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_LEASE_ID", "l-1")
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)

	var stdout, stderr bytes.Buffer
	withStdin(t, "refs/heads/x abc refs/heads/x def\n", func() {
		if code := run([]string{"hook", "claude", "prepush"}, &stdout, &stderr); code != 0 {
			t.Fatalf("prepush exit = %d, want 0; stderr=%q", code, stderr.String())
		}
	})

	if !server.sawPath(http.MethodPost, "/v1/sessions/s-1/signal") {
		t.Fatalf("expected a capture POST to the session signal endpoint; got %+v", server.recorded())
	}
	if !server.sawPath(http.MethodGet, "/v1/changes/ch-1/threads") {
		t.Fatalf("expected a threads fetch; got %+v", server.recorded())
	}
	// Only open + reopened threads are unresolved (claimed/certified are not).
	if !strings.Contains(stderr.String(), "2 unresolved review thread") {
		t.Fatalf("steering message missing unresolved count:\n%s", stderr.String())
	}

	// The capture payload should carry the commit subject the server-side
	// post-receive can't see.
	var captured string
	for _, req := range server.recorded() {
		if strings.HasSuffix(req.Path, "/signal") {
			captured = req.Body
		}
	}
	if !strings.Contains(captured, "Fix the parser") {
		t.Fatalf("capture payload missing commit subject: %q", captured)
	}
}

func TestHookPrepushSilentWhenNoUnresolvedThreads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	chdirTempGitRepo(t, "Tidy up")
	server := newHookTestServer(t, []coordinator.ReviewThread{
		claimedThread("t-claimed", coordinator.ThreadClaimed, ""),
	})
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "tok")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)

	var stdout, stderr bytes.Buffer
	withStdin(t, "", func() {
		if code := run([]string{"hook", "claude", "prepush"}, &stdout, &stderr); code != 0 {
			t.Fatalf("prepush exit = %d, want 0; stderr=%q", code, stderr.String())
		}
	})
	if strings.Contains(stderr.String(), "unresolved review thread") {
		t.Fatalf("did not expect a steering message with no unresolved threads:\n%s", stderr.String())
	}
}

// --- commit-msg -----------------------------------------------------------

func writeCommitMsg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write commit message: %v", err)
	}
	return path
}

func TestHookCommitMsgInjectsResolvesForClaimedThreads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := newHookTestServer(t, []coordinator.ReviewThread{
		claimedThread("t-open", coordinator.ThreadOpen, ""),
		claimedThread("t-claim-nocommit", coordinator.ThreadClaimed, ""),
		claimedThread("t-claim-withcommit", coordinator.ThreadClaimed, "deadbeef"),
	})
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "tok")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)

	msgPath := writeCommitMsg(t, "Fix the bug\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hook", "claude", "commit-msg", msgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("commit-msg exit = %d, want 0; stderr=%q", code, stderr.String())
	}

	updated := readTestFile(t, msgPath)
	if !strings.Contains(updated, "Resolves: t-claim-nocommit") {
		t.Fatalf("expected injected trailer for claimed thread:\n%s", updated)
	}
	if strings.Contains(updated, "t-open") {
		t.Fatalf("open (unclaimed) thread should not be injected:\n%s", updated)
	}
	if strings.Contains(updated, "t-claim-withcommit") {
		t.Fatalf("already-committed claim should not be re-injected:\n%s", updated)
	}
}

func TestHookCommitMsgDoesNotDuplicateExistingTrailer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := newHookTestServer(t, []coordinator.ReviewThread{
		claimedThread("t-claim", coordinator.ThreadClaimed, ""),
	})
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "tok")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_COORDINATOR_URL", server.URL)

	msgPath := writeCommitMsg(t, "Fix it\n\nResolves: t-claim\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hook", "claude", "commit-msg", msgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("commit-msg exit = %d, want 0; stderr=%q", code, stderr.String())
	}

	updated := readTestFile(t, msgPath)
	if got := strings.Count(updated, "t-claim"); got != 1 {
		t.Fatalf("expected exactly one trailer reference, got %d:\n%s", got, updated)
	}
}

func TestHookCommitMsgLeavesNonFlowCommitUntouched(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// No FLOW_SESSION_ID / token / change: a normal git op outside a flow session.
	msgPath := writeCommitMsg(t, "Local commit\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hook", "claude", "commit-msg", msgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("commit-msg exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got := readTestFile(t, msgPath); got != "Local commit\n" {
		t.Fatalf("non-flow commit message was modified: %q", got)
	}
}

func TestHookCommitMsgNeverFailsWhenCoordinatorUnreachable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FLOW_SESSION_ID", "s-1")
	t.Setenv("FLOW_SESSION_TOKEN", "tok")
	t.Setenv("FLOW_CHANGE_ID", "ch-1")
	t.Setenv("FLOW_COORDINATOR_URL", "http://127.0.0.1:1")

	msgPath := writeCommitMsg(t, "Fix under outage\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hook", "claude", "commit-msg", msgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("commit-msg exit = %d, want 0 even when coordinator is unreachable; stderr=%q", code, stderr.String())
	}
	if got := readTestFile(t, msgPath); got != "Fix under outage\n" {
		t.Fatalf("commit message changed despite outage: %q", got)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
