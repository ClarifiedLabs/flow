package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api"
	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/testenv"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	workerexec "github.com/ClarifiedLabs/flow/internal/worker/execution"
)

func TestMain(m *testing.M) {
	cleanup := testenv.Isolate()
	denyDir, err := os.MkdirTemp("", "flow-worker-deny-agents-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create deny agent dir: %v\n", err)
		os.Exit(2)
	}
	writeDenyAgentExecutable(denyDir, "harness", "--check-model-proxy")
	_ = os.Setenv("PATH", denyDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_ = os.Setenv("HARNESS_MODEL_PROXY_URL", "http://127.0.0.1:1")
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func writeDenyAgentExecutable(dir string, name string, checkArgs string) {
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"if [ \"$*\" = " + workerTestShellQuote(checkArgs) + " ]; then\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo \"unexpected test invocation of real agent shim: " + name + " $*\" >&2\n" +
		"exit 127\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "write deny agent %s: %v\n", name, err)
		os.Exit(2)
	}
}

func TestIsLeaseNotRenewableMatchesWrappedRenewFailure(t *testing.T) {
	err := fmt.Errorf("renew lease: %w", &flowclient.HTTPStatusError{
		StatusCode: http.StatusBadRequest,
		Code:       "renew_lease_failed",
		Message:    "lease is not renewable",
	})
	if !isLeaseNotRenewable(err) {
		t.Fatalf("isLeaseNotRenewable(%v) = false, want true", err)
	}
	other := &flowclient.HTTPStatusError{
		StatusCode: http.StatusBadRequest,
		Code:       "renew_lease_failed",
		Message:    "different failure",
	}
	if isLeaseNotRenewable(other) {
		t.Fatalf("isLeaseNotRenewable(%v) = true, want false", other)
	}
}

func TestRetryTransientOperationContextStopsAfterLeaseCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	leaseLost := errors.New("lease lost")
	calls := 0
	started := time.Now()
	err := retryTransientOperationContext(ctx, "test operation", io.Discard, func() error {
		calls++
		cancel(leaseLost)
		return &flowclient.HTTPStatusError{StatusCode: http.StatusServiceUnavailable, Code: "temporarily_unavailable"}
	})
	if !errors.Is(err, leaseLost) || calls != 1 {
		t.Fatalf("retry result err=%v calls=%d, want lease cancellation after one call", err, calls)
	}
	if elapsed := time.Since(started); elapsed >= transientWorkerRetryDelay {
		t.Fatalf("canceled retry waited %s", elapsed)
	}
}

func TestIsStaleSourceJobHeadReportMatchesWrappedForbidden(t *testing.T) {
	err := fmt.Errorf("report check: %w", &flowclient.HTTPStatusError{
		StatusCode: http.StatusForbidden,
		Code:       "forbidden",
		Message:    "source job head does not match current change head",
	})
	if !isStaleSourceJobHeadReport(err) {
		t.Fatalf("isStaleSourceJobHeadReport(%v) = false, want true", err)
	}
	other := &flowclient.HTTPStatusError{
		StatusCode: http.StatusForbidden,
		Code:       "forbidden",
		Message:    "source job does not belong to the reported check",
	}
	if isStaleSourceJobHeadReport(other) {
		t.Fatalf("isStaleSourceJobHeadReport(%v) = true, want false", other)
	}
}

func TestAdvisoryVerdictFindingsStayInCheckDetailsWithoutThreadActions(t *testing.T) {
	job := flowworker.Job{Payload: map[string]any{"blocking": false}}
	if checkJobBlocksApproval(job) {
		t.Fatal("advisory job was treated as blocking")
	}
	if !checkJobBlocksApproval(flowworker.Job{Payload: map[string]any{}}) {
		t.Fatal("legacy job without blocking marker must default to blocking")
	}
	report := workerexec.VerdictReport{
		Verdict: "blocked",
		Reason:  "Potential timing leak.",
		Comments: []workerexec.ReviewCommentReport{{
			SHA: "abc123", File: "auth.go", Line: 42, Body: "Use a constant-time comparison.",
			Severity: "high", IntroducedByChange: boolPtr(false), Requirement: "secret comparisons are constant time",
			FollowUp: "Track as a security-hardening task.",
		}},
		Threads: []workerexec.ThreadDecisionReport{{
			ID: "th-1", Decision: "reopen", Body: "Recheck this path.",
		}},
	}
	details := advisoryVerdictDetails(report.Reason, report, nil)
	for _, want := range []string{"Advisory (non-blocking)", "auth.go:42", "constant-time comparison", "thread th-1", "recommends reopen"} {
		if !strings.Contains(details, want) {
			t.Fatalf("advisory details %q missing %q", details, want)
		}
	}
	var stdout bytes.Buffer
	_, _, _ = applyVerdictActions(context.Background(), nil, coordinator.CheckKindReviewer, false, false, "", flowworker.Lease{}, workerexec.RunResult{}, report, &stdout)
	if !strings.Contains(stdout.String(), "retained 2 advisory finding(s)") || !strings.Contains(stdout.String(), "no review threads changed") {
		t.Fatalf("advisory action output = %q", stdout.String())
	}
}

func TestReviewDiscoveryKeepsBlockingPolicyButSuppressesThreadActions(t *testing.T) {
	job := flowworker.Job{Payload: map[string]any{
		"blocking":         true,
		"review_discovery": true,
	}}
	if !checkJobBlocksApproval(job) {
		t.Fatal("blocking discovery source lost its error policy")
	}
	if !reviewDiscoveryJob(job) {
		t.Fatal("review discovery marker was not recognized")
	}
	report := workerexec.VerdictReport{Comments: []workerexec.ReviewCommentReport{{
		SHA: "abc123", File: "auth.go", Line: 42, Body: "Authorization is bypassed.",
		Severity: "high", IntroducedByChange: boolPtr(true), Requirement: "authorize requests",
	}}}
	if _, _, err := applyVerdictActions(
		context.Background(),
		nil,
		coordinator.CheckKindReviewer,
		checkJobBlocksApproval(job) && !reviewDiscoveryJob(job),
		false,
		"",
		flowworker.Lease{},
		workerexec.RunResult{},
		report,
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("parallel discovery attempted thread actions: %v", err)
	}
}

func TestBlockingReviewerOnlyCountsTaskCausedUniqueHighSeverityFindings(t *testing.T) {
	report := workerexec.VerdictReport{
		Comments: []workerexec.ReviewCommentReport{
			{
				SHA: "a", File: "auth.go", Line: 1, Body: "Authorization is bypassed.",
				Severity: "high", IntroducedByChange: boolPtr(true), Requirement: "authorize requests",
			},
			{
				SHA: "b", File: "legacy.go", Line: 2, Body: "This old path is unsafe.",
				Severity: "critical", IntroducedByChange: boolPtr(false), Requirement: "authorize requests",
				FollowUp: "Create a security hardening task.",
			},
			{
				SHA: "c", File: "style.go", Line: 3, Body: "This could be simpler.",
				Severity: "medium", IntroducedByChange: boolPtr(true), Requirement: "keep code maintainable",
			},
			{
				SHA: "d", File: "auth.go", Line: 4, Body: "Same authorization issue.",
				Severity: "high", IntroducedByChange: boolPtr(true), Requirement: "authorize requests",
				DuplicateOf: "th-1",
			},
		},
	}
	if got := blockingReviewFindings(report); got != 1 {
		t.Fatalf("blockingReviewFindings = %d, want 1", got)
	}
	details := classifiedReviewDetails("review complete", report, nil)
	for _, want := range []string{"pre-existing", "medium", "duplicate of th-1", "security hardening task"} {
		if !strings.Contains(details, want) {
			t.Fatalf("classified details %q missing %q", details, want)
		}
	}
	if strings.Contains(details, "Authorization is bypassed") {
		t.Fatalf("blocking finding leaked into non-blocking follow-ups: %q", details)
	}
	if _, _, err := applyVerdictActions(
		context.Background(),
		nil,
		coordinator.CheckKindReviewer,
		true,
		false,
		"",
		flowworker.Lease{},
		workerexec.RunResult{},
		workerexec.VerdictReport{Comments: report.Comments[1:]},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("non-blocking classified findings attempted thread actions: %v", err)
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func TestReviewerHarnessFailureReportsErroredInsteadOfBlocked(t *testing.T) {
	var reported struct {
		Verdict string `json:"verdict"`
		Details string `json:"details"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/tasks/t-review/checks/security-review" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
			t.Errorf("decode report: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"check":{"task_id":"t-review","name":"security-review","kind":"reviewer","required":true,"verdict":"errored"},"review_state":"in_review"}`)
	}))
	t.Cleanup(server.Close)

	client, err := flowclient.New(config.ClientConfig{ServerURL: server.URL, Token: "worker-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	taskID := "t-review"
	job := flowworker.Job{
		ID:             "j-review",
		TaskID:         &taskID,
		Role:           flowworker.RoleReviewer,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Payload:        map[string]any{"blocking": true},
	}
	result := workerexec.RunResult{
		ExitCode: 1,
		Err:      errors.New("agent transcript invalid after assistant turn"),
		Payload: workerexec.JobPayload{
			CheckName: "security-review",
		},
		VerdictFilePath: filepath.Join(t.TempDir(), workerexec.VerdictFileName),
	}
	var stdout bytes.Buffer
	verdict, err := reportCheckIfNeeded(context.Background(), client, job, flowworker.Lease{ID: "l-review"}, result, &stdout)
	if err == nil {
		t.Fatal("report check error = nil, want worker job failure after errored report")
	}
	if verdict != coordinator.CheckErrored {
		t.Fatalf("reported result = %q, want errored", verdict)
	}
	if reported.Verdict != string(coordinator.CheckErrored) {
		t.Fatalf("reported verdict = %q, want errored", reported.Verdict)
	}
	if !strings.Contains(reported.Details, "agent transcript invalid") {
		t.Fatalf("reported details = %q, want harness failure", reported.Details)
	}
	if strings.Contains(stdout.String(), "falling back to exit code") {
		t.Fatalf("worker output used exit-code fallback: %q", stdout.String())
	}
}

func TestAdvisoryReviewAggregationAppliesTaskActionBeforeReportingVerdict(t *testing.T) {
	var followUpCalls int
	var reported struct {
		Verdict string `json:"verdict"`
		Details string `json:"details"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/tasks/t-review/review-follow-ups":
			followUpCalls++
			var request struct {
				LeaseID string `json:"lease_id"`
				Finding struct {
					File string `json:"file"`
				} `json:"finding"`
				TaskAction struct {
					Action string `json:"action"`
					Title  string `json:"title"`
				} `json:"task_action"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode follow-up: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if request.LeaseID != "l-review" || request.Finding.File != "internal/cache.go" ||
				request.TaskAction.Action != "create_task" || request.TaskAction.Title != "Bound the legacy cache" {
				t.Errorf("review follow-up request = %+v", request)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"task":{"ID":"t-review-0002","Title":"Bound the legacy cache","Body":"Add a bound."},"disposition":"created"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/tasks/t-review/checks/review-aggregation.node.nr-1":
			if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
				t.Errorf("decode report: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"check":{"task_id":"t-review","name":"review-aggregation.node.nr-1","kind":"reviewer","required":false,"verdict":"satisfied"},"review_state":"approved"}`)
		default:
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := flowclient.New(config.ClientConfig{ServerURL: server.URL, Token: "worker-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	verdictPath := filepath.Join(t.TempDir(), workerexec.VerdictFileName)
	if err := os.WriteFile(verdictPath, []byte(`{
		"verdict":"satisfied",
		"reason":"one deferred issue",
		"comments":[{
			"sha":"head-1",
			"file":"internal/cache.go",
			"line":42,
			"body":"The legacy cache has no size bound.",
			"severity":"high",
			"introduced_by_change":false,
			"requirement":"cache memory remains bounded",
			"task_action":{
				"action":"create_task",
				"title":"Bound the legacy cache",
				"body":"Add a configurable cache bound and tests covering eviction."
			}
		}]
	}`), 0o600); err != nil {
		t.Fatalf("write verdict: %v", err)
	}
	sealedReport, ok, err := workerexec.ReadVerdictFile(verdictPath)
	if err != nil || !ok {
		t.Fatalf("read sealed verdict: ok=%v err=%v", ok, err)
	}
	// The live worker captures this report while validating the completion
	// seal. Changing the file afterward must not affect applied actions.
	if err := os.WriteFile(verdictPath, []byte(`{"verdict":"blocked","reason":"mutated after seal"}`), 0o600); err != nil {
		t.Fatalf("mutate verdict after capture: %v", err)
	}
	taskID := "t-review"
	job := flowworker.Job{
		ID: "j-review", TaskID: &taskID, Role: flowworker.RoleReviewer,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{
			"blocking":           false,
			"review_aggregation": true,
		},
	}
	result := workerexec.RunResult{
		ExitCode: 0,
		Payload: workerexec.JobPayload{
			CheckName:          "review-aggregation.node.nr-1",
			ChangeID:           "ch-review",
			CompletionProtocol: checkverdict.CompletionProtocol,
		},
		VerdictFilePath: verdictPath,
		VerdictReport:   &sealedReport,
	}
	var stdout bytes.Buffer
	verdict, err := reportCheckIfNeeded(context.Background(), client, job, flowworker.Lease{ID: "l-review"}, result, &stdout)
	if err != nil || verdict != coordinator.CheckSatisfied {
		t.Fatalf("reportCheckIfNeeded verdict=%s err=%v stdout=%q", verdict, err, stdout.String())
	}
	if followUpCalls != 1 {
		t.Fatalf("follow-up calls = %d, want 1", followUpCalls)
	}
	for _, want := range []string{"Advisory (non-blocking)", "[t-review-0002](/ui/tasks/t-review-0002)", "(created)"} {
		if !strings.Contains(reported.Details, want) {
			t.Fatalf("reported details %q missing %q", reported.Details, want)
		}
	}
}

// Regression: a review aggregation verdict whose follow-up task_action is
// rejected by the coordinator (here: use_existing_task naming a task that is
// already done) must not error the check or block the workflow. The rejected
// action is recorded in the check details while the verdict and the sibling
// follow-ups still apply.
func TestReviewAggregationFollowUpRejectionDegradesToCheckDetails(t *testing.T) {
	var followUpCalls int
	var reported struct {
		Verdict string `json:"verdict"`
		Details string `json:"details"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/tasks/t-review/review-follow-ups":
			followUpCalls++
			var request struct {
				Finding struct {
					File string `json:"file"`
				} `json:"finding"`
				TaskAction struct {
					Action string `json:"action"`
					TaskID string `json:"task_id"`
				} `json:"task_action"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode follow-up: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if request.TaskAction.Action == "use_existing_task" {
				if request.TaskAction.TaskID != "t-review-0001" {
					t.Errorf("use_existing_task target = %q", request.TaskAction.TaskID)
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"review_follow_up_failed","message":"review follow-up task must be open"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"task":{"ID":"t-review-0002","Title":"Bound the legacy cache","Body":"Add a bound."},"disposition":"created"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/tasks/t-review/checks/review-aggregation.node.nr-1":
			if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
				t.Errorf("decode report: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"check":{"task_id":"t-review","name":"review-aggregation.node.nr-1","kind":"reviewer","required":true,"verdict":"satisfied"},"review_state":"approved"}`)
		default:
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := flowclient.New(config.ClientConfig{ServerURL: server.URL, Token: "worker-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	report := workerexec.VerdictReport{
		Verdict: "satisfied",
		Reason:  "two deferred issues",
		Comments: []workerexec.ReviewCommentReport{
			{
				SHA: "head-1", File: "internal/cache.go", Line: 42, Body: "The legacy cache has no size bound.",
				Severity: "medium", IntroducedByChange: boolPtr(true), Requirement: "cache memory remains bounded",
				TaskAction: &workerexec.ReviewTaskActionReport{Action: "create_task", Title: "Bound the legacy cache", Body: "Add a configurable cache bound."},
			},
			{
				SHA: "head-1", File: "internal/board.ts", Line: 7, Body: "Cancelled mutations leave a stale pending status.",
				Severity: "medium", IntroducedByChange: boolPtr(true), Requirement: "pending status clears on cancel",
				TaskAction: &workerexec.ReviewTaskActionReport{Action: "use_existing_task", TaskID: "t-review-0001"},
			},
		},
	}
	taskID := "t-review"
	job := flowworker.Job{
		ID: "j-review", TaskID: &taskID, Role: flowworker.RoleReviewer,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{
			"blocking":           true,
			"review_aggregation": true,
		},
	}
	result := workerexec.RunResult{
		ExitCode: 0,
		Payload: workerexec.JobPayload{
			CheckName:          "review-aggregation.node.nr-1",
			ChangeID:           "ch-review",
			CompletionProtocol: checkverdict.CompletionProtocol,
		},
		VerdictReport: &report,
	}
	var stdout bytes.Buffer
	verdict, err := reportCheckIfNeeded(context.Background(), client, job, flowworker.Lease{ID: "l-review"}, result, &stdout)
	if err != nil || verdict != coordinator.CheckSatisfied {
		t.Fatalf("reportCheckIfNeeded verdict=%s err=%v stdout=%q", verdict, err, stdout.String())
	}
	if followUpCalls != 2 {
		t.Fatalf("follow-up calls = %d, want 2", followUpCalls)
	}
	if reported.Verdict != string(coordinator.CheckSatisfied) {
		t.Fatalf("reported verdict = %q, want satisfied", reported.Verdict)
	}
	for _, want := range []string{
		"[t-review-0002](/ui/tasks/t-review-0002)",
		"stale pending status",
		"Follow-up task actions failed (non-blocking):",
		"internal/board.ts:7 (use_existing_task)",
		"review_follow_up_failed: review follow-up task must be open",
	} {
		if !strings.Contains(reported.Details, want) {
			t.Fatalf("reported details %q missing %q", reported.Details, want)
		}
	}
}

// A non-aggregation reviewer has no business emitting task_action, but dropping
// that action must not error the check: the finding itself is already retained
// in the details, and the ignored action is recorded alongside it.
func TestNonAggregationReviewerTaskActionDegradesToDetails(t *testing.T) {
	report := workerexec.VerdictReport{Comments: []workerexec.ReviewCommentReport{{
		SHA: "abc123", File: "auth.go", Line: 42, Body: "Pre-existing issue.",
		Severity: "medium", IntroducedByChange: boolPtr(false), Requirement: "authorize requests",
		TaskAction: &workerexec.ReviewTaskActionReport{Action: "create_task", Title: "Follow up on auth hardening"},
	}}}
	var stdout bytes.Buffer
	results, failures, err := applyVerdictActions(context.Background(), nil, coordinator.CheckKindReviewer, false, false, "", flowworker.Lease{}, workerexec.RunResult{}, report, &stdout)
	if err != nil {
		t.Fatalf("applyVerdictActions error = %v, want nil", err)
	}
	if len(results) != 0 {
		t.Fatalf("follow-up results = %v, want none", results)
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "auth.go:42") || !strings.Contains(failures[0], "only a review aggregation job may apply task_action") {
		t.Fatalf("follow-up failures = %v", failures)
	}
	details := appendFollowUpActionFailures("review complete", failures)
	if !strings.Contains(details, "Follow-up task actions failed (non-blocking):") || !strings.Contains(details, "auth.go:42") {
		t.Fatalf("details = %q", details)
	}
}

func TestStructuredCheckResultIsAuthoritativeForJobState(t *testing.T) {
	result := workerexec.RunResult{
		FinalState: flowworker.JobFailed,
		ExitCode:   1,
		Err:        errors.New("diagnostic process failure"),
	}
	for _, verdict := range []coordinator.CheckVerdict{coordinator.CheckSatisfied, coordinator.CheckBlocked} {
		if got := finalStateForCheckReport(result, verdict, nil, false); got != flowworker.JobFinished {
			t.Fatalf("final state for %s verdict = %s, want finished", verdict, got)
		}
	}
	if got := finalStateForCheckReport(result, coordinator.CheckErrored, errors.New("invalid verdict"), false); got != flowworker.JobFailed {
		t.Fatalf("final state for execution error = %s, want failed", got)
	}
	if got := finalStateForCheckReport(result, coordinator.CheckSatisfied, nil, true); got != flowworker.JobCanceled {
		t.Fatalf("final state for stale result = %s, want canceled", got)
	}
}

func TestLogLevelFlagEnablesDebugLogging(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--log-level", "debug", "--version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") || !strings.Contains(stderr.String(), "flow-worker command start") {
		t.Fatalf("stderr missing debug log: %q", stderr.String())
	}
}

func TestWorkerJoinsWhenConfigOmitsToken(t *testing.T) {
	fixture := newWorkerTestFixture(t)
	server, err := api.NewServer(api.ServerOptions{
		Registry:        fixture.Registry,
		OwnerToken:      "owner-token",
		HookToken:       "hook-token",
		WorkerJoinToken: "join-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  1,
		toolYAML:       toolYAML,
		omitToken:      true,
	})
	t.Setenv("FLOW_WORKER_JOIN_TOKEN", "join-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--register-only", "--heartbeat-ttl=1s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q, stdout = %q", exitCode, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "registered: w-local") || !strings.Contains(stdout.String(), "claim: disabled") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if value := os.Getenv("FLOW_WORKER_JOIN_TOKEN"); value != "" {
		t.Fatalf("FLOW_WORKER_JOIN_TOKEN remained set as %q", value)
	}
}

func TestReadySessionHelperProcess(t *testing.T) {
	if os.Getenv("WORKER_READY_HELPER") != "1" {
		return
	}
	client, err := flowclient.New(config.ClientConfig{
		ServerURL: os.Getenv("FLOW_COORDINATOR_URL"),
		Token:     os.Getenv("FLOW_SESSION_TOKEN"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create ready helper client: %v\n", err)
		os.Exit(2)
	}
	if _, err := client.ReadySession(os.Getenv("FLOW_SESSION_ID")); err != nil {
		fmt.Fprintf(os.Stderr, "ready session: %v\n", err)
		os.Exit(2)
	}
	if path := strings.TrimSpace(os.Getenv("READY_HELPER_DONE_FILE")); path != "" {
		if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write ready marker: %v\n", err)
			os.Exit(2)
		}
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

func TestWorkerRetriesTransientCoordinatorHeartbeatBeforeClaim(t *testing.T) {
	t.Parallel()
	fixture := newWorkerTestFixture(t)
	server := fixture.Server
	var heartbeatAttempts atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/workers/heartbeat" && heartbeatAttempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"restart","message":"coordinator restarting"}}`))
			return
		}
		server.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  1,
		toolYAML:       toolYAML,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"worker transient error: heartbeat worker: restart: coordinator restarting", "claimed: none"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if heartbeatAttempts.Load() < 2 {
		t.Fatalf("heartbeat attempts = %d, want retry after transient failure", heartbeatAttempts.Load())
	}
}

func TestWorkerLeaseHeartbeatRecoversAfterTransientCoordinatorRenewalFailure(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	scriptPath := writeWorkerScript(t, `#!/bin/sh
sleep 7
printf renew-ok > "$1"
`)
	outPath := filepath.Join(t.TempDir(), "renew.out")
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       9,
		Payload: map[string]any{
			"entrypoint": map[string]any{
				"argv":  []string{scriptPath, outPath},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	server := fixture.Server
	var renewAttempts atomic.Int32
	var protectOnce sync.Once
	var protectErr error
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/workers/renew" {
			attempt := renewAttempts.Add(1)
			if attempt <= 4 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"code":"restart","message":"coordinator restarting"}}`))
				return
			}
			protectOnce.Do(func() {
				_, protectErr = fixture.Queue.ExtendActiveLeaseDeadlines(ctx, time.Now().UTC().Add(2*time.Minute))
			})
			if protectErr != nil {
				http.Error(w, protectErr.Error(), http.StatusInternalServerError)
				return
			}
		}
		server.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  1,
		toolYAML:       toolYAML,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "3s", "--heartbeat-ttl", "2s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q\nstdout = %s", exitCode, stderr.String(), stdout.String())
	}
	output := stdout.String()
	for _, want := range []string{"renew transient error: restart: coordinator restarting", "renewed:", "released: " + job.ID + " state=finished"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if renewAttempts.Load() < 5 {
		t.Fatalf("renew attempts = %d, want retries beyond the original lease deadline", renewAttempts.Load())
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read worker output: %v", err)
	}
	if string(contents) != "renew-ok" {
		t.Fatalf("worker output file = %q", string(contents))
	}
	released, err := fixture.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if released.State != flowworker.JobFinished {
		t.Fatalf("job state = %q, want finished", released.State)
	}
}

func TestLeaseHeartbeatCancelsJobAfterAuthoritativeLeaseLoss(t *testing.T) {
	t.Parallel()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/workers/heartbeat":
			_, _ = w.Write([]byte(`{"worker":{"id":"w-local"}}`))
		case "/v2/workers/renew":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"renew_lease_failed","message":"lease is not renewable"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(httpServer.Close)

	client, err := flowclient.New(config.ClientConfig{
		ServerURL: httpServer.URL,
		Token:     "worker-token",
	})
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	jobCtx, cancelJob := context.WithCancelCause(context.Background())
	heartbeat := startLeaseHeartbeat(
		client,
		config.WorkerConfig{WorkerID: "w-local"},
		flowworker.Lease{ID: "l-lost", ExpiresAt: time.Now().UTC().Add(time.Minute)},
		workerTimings{LeaseDuration: 2 * time.Second, HeartbeatTTL: 2 * time.Second},
		io.Discard,
		cancelJob,
	)

	select {
	case <-jobCtx.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("job context was not canceled after authoritative lease loss")
	}
	heartbeatErr := heartbeat.Stop()
	if heartbeatErr == nil || !isLeaseNotRenewable(heartbeatErr) {
		t.Fatalf("heartbeat error = %v, want nonrenewable lease", heartbeatErr)
	}
	if cause := context.Cause(jobCtx); cause == nil || !isLeaseNotRenewable(cause) {
		t.Fatalf("job cancellation cause = %v, want nonrenewable lease", cause)
	}
}

func TestWorkerLegacyCapacityMagnitudeRunsOneSequentialClaim(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	scriptPath := writeWorkerScript(t, "#!/bin/sh\nexit 0\n")
	enqueue := func(priority int) flowworker.Job {
		job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
			Role:           flowworker.RoleCI,
			CapacityBucket: flowworker.BucketEphemeral,
			Priority:       priority,
			Payload: map[string]any{"entrypoint": map[string]any{
				"argv": []string{scriptPath}, "shell": false,
			}},
		})
		if err != nil {
			t.Fatalf("enqueue job: %v", err)
		}
		return job
	}
	firstJob := enqueue(10)
	secondJob := enqueue(9)

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL,
		capacityBucket: "ephemeral",
		capacityCount:  7,
		toolYAML:       toolYAML,
	})

	var stdout, stderr bytes.Buffer
	if exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exitCode = %d, stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	first, err := fixture.Queue.GetJob(ctx, firstJob.ID)
	if err != nil {
		t.Fatalf("get first job: %v", err)
	}
	second, err := fixture.Queue.GetJob(ctx, secondJob.ID)
	if err != nil {
		t.Fatalf("get second job: %v", err)
	}
	if first.State != flowworker.JobFinished || second.State != flowworker.JobQueued {
		t.Fatalf("job states = %s, %s; want finished, queued", first.State, second.State)
	}
	registered, err := fixture.Directory.GetWorker(ctx, "w-local")
	if err != nil {
		t.Fatalf("get registered worker: %v", err)
	}
	if registered.CapacityEphemeral != 1 {
		t.Fatalf("registered ephemeral acceptance = %d, want 1", registered.CapacityEphemeral)
	}
}

func TestWorkerConsoleCleanExitReleasesSession(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)

	outPath := filepath.Join(t.TempDir(), "console.out")
	scriptPath := writeWorkerScript(t, `#!/bin/sh
printf console-exit > "$1"
`)
	ensured, err := fixture.Sessions.EnsureConsoleJob(ctx, coordinator.EnsureConsoleJobInput{
		Harness: "harness",
		Entrypoint: map[string]any{
			"argv":  []string{scriptPath, outPath},
			"shell": false,
		},
	})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}

	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL:    httpServer.URL,
		agentHarnessLabel: true,
		capacityBucket:    "persistent_agent",
		capacityCount:     1,
		toolYAML:          toolYAML,
		principal:         true,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"claimed: " + ensured.Job.ID, "running: " + ensured.Job.ID + " state=running", "released: " + ensured.Job.ID + " state=finished"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "persistent session active:") {
		t.Fatalf("console session stayed active after clean exit:\n%s", output)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read console output: %v", err)
	}
	if string(contents) != "console-exit" {
		t.Fatalf("console output file = %q", string(contents))
	}

	finished, err := fixture.Queue.GetJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("get console job: %v", err)
	}
	if finished.State != flowworker.JobFinished {
		t.Fatalf("console job state = %q, want finished", finished.State)
	}
	session, ok, err := fixture.Sessions.LatestSessionForJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("latest console session: %v", err)
	}
	if !ok {
		t.Fatal("latest console session not found")
	}
	if session.RuntimeState != coordinator.SessionFinished || session.FinishedAt == nil {
		t.Fatalf("console session = %+v, want finished", session)
	}
	lease, err := fixture.Queue.GetLease(ctx, session.LeaseID)
	if err != nil {
		t.Fatalf("get console lease: %v", err)
	}
	if lease.ReleasedAt == nil {
		t.Fatal("console lease ReleasedAt is nil")
	}
	current, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console: %v", err)
	}
	if current.Active {
		t.Fatalf("current console = %+v, want inactive", current)
	}
}

func TestWorkerStoppedTaskConsoleReportsProcessExit(t *testing.T) {
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)

	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Stopped repair console"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureTaskConsoleJob(ctx, coordinator.EnsureTaskConsoleJobInput{
		TaskID:  task.ID,
		Harness: "harness",
	})
	if err != nil {
		t.Fatalf("ensure task console job: %v", err)
	}
	startedPath := filepath.Join(t.TempDir(), "console-started")
	stopPath := filepath.Join(t.TempDir(), "console-stop")
	scriptPath := writeWorkerScript(t, `#!/bin/sh
set -eu
: > "$1"
while [ ! -f "$2" ]; do sleep 0.1; done
`)
	payload := ensured.Job.Payload
	payload["entrypoint"] = map[string]any{
		"argv":       []string{scriptPath, startedPath, stopPath},
		"shell":      false,
		"persistent": true,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal task console payload: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE jobs SET payload_json = ? WHERE id = ?`, string(payloadJSON), ensured.Job.ID); err != nil {
		t.Fatalf("install blocking task console entrypoint: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL: httpServer.URL, agentHarnessLabel: true,
		capacityBucket: "persistent_agent", capacityCount: 1,
		toolYAML: toolYAML, principal: true,
	})

	type runResult struct {
		exitCode int
		stdout   string
		stderr   string
	}
	done := make(chan runResult, 1)
	go func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "3s"}, &stdout, &stderr)
		done <- runResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
	}()

	waitForWorkerFile(t, startedPath, 15*time.Second)
	session := waitForWorkerSessionState(t, fixture, task.ID, coordinator.SessionStarting, 15*time.Second)
	state, err := fixture.Sessions.StopConvergenceRepairConsole(ctx, task.ID)
	if err != nil {
		t.Fatalf("stop repair console: %v", err)
	}
	if !state.Active || state.Session == nil || state.Session.ID != session.ID {
		t.Fatalf("stopped repair console state = %+v, want active process-exit fence", state)
	}
	// Let the worker observe the canceled lease before the local tmux process
	// exits, exercising the lease-loss completion path.
	time.Sleep(1500 * time.Millisecond)
	if err := os.WriteFile(stopPath, []byte("stop\n"), 0o600); err != nil {
		t.Fatalf("signal console exit: %v", err)
	}

	select {
	case result := <-done:
		if result.exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q, stdout = %q", result.exitCode, result.stderr, result.stdout)
		}
		if !strings.Contains(result.stdout, "console process exit acknowledged: "+session.ID) {
			t.Fatalf("worker did not acknowledge stopped process exit:\n%s", result.stdout)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("worker did not finish after stopped console process exited")
	}

	finished, err := fixture.Sessions.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("get stopped console session: %v", err)
	}
	if finished.RuntimeState != coordinator.SessionFinished || finished.FinishedAt == nil {
		t.Fatalf("stopped console session = %+v, want finished", finished)
	}
}

func TestWorkerConsoleNonZeroExitReleasesSession(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)

	outPath := filepath.Join(t.TempDir(), "console-failed.out")
	scriptPath := writeWorkerScript(t, `#!/bin/sh
printf console-failed > "$1"
exit 42
`)
	ensured, err := fixture.Sessions.EnsureConsoleJob(ctx, coordinator.EnsureConsoleJobInput{
		Harness: "harness",
		Entrypoint: map[string]any{
			"argv":  []string{scriptPath, outPath},
			"shell": false,
		},
	})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL:    httpServer.URL,
		agentHarnessLabel: true,
		capacityBucket:    "persistent_agent",
		capacityCount:     1,
		toolYAML:          toolYAML,
		principal:         true,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"claimed: " + ensured.Job.ID, "exit=42", "released: " + ensured.Job.ID + " state=finished"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "persistent session active:") {
		t.Fatalf("console session stayed active after non-zero exit:\n%s", output)
	}
	contents, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read console output: %v", err)
	}
	if string(contents) != "console-failed" {
		t.Fatalf("console output file = %q", string(contents))
	}

	finished, err := fixture.Queue.GetJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("get console job: %v", err)
	}
	if finished.State != flowworker.JobFinished {
		t.Fatalf("console job state = %q, want finished", finished.State)
	}
	session, ok, err := fixture.Sessions.LatestSessionForJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("latest console session: %v", err)
	}
	if !ok {
		t.Fatal("latest console session not found")
	}
	if session.RuntimeState != coordinator.SessionFinished || session.FinishedAt == nil {
		t.Fatalf("console session = %+v, want finished", session)
	}
	lease, err := fixture.Queue.GetLease(ctx, session.LeaseID)
	if err != nil {
		t.Fatalf("get console lease: %v", err)
	}
	if lease.ReleasedAt == nil {
		t.Fatal("console lease ReleasedAt is nil")
	}
	current, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console: %v", err)
	}
	if current.Active {
		t.Fatalf("current console = %+v, want inactive", current)
	}
}

func TestWorkerRegisterOnlyDoesNotClaimJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: `+httpServer.URL+`
token: worker-token
work_dir: `+filepath.ToSlash(t.TempDir())+`
capacity:
  ephemeral: 1
`+toolYAML+`
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--register-only", "--once"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claim: disabled") {
		t.Fatalf("worker output = %q", stdout.String())
	}

	stillQueued, err := fixture.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stillQueued.State != flowworker.JobQueued {
		t.Fatalf("job state = %q, want queued", stillQueued.State)
	}
}

func TestWorkerUsesDiscoveredWorkerConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	job, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	dataDir := t.TempDir()
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: httpServer.URL,
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	workerConfigPath := config.DefaultWorkerConfigPath(dataDir)
	if err := os.WriteFile(workerConfigPath, []byte(`worker_id: w-local
coordinator_url: `+httpServer.URL+`
token: worker-token
work_dir: `+filepath.ToSlash(t.TempDir())+`
capacity:
  ephemeral: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"--register-only"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claim: disabled") {
		t.Fatalf("worker output = %q", stdout.String())
	}

	stillQueued, err := fixture.Queue.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stillQueued.State != flowworker.JobQueued {
		t.Fatalf("job state = %q, want queued", stillQueued.State)
	}
}

func TestWorkerConfigUsesDiscoveredWorkerConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dataDir := t.TempDir()
	configPath, err := config.DefaultClientConfigPath()
	if err != nil {
		t.Fatalf("default client config path: %v", err)
	}
	if err := config.WriteClientConfig(configPath, config.ClientConfig{
		ServerURL: "http://127.0.0.1:8421",
		DataDir:   dataDir,
	}); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	workerConfigPath := config.DefaultWorkerConfigPath(dataDir)
	if err := os.WriteFile(workerConfigPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
token: worker-token
work_dir: /tmp/worker
labels:
  local: "true"
capacity:
  persistent_agent: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"config"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"worker_id: w-local", "protocol: 6", "labels: 3", "capacity_persistent_agent: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("config output missing %q:\n%s", want, output)
		}
	}
}

func TestRegistrationLabelsAdvertiseAvailableHarnessesAndDropGenericAgent(t *testing.T) {
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "harness"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", toolDir)

	labels := registrationLabels(map[string]string{
		"agent": "true",
		"local": "true",
	})
	if labels["agent"] != "" {
		t.Fatalf("labels = %#v, generic agent label should be dropped", labels)
	}
	for _, want := range []string{"local", "agent.harness.harness"} {
		if labels[want] != "true" {
			t.Fatalf("labels = %#v, missing %s=true", labels, want)
		}
	}
}

func TestRegistrationLabelsReportHarnessAvailability(t *testing.T) {
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "harness"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", toolDir)

	labels, availability := registrationLabelsWithAvailability(map[string]string{"local": "true"})
	if labels["agent.harness.harness"] != "true" {
		t.Fatalf("labels = %#v, want harness label", labels)
	}
	statusByName := map[string]flowharness.Availability{}
	for _, status := range availability {
		statusByName[status.Name] = status
	}
	if len(availability) != 1 || !statusByName[flowharness.Harness].Available {
		t.Fatalf("availability = %#v, want only harness available", statusByName)
	}
}

func TestLogAgentHarnessAvailabilityIncludesDetectedAndMissing(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	logAgentHarnessAvailability([]flowharness.Availability{
		{Name: flowharness.Harness, Executable: "harness", Path: "/usr/bin/harness", Available: true, Reason: "usability check passed"},
		{Name: "bogus", Executable: "bogus", Available: false, Reason: "executable not found", Error: "not found"},
	})

	output := logs.String()
	for _, want := range []string{
		"msg=\"flow-worker agent harness detected\"",
		"harness=harness",
		"msg=\"flow-worker agent harness not detected\"",
		"harness=bogus",
		"available=harness",
		"unavailable=bogus",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("log output missing %q:\n%s", want, output)
		}
	}
}

func TestRegistrationHarnessModelsUsesHarnessCatalogWhenAvailable(t *testing.T) {
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "harness"), []byte(`#!/bin/sh
if [ "$*" = "--models --format json" ]; then
  printf '%s\n' '{"version":1,"model_count":1,"models":[{"target_id":"anthropic:claude-opus-4-8","display_name":"Claude Opus 4.8","provider_label":"Anthropic","model_label":"claude-opus-4-8","reasoning":true}]}'
  exit 0
fi
exit 1
`), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", toolDir)

	models := registrationHarnessModels(map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"})
	if len(models) != 1 || models[0].QualifiedID != "anthropic:claude-opus-4-8" {
		t.Fatalf("registration harness models = %#v", models)
	}
	if models[0].TargetID != "anthropic:claude-opus-4-8" || models[0].ProviderID != "anthropic" || models[0].ModelID != "claude-opus-4-8" {
		t.Fatalf("registration harness model normalized = %#v", models[0])
	}
	if models[0].Reasoning.Options[0].Type != "profile" {
		t.Fatalf("reasoning option = %#v, want profile", models[0].Reasoning.Options[0])
	}
	if values := models[0].Reasoning.Options[0].Values; len(values) != 7 || values[0] != "none" || values[6] != "max" {
		t.Fatalf("reasoning values = %#v", values)
	}
}

func TestRegistrationHarnessModelsFallsBackWhenCatalogFails(t *testing.T) {
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolDir, "harness"), []byte("#!/bin/sh\nexit 12\n"), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", toolDir)

	models := registrationHarnessModels(map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"})
	if len(models) != 0 {
		t.Fatalf("registration harness models = %#v, want none", models)
	}
}

func TestWorkerStartupReaperUsesWorkerToken(t *testing.T) {
	putFakeEmptyTmuxOnPath(t)
	fixture := newWorkerTestFixture(t)
	server := fixture.Server
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: `+httpServer.URL+`
token: worker-token
work_dir: `+filepath.ToSlash(t.TempDir())+`
capacity:
  ephemeral: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "owner token is required") {
		t.Fatalf("startup reaper used owner-only job listing: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "reaped orphaned tmux sessions: 0") {
		t.Fatalf("stderr = %q, want startup reaper summary", stderr.String())
	}
}

// TestRunWorkerLoopContinuesAfterJobError is the regression for one failed job
// taking down the whole worker process (and abandoning its sibling jobs): in
// service mode a job-scoped failure must be logged and survived, leaving the
// loop free to claim and finish the next job.
func TestRunWorkerLoopContinuesAfterJobError(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)
	if _, err := fixture.Directory.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
		HeartbeatTTL:      time.Minute,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	failingJob, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       2,
		Payload: map[string]any{
			"base":   "missing-base",
			"branch": "missing-base",
			"entrypoint": map[string]any{
				"argv":  []string{"true"},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue failing job: %v", err)
	}

	nextOut := filepath.Join(t.TempDir(), "next.out")
	nextScript := writeWorkerScript(t, `#!/bin/sh
printf next-ok > "$1"
`)
	nextJob, err := fixture.Queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       1,
		Payload: map[string]any{
			"base":   "main",
			"branch": "main",
			"entrypoint": map[string]any{
				"argv":  []string{nextScript, nextOut},
				"shell": false,
			},
		},
	})
	if err != nil {
		t.Fatalf("enqueue next job: %v", err)
	}

	coordinatorServer := httptest.NewServer(fixture.Server)
	t.Cleanup(coordinatorServer.Close)
	target, err := url.Parse(coordinatorServer.URL)
	if err != nil {
		t.Fatalf("parse coordinator url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// The gate ends the otherwise endless service loop: once closed, the next
	// pre-claim heartbeat fails non-retryably and runWorkerLoop returns.
	var gateClosed atomic.Bool
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gateClosed.Load() {
			http.Error(w, "test gate closed", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)

	cfg := config.WorkerConfig{
		WorkerID:       "w-local",
		CoordinatorURL: gate.URL,
		Token:          "worker-token",
		WorkDir:        t.TempDir(),
		Tmux: config.WorkerTmuxConfig{
			SocketPath: isolatedWorkerTmuxSocket(t),
		},
		Git: config.WorkerGitConfig{
			Principal: "worker:w-local",
		},
	}
	client, err := newWorkerClient(cfg)
	if err != nil {
		t.Fatalf("create worker client: %v", err)
	}
	var diskReady atomic.Bool
	diskReady.Store(true)
	maintenance := newWorkerMaintenance(cfg, config.ResolvedWorkerCleanup{
		Interval:          time.Hour,
		OrphanGrace:       time.Hour,
		MinFreePercent:    0.0001,
		ResumeFreePercent: 0.0002,
	}, client, nil, &diskReady)
	timings := workerTimings{
		ClaimWait:     0,
		LeaseDuration: 30 * time.Second,
		HeartbeatTTL:  30 * time.Second,
	}

	var stdout bytes.Buffer
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runWorkerLoop(context.Background(), client, cfg, timings, maintenance, false, &stdout)
	}()

	waitForWorkerJobState(t, fixture, failingJob.ID, flowworker.JobFailed, 30*time.Second)
	waitForWorkerJobState(t, fixture, nextJob.ID, flowworker.JobFinished, 30*time.Second)
	waitForWorkerFile(t, nextOut, 30*time.Second)
	gateClosed.Store(true)

	var loopErr error
	select {
	case loopErr = <-loopDone:
	case <-time.After(15 * time.Second):
		t.Fatalf("worker loop did not stop after gate closed; stdout:\n%s", stdout.String())
	}
	if loopErr == nil {
		t.Fatal("worker loop returned nil, want gate error")
	}
	var jobErr *jobError
	if errors.As(loopErr, &jobErr) {
		t.Fatalf("worker loop exited on job-scoped error %v; should have continued", loopErr)
	}
	output := stdout.String()
	if !strings.Contains(output, "job error:") || !strings.Contains(output, "continuing") {
		t.Fatalf("stdout missing job error continuation:\n%s", output)
	}
	finishedNext, err := fixture.Queue.GetJob(ctx, nextJob.ID)
	if err != nil {
		t.Fatalf("get next job: %v", err)
	}
	if finishedNext.State != flowworker.JobFinished {
		t.Fatalf("next job state = %q, want finished", finishedNext.State)
	}
	waitForWorkerPathAbsent(t, filepath.Join(cfg.WorkDir, "jobs", nextJob.ID), 15*time.Second)
}

func waitForWorkerJobState(t *testing.T, fixture workerTestFixture, jobID string, want flowworker.JobState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := fixture.Queue.GetJob(context.Background(), jobID)
		if err == nil && job.State == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	job, err := fixture.Queue.GetJob(context.Background(), jobID)
	t.Fatalf("job %s did not reach state %q within %s (state=%v err=%v)", jobID, want, timeout, job.State, err)
}

func workerTestRenderShellArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "-c" {
			quoted = append(quoted, arg)
			continue
		}
		quoted = append(quoted, workerTestShellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func waitForWorkerSessionState(t *testing.T, fixture workerTestFixture, taskID string, want coordinator.SessionRuntimeState, timeout time.Duration) coordinator.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last coordinator.Session
	for time.Now().Before(deadline) {
		sessions, err := fixture.Sessions.ListSessionsForTask(context.Background(), taskID, 1)
		if err == nil && len(sessions) > 0 {
			last = sessions[0]
			if sessions[0].RuntimeState == want {
				return sessions[0]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session for task %s did not reach state %q within %s (last=%+v)", taskID, want, timeout, last)
	return coordinator.Session{}
}

func workerTestShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func writeWorkerScript(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func waitForWorkerFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("file %s was not created within %s", path, timeout)
		case <-ticker.C:
		}
	}
}

func waitForWorkerPathAbsent(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("path still exists after %s: %s", timeout, path)
}

func isolatedWorkerTmuxSocket(t *testing.T) string {
	t.Helper()
	tmuxTmp, err := os.MkdirTemp("/tmp", "flow-worker-tmux-")
	if err != nil {
		t.Fatalf("create tmux dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tmuxTmp)
	})
	return filepath.Join(tmuxTmp, "tmux.sock")
}

func workerTmuxSessionExists(socketPath string, sessionName string) bool {
	args := []string{"has-session", "-t", sessionName}
	if strings.TrimSpace(socketPath) != "" {
		args = append([]string{"-S", socketPath}, args...)
	}
	return exec.Command("tmux", args...).Run() == nil
}

// workerConfigOptions describes the per-test variations of the worker
// worker.yaml fixture assembled by writeWorkerConfig. The constant fields
// (worker_id, token, and dynamic work_dir) are shared across
// every call site; only the fields below differ.
type workerConfigOptions struct {
	coordinatorURL    string // coordinator_url value (e.g. httpServer.URL)
	agentHarnessLabel bool   // include labels: { agent.harness.harness: "true" }
	capacityBucket    string // capacity bucket key (e.g. "ephemeral")
	capacityCount     int    // capacity bucket count
	toolYAML          string // tool/terminal/tmux config fragment
	principal         bool   // include git.principal: worker:w-local
	omitToken         bool   // leave token empty so the worker joins at startup
}

// writeWorkerConfig writes a worker worker.yaml into dir from opts and returns
// the config path. It assembles the shared constant fragments plus the varying
// capacity bucket, labels, and git fields used across the worker tests.
func writeWorkerConfig(t *testing.T, dir string, opts workerConfigOptions) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("worker_id: w-local\n")
	b.WriteString("coordinator_url: " + opts.coordinatorURL + "\n")
	if !opts.omitToken {
		b.WriteString("token: worker-token\n")
	}
	b.WriteString("work_dir: " + filepath.ToSlash(t.TempDir()) + "\n")
	if opts.agentHarnessLabel {
		b.WriteString("labels:\n  agent.harness.harness: \"true\"\n")
	}
	b.WriteString("capacity:\n")
	b.WriteString(fmt.Sprintf("  %s: %d\n", opts.capacityBucket, opts.capacityCount))
	b.WriteString(opts.toolYAML)
	if opts.principal {
		b.WriteString("git:\n")
		b.WriteString("  principal: worker:w-local\n")
	}
	configPath := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(configPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}
	return configPath
}

func workerToolConfigYAML(t *testing.T) (string, string) {
	t.Helper()
	socketPath := isolatedWorkerTmuxSocket(t)
	return `tmux:
  socket_path: ` + filepath.ToSlash(socketPath) + `
`, socketPath
}

func putFakeEmptyTmuxOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
case "$1" in
list-sessions)
  exit 0
  ;;
kill-session)
  exit 0
  ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func requireWorkerTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

func TestWorkerConfigLoadsYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(configPath, []byte(`worker_id: w-local
coordinator_url: http://127.0.0.1:8421
work_dir: /tmp/worker
labels:
  local: "true"
capacity:
  persistent_agent: 1
`), 0o600); err != nil {
		t.Fatalf("write worker config: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"config", "-c", configPath}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"worker_id: w-local", "protocol: 6", "labels: 3", "capacity_persistent_agent: 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("config output missing %q:\n%s", want, output)
		}
	}
}

// TestWorkerConsoleRunErrorReleasesSessionAndSurfacesError is a regression test
// for the console-crash masking + lease leak: when a console session's worker
// step fails (RunResult.Err != nil), the console branch must still release the
// lease through /v2/console and surface the real error, never falling through to
// the generic persistent-session process-exit path (which rejects the console
// role, leaking the lease and masking the error). The reserved FLOW_* env key
// makes RunJob fail in validateEntrypoint deterministically before any tmux work.
func TestWorkerConsoleRunErrorReleasesSessionAndSurfacesError(t *testing.T) {
	t.Parallel()
	requireWorkerTool(t, "git")
	requireWorkerTool(t, "tmux")
	ctx := context.Background()
	fixture := newWorkerTestFixture(t)

	ensured, err := fixture.Sessions.EnsureConsoleJob(ctx, coordinator.EnsureConsoleJobInput{
		Harness: "harness",
		Entrypoint: map[string]any{
			"argv":  []string{"/bin/sh", "-c", "true"},
			"shell": false,
			// A reserved FLOW_* override is rejected by validateEntrypoint, so
			// RunJob returns RunResult.Err != nil before reaching the agent.
			"env": map[string]string{"FLOW_INJECTED": "1"},
		},
	})
	if err != nil {
		t.Fatalf("ensure console job: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)

	toolYAML, _ := workerToolConfigYAML(t)
	configPath := writeWorkerConfig(t, t.TempDir(), workerConfigOptions{
		coordinatorURL:    httpServer.URL,
		agentHarnessLabel: true,
		capacityBucket:    "persistent_agent",
		capacityCount:     1,
		toolYAML:          toolYAML,
		principal:         true,
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"-c", configPath, "--once", "--claim-wait", "0s", "--lease", "30s"}, &stdout, &stderr)
	// The job fails, so the worker exits non-zero and reports the error on stderr.
	if exitCode != 1 {
		t.Fatalf("exitCode = %d, want 1; stdout = %q, stderr = %q", exitCode, stdout.String(), stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "released: "+ensured.Job.ID+" state=finished") {
		t.Fatalf("worker output missing console release:\n%s\nstderr:\n%s", output, stderr.String())
	}
	// The console branch must surface the real run error, never the masked
	// process-exit error from the generic persistent-session path.
	if strings.Contains(output, "report persistent session process exit") ||
		strings.Contains(stderr.String(), "report persistent session process exit") {
		t.Fatalf("console run error masked by process-exit path:\nstdout=%s\nstderr=%s", output, stderr.String())
	}
	if !strings.Contains(stderr.String(), "run job:") {
		t.Fatalf("worker stderr missing real run error: %q", stderr.String())
	}

	session, ok, err := fixture.Sessions.LatestSessionForJob(ctx, ensured.Job.ID)
	if err != nil {
		t.Fatalf("latest console session: %v", err)
	}
	if !ok {
		t.Fatal("latest console session not found")
	}
	lease, err := fixture.Queue.GetLease(ctx, session.LeaseID)
	if err != nil {
		t.Fatalf("get console lease: %v", err)
	}
	if lease.ReleasedAt == nil {
		t.Fatal("console lease ReleasedAt is nil; lease leaked after console run error")
	}
	current, err := fixture.Sessions.CurrentConsole(ctx)
	if err != nil {
		t.Fatalf("current console: %v", err)
	}
	if current.Active {
		t.Fatalf("current console = %+v, want inactive", current)
	}
}
