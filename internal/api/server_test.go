package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/handoff"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/lifecycle"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestServerRequiresOwnerToken(t *testing.T) {
	server := newTestServer(t)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/issues", nil)
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", response.Code)
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/issues", nil)
	request.Header.Set("Authorization", "Bearer wrong")
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", response.Code)
	}
}

func TestServerReportsProtocolMismatch(t *testing.T) {
	server := newTestServer(t)

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, "/v1/issues", nil)
	request.Header.Set(protocolHeader, "999")
	server.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	var body errorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "protocol_mismatch" {
		t.Fatalf("error code = %q, want protocol_mismatch", body.Error.Code)
	}
}

func TestHarnessOptionsUseLiveWorkerHarnessLabels(t *testing.T) {
	server := newTestServer(t)

	if _, err := server.registry.Directory().RegisterWorker(context.Background(), flowworker.RegisterWorkerInput{
		ID: "w-codex",
		Labels: map[string]string{
			flowharness.AgentHarnessLabel(flowharness.Codex):   "true",
			flowharness.AgentHarnessLabel(flowharness.Harness): "true",
		},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register codex/harness worker: %v", err)
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v1/harnesses", nil, http.StatusOK, &response)

	agents := harnessOptionNames(response.Agents)
	if !agents[flowharness.Codex] || !agents[flowharness.Harness] {
		t.Fatalf("agent harness options = %+v, want codex and harness", response.Agents)
	}
	if agents[flowharness.Claude] {
		t.Fatalf("agent harness options = %+v, did not expect claude without binary", response.Agents)
	}
	consoles := harnessOptionNames(response.Consoles)
	for _, want := range []string{flowharness.Codex, flowharness.Harness, flowharness.Shell} {
		if !consoles[want] {
			t.Fatalf("console harness options = %+v, missing %s", response.Consoles, want)
		}
	}
	if consoles[flowharness.Claude] {
		t.Fatalf("console harness options = %+v, did not expect claude without binary", response.Consoles)
	}
}

func TestHarnessOptionsIncludeModelsAvailableOnEveryLiveHarnessWorker(t *testing.T) {
	server := newTestServer(t)
	ctx := context.Background()
	minBudget := 1024

	common := flowharness.Model{
		ProviderID:   "anthropic",
		ProviderName: "anthropic",
		ModelID:      "claude-opus-4-8",
		QualifiedID:  "anthropic:claude-opus-4-8",
		ModelName:    "claude-opus-4-8",
		Harness:      flowharness.Harness,
		Reasoning: flowharness.ReasoningInfo{
			Supported: true,
			Options: []flowharness.ReasoningOption{{
				Type:   "effort",
				Values: []string{"low", "high"},
			}},
		},
	}
	onlyFirst := flowharness.Model{
		ProviderID:  "google",
		ModelID:     "gemini-3.5-flash",
		QualifiedID: "google:gemini-3.5-flash",
		Harness:     flowharness.Harness,
		Reasoning: flowharness.ReasoningInfo{
			Supported: true,
			Options: []flowharness.ReasoningOption{{
				Type: "budget_tokens",
				Min:  &minBudget,
			}},
		},
	}

	for _, input := range []flowworker.RegisterWorkerInput{
		{
			ID: "w-harness-a",
			Labels: map[string]string{
				flowharness.AgentHarnessLabel(flowharness.Harness): "true",
			},
			HarnessModels:           []flowharness.Model{onlyFirst, common},
			CapacityPersistentAgent: 1,
			HeartbeatTTL:            time.Minute,
		},
		{
			ID: "w-harness-b",
			Labels: map[string]string{
				flowharness.AgentHarnessLabel(flowharness.Harness): "true",
			},
			HarnessModels:           []flowharness.Model{common},
			CapacityPersistentAgent: 1,
			HeartbeatTTL:            time.Minute,
		},
	} {
		if _, err := server.registry.Directory().RegisterWorker(ctx, input); err != nil {
			t.Fatalf("register harness worker %s: %v", input.ID, err)
		}
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v1/harnesses", nil, http.StatusOK, &response)

	var harnessOption *contract.HarnessOption
	for i := range response.Agents {
		if response.Agents[i].Name == flowharness.Harness {
			harnessOption = &response.Agents[i]
			break
		}
	}
	if harnessOption == nil {
		t.Fatalf("agent harness options = %+v, missing harness", response.Agents)
	}
	if len(harnessOption.Models) != 1 || harnessOption.Models[0].QualifiedID != common.QualifiedID {
		t.Fatalf("harness models = %+v, want only %s", harnessOption.Models, common.QualifiedID)
	}
	if got := harnessOption.Models[0].Reasoning.Options[0].Values; len(got) != 2 || got[0] != "low" || got[1] != "high" {
		t.Fatalf("harness reasoning values = %#v", got)
	}
}

func TestHarnessOptionsExcludeExpiredWorkers(t *testing.T) {
	server := newTestServer(t)

	ctx := context.Background()
	if _, err := server.registry.Directory().RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-claude",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Claude): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register live claude worker: %v", err)
	}
	if _, err := server.registry.Directory().RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-expired-harness",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register expired harness worker: %v", err)
	}
	if _, err := server.registry.global.DB().ExecContext(ctx, `UPDATE workers SET expires_at = ? WHERE id = ?`, "2020-01-01T00:00:00Z", "w-expired-harness"); err != nil {
		t.Fatalf("expire harness worker: %v", err)
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v1/harnesses", nil, http.StatusOK, &response)

	agents := harnessOptionNames(response.Agents)
	if agents[flowharness.Codex] || !agents[flowharness.Claude] || agents[flowharness.Harness] {
		t.Fatalf("agent harness options = %+v, want only claude", response.Agents)
	}
	consoles := harnessOptionNames(response.Consoles)
	if consoles[flowharness.Codex] || !consoles[flowharness.Claude] || consoles[flowharness.Harness] || !consoles[flowharness.Shell] {
		t.Fatalf("console harness options = %+v, want claude and shell", response.Consoles)
	}
}

func TestHarnessOptionsIncludeDefaultArgs(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	registry, err := NewRegistry(RegistryOptions{
		DataDir: dataDir,
		Global:  global,
		HarnessArgs: flowharness.Args{
			Codex: []string{"--model", "gpt-5"},
		},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	if err := registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
		Token: "owner-token",
		Scope: coordinator.TokenScopeOwner,
	}); err != nil {
		t.Fatalf("store owner token: %v", err)
	}
	server, err := NewServer(ServerOptions{Registry: registry, OwnerToken: "owner-token"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if _, err := registry.Directory().RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-codex",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register codex worker: %v", err)
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v1/harnesses", nil, http.StatusOK, &response)
	var codexDefaults []string
	for _, option := range response.Agents {
		if option.Name == flowharness.Codex {
			codexDefaults = option.DefaultArgs
		}
	}
	if len(codexDefaults) != 2 || codexDefaults[0] != "--model" || codexDefaults[1] != "gpt-5" {
		t.Fatalf("codex default args = %#v", codexDefaults)
	}
}

func TestServerIssueLifecycleAndBoard(t *testing.T) {
	fixture := newTestFixture(t)
	server := fixture.Server

	createResponse := issueResponse{}
	doJSONRequest(t, server, http.MethodPost, "/v1/issues", createIssueRequest{Title: "API issue", PlanMode: true}, http.StatusCreated, &createResponse)
	if createResponse.Issue.ID != "i-0001" {
		t.Fatalf("created issue ID = %q", createResponse.Issue.ID)
	}
	if !createResponse.Issue.PlanMode {
		t.Fatalf("created issue PlanMode = false, want true")
	}

	title := "Renamed API issue"
	priority := 7
	planMode := false
	editResponse := issueResponse{}
	doJSONRequest(t, server, http.MethodPatch, "/v1/issues/"+createResponse.Issue.ID, editIssueRequest{
		Title:    &title,
		Priority: &priority,
		PlanMode: &planMode,
	}, http.StatusOK, &editResponse)
	if editResponse.Issue.Title != title || editResponse.Issue.Priority != priority {
		t.Fatalf("edited issue mismatch: %+v", editResponse.Issue)
	}
	if editResponse.Issue.PlanMode {
		t.Fatalf("edited issue PlanMode = true, want false")
	}

	scheduleResponse := issueResponse{}
	doJSONRequest(t, server, http.MethodPost, "/v1/issues/"+createResponse.Issue.ID+"/schedule", scheduleIssueRequest{
		State: string(coordinator.ScheduleUpNext),
	}, http.StatusOK, &scheduleResponse)
	if scheduleResponse.Issue.ScheduleState != coordinator.ScheduleUpNext {
		t.Fatalf("ScheduleState = %q, want up_next", scheduleResponse.Issue.ScheduleState)
	}

	var listResponse issuesResponse
	doJSONRequest(t, server, http.MethodGet, "/v1/issues?schedule_state=up_next", nil, http.StatusOK, &listResponse)
	if len(listResponse.Issues) != 1 || listResponse.Issues[0].ID != createResponse.Issue.ID {
		t.Fatalf("list response = %+v", listResponse.Issues)
	}

	var board boardResponse
	doJSONRequest(t, server, http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.UpNext) != 1 || board.Board.UpNext[0].ID != createResponse.Issue.ID {
		t.Fatalf("board up_next = %+v", board.Board.UpNext)
	}

	closeResponse := issueResponse{}
	doJSONRequest(t, server, http.MethodPost, "/v1/issues/"+createResponse.Issue.ID+"/close", map[string]string{}, http.StatusOK, &closeResponse)
	if closeResponse.Issue.ScheduleState != coordinator.ScheduleClosed {
		t.Fatalf("closed ScheduleState = %q, want closed", closeResponse.Issue.ScheduleState)
	}

	stateResponse := issueResponse{}
	doJSONRequest(t, server, http.MethodPost, "/v1/issues/"+createResponse.Issue.ID+"/state", issueStateRequest{
		State: string(coordinator.IssueStateBacklog),
	}, http.StatusOK, &stateResponse)
	if stateResponse.Issue.ScheduleState != coordinator.ScheduleBacklog || stateResponse.Issue.TriageState != coordinator.TriageAccepted {
		t.Fatalf("state issue = %+v, want backlog/accepted", stateResponse.Issue)
	}
	if stateResponse.Issue.ClosedAt != nil {
		t.Fatalf("state issue ClosedAt = %v, want nil", stateResponse.Issue.ClosedAt)
	}
}

func TestIssueAttachmentUploadDetailAndDownload(t *testing.T) {
	fixture := newTestFixture(t)
	issue, err := fixture.Issues.CreateIssue(context.Background(), coordinator.CreateIssueInput{Title: "Attachment issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	uploadPath := "/v1/projects/" + fixture.Project.ID + "/issues/" + issue.ID + "/attachments"
	uploaded := uploadIssueAttachmentForTest(t, fixture, uploadPath, string(coordinator.IssueAttachmentStageReviewer), "review.png", "image/png", []byte("png-data"))
	if uploaded.Stage != coordinator.IssueAttachmentStageReviewer || uploaded.Filename != "review.png" || uploaded.ContentType != "image/png" {
		t.Fatalf("uploaded attachment = %+v", uploaded)
	}

	var detail issueResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v1/projects/"+fixture.Project.ID+"/issues/"+issue.ID, nil, http.StatusOK, &detail)
	if detail.Detail == nil || len(detail.Detail.Attachments) != 1 || detail.Detail.Attachments[0].ID != uploaded.ID {
		t.Fatalf("detail attachments = %+v", detail.Detail)
	}

	downloadPath := uploadPath + "/" + uploaded.ID
	download := getAs(t, fixture.Server, "owner-token", downloadPath+"?download=1")
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	if got := download.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("download content type = %q, want image/png", got)
	}
	if got := download.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, "review.png") {
		t.Fatalf("download disposition = %q", got)
	}
	if download.Body.String() != "png-data" {
		t.Fatalf("download body = %q", download.Body.String())
	}

	inline := getAs(t, fixture.Server, "owner-token", downloadPath)
	if got := inline.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("inline content type = %q, want image/png", got)
	}
	if got := inline.Header().Get("Content-Disposition"); !strings.Contains(got, "inline") {
		t.Fatalf("inline disposition = %q", got)
	}
}

func TestIssueAttachmentUnsafeContentTypesAreDownloadOnly(t *testing.T) {
	fixture := newTestFixture(t)
	issue, err := fixture.Issues.CreateIssue(context.Background(), coordinator.CreateIssueInput{Title: "Unsafe attachment issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	uploadPath := "/v1/projects/" + fixture.Project.ID + "/issues/" + issue.ID + "/attachments"
	for _, tc := range []struct {
		name        string
		filename    string
		contentType string
		body        string
	}{
		{name: "html", filename: "proof.html", contentType: "text/html", body: "<script>alert(1)</script>"},
		{name: "svg", filename: "proof.svg", contentType: "image/svg+xml", body: `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uploaded := uploadIssueAttachmentForTest(t, fixture, uploadPath, string(coordinator.IssueAttachmentStageReviewer), tc.filename, tc.contentType, []byte(tc.body))
			if uploaded.ContentType != tc.contentType {
				t.Fatalf("stored content type = %q, want %q", uploaded.ContentType, tc.contentType)
			}

			response := getAs(t, fixture.Server, "owner-token", uploadPath+"/"+uploaded.ID)
			if response.Code != http.StatusOK {
				t.Fatalf("attachment status = %d, want 200; body: %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("content type = %q, want application/octet-stream", got)
			}
			disposition := response.Header().Get("Content-Disposition")
			if !strings.Contains(disposition, "attachment") || strings.Contains(disposition, "inline") || !strings.Contains(disposition, tc.filename) {
				t.Fatalf("content disposition = %q, want attachment with filename %q", disposition, tc.filename)
			}
			if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("x-content-type-options = %q, want nosniff", got)
			}
			if response.Body.String() != tc.body {
				t.Fatalf("attachment body = %q", response.Body.String())
			}
		})
	}
}

func uploadIssueAttachmentForTest(t *testing.T, fixture testFixture, uploadPath string, stage string, filename string, contentType string, data []byte) coordinator.IssueAttachment {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("stage", stage); err != nil {
		t.Fatalf("write stage: %v", err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name":     "file",
		"filename": filename,
	}))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("create multipart part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write multipart part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	upload := httptest.NewRecorder()
	request := authorizedRequest(http.MethodPost, uploadPath, nil)
	request.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	request.ContentLength = int64(body.Len())
	request.Header.Set("Content-Type", writer.FormDataContentType())
	fixture.Server.ServeHTTP(upload, request)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201; body: %s", upload.Code, upload.Body.String())
	}
	var uploaded issueAttachmentResponse
	if err := json.NewDecoder(upload.Body).Decode(&uploaded); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	return uploaded.Attachment
}

func TestCreateUpNextIssueStartsAuthorJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	var created issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues", createIssueRequest{
		Title:         "Queued on create",
		ScheduleState: string(coordinator.ScheduleUpNext),
	}, http.StatusCreated, &created)
	if created.Issue.ScheduleState != coordinator.ScheduleUpNext {
		t.Fatalf("created issue = %+v, want up_next", created.Issue)
	}

	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Role != flowworker.RoleAuthor || jobs[0].IssueID == nil || *jobs[0].IssueID != created.Issue.ID || jobs[0].ChangeID == nil {
		t.Fatalf("jobs after queued create = %+v", jobs)
	}
}

func TestIdempotentCreateDoesNotDuplicateIssue(t *testing.T) {
	fixture := newTestFixture(t)

	first := issueResponse{}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues", createIssueRequest{
		Title: "Idempotent issue",
	}, http.StatusCreated, &first, idempotencyHeader, "create-1")

	second := issueResponse{}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues", createIssueRequest{
		Title: "Idempotent issue",
	}, http.StatusCreated, &second, idempotencyHeader, "create-1")

	if first.Issue.ID != second.Issue.ID {
		t.Fatalf("second idempotent create returned %s, want %s", second.Issue.ID, first.Issue.ID)
	}
	issues, err := fixture.Issues.ListIssues(context.Background(), coordinator.IssueFilter{})
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issue count = %d, want 1", len(issues))
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues", createIssueRequest{
		Title: "Different issue",
	}, http.StatusConflict, nil, idempotencyHeader, "create-1")
}

func TestConcurrentIdempotentCreateDoesNotDuplicateIssue(t *testing.T) {
	fixture := newTestFixture(t)

	const requests = 24
	start := make(chan struct{})
	results := make(chan issueResponse, requests)
	errors := make(chan string, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response := httptest.NewRecorder()
			request := authorizedRequest(http.MethodPost, "/v1/issues", createIssueRequest{Title: "Concurrent idempotent issue"})
			request.Header.Set(idempotencyHeader, "concurrent-create")
			fixture.Server.ServeHTTP(response, request)
			if response.Code != http.StatusCreated {
				errors <- response.Body.String()
				return
			}
			var body issueResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				errors <- err.Error()
				return
			}
			results <- body
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)

	for err := range errors {
		t.Fatalf("idempotent request failed: %s", err)
	}

	issueIDs := map[string]bool{}
	for result := range results {
		issueIDs[result.Issue.ID] = true
	}
	if len(issueIDs) != 1 {
		t.Fatalf("idempotent issue IDs = %+v, want exactly one", issueIDs)
	}

	issues, err := fixture.Issues.ListIssues(context.Background(), coordinator.IssueFilter{})
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issue count = %d, want 1", len(issues))
	}
}

func TestCredentialStoreInvalidCredentialDoesNotFallBackToConfiguredToken(t *testing.T) {
	fixture := newTestFixture(t)
	if _, err := fixture.GlobalDB.ExecContext(context.Background(), `
UPDATE tokens
SET revoked_at = ?
WHERE token_hash = ?`, time.Now().UTC().Format(time.RFC3339Nano), coordinator.HashToken("owner-token")); err != nil {
		t.Fatalf("revoke owner token: %v", err)
	}

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, "/v1/issues", nil)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked owner token status = %d, want 401", response.Code)
	}

	if _, err := fixture.GlobalDB.ExecContext(context.Background(), `
UPDATE tokens
SET revoked_at = ?
WHERE token_hash = ?`, time.Now().UTC().Format(time.RFC3339Nano), coordinator.HashToken("hook-token")); err != nil {
		t.Fatalf("revoke hook token: %v", err)
	}

	response = httptest.NewRecorder()
	request = authorizedRequest(http.MethodPost, fixture.gitEventsPath(), gitEventsRequest{
		OldSHA: "old",
		NewSHA: "new",
		Ref:    "refs/heads/issue/i-0001",
		Actor:  "hook",
	})
	request.Header.Set("Authorization", "Bearer hook-token")
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked hook token status = %d, want 401", response.Code)
	}
}

func TestWebUIBootstrapLoginAndCookieAuth(t *testing.T) {
	fixture := newTestFixture(t)

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/ui/bootstrap", map[string]string{}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodPost, "/v1/ui/bootstrap", map[string]string{}, http.StatusForbidden, nil)

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)
	if !strings.HasPrefix(bootstrap.LoginPath, "/ui/login?token=") || bootstrap.ExpiresAt.IsZero() {
		t.Fatalf("bootstrap = %+v", bootstrap)
	}

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil)
	fixture.Server.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}
	if login.Header().Get("Location") != "/ui/" {
		t.Fatalf("login location = %q, want /ui/", login.Header().Get("Location"))
	}
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			sessionCookie = cookie
		case webCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("login cookies = %+v", login.Result().Cookies())
	}
	if !sessionCookie.HttpOnly {
		t.Fatalf("session cookie is not HttpOnly: %+v", sessionCookie)
	}
	if csrfCookie.HttpOnly {
		t.Fatalf("csrf cookie should be readable by browser JavaScript: %+v", csrfCookie)
	}
	if sessionCookie.Path != "/ui" || csrfCookie.Path != "/ui" {
		t.Fatalf("cookie paths = session:%q csrf:%q, want /ui", sessionCookie.Path, csrfCookie.Path)
	}

	reusedLogin := httptest.NewRecorder()
	fixture.Server.ServeHTTP(reusedLogin, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if reusedLogin.Code != http.StatusUnauthorized {
		t.Fatalf("reused login status = %d, want 401", reusedLogin.Code)
	}

	directBoard := httptest.NewRecorder()
	directBoardRequest := httptest.NewRequest(http.MethodGet, "/v1/board", nil)
	directBoardRequest.Header.Set(webCSRFHeader, csrfCookie.Value)
	directBoardRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(directBoard, directBoardRequest)
	if directBoard.Code != http.StatusUnauthorized {
		t.Fatalf("direct cookie board status = %d, want 401; body: %s", directBoard.Code, directBoard.Body.String())
	}

	missingReadCSRF := httptest.NewRecorder()
	missingReadCSRFRequest := httptest.NewRequest(http.MethodGet, "/ui/api/v1/board", nil)
	missingReadCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(missingReadCSRF, missingReadCSRFRequest)
	if missingReadCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("missing read csrf status = %d, want 401; body: %s", missingReadCSRF.Code, missingReadCSRF.Body.String())
	}

	boardResponse := httptest.NewRecorder()
	boardRequest := httptest.NewRequest(http.MethodGet, "/ui/api/v1/board", nil)
	boardRequest.Header.Set(webCSRFHeader, csrfCookie.Value)
	boardRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(boardResponse, boardRequest)
	if boardResponse.Code != http.StatusOK {
		t.Fatalf("cookie board status = %d, want 200; body: %s", boardResponse.Code, boardResponse.Body.String())
	}

	missingCSRF := httptest.NewRecorder()
	missingCSRFRequest := httptest.NewRequest(http.MethodPost, "/ui/api/v1/issues", strings.NewReader(`{"title":"Missing csrf"}`))
	missingCSRFRequest.Header.Set("Content-Type", "application/json")
	missingCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(missingCSRF, missingCSRFRequest)
	if missingCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("missing csrf status = %d, want 401; body: %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	var created issueResponse
	withCSRF := httptest.NewRecorder()
	withCSRFRequest := httptest.NewRequest(http.MethodPost, "/ui/api/v1/issues", strings.NewReader(`{"title":"Browser issue"}`))
	withCSRFRequest.Header.Set("Content-Type", "application/json")
	withCSRFRequest.Header.Set(webCSRFHeader, csrfCookie.Value)
	withCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(withCSRF, withCSRFRequest)
	if withCSRF.Code != http.StatusCreated {
		t.Fatalf("with csrf status = %d, want 201; body: %s", withCSRF.Code, withCSRF.Body.String())
	}
	if err := json.NewDecoder(withCSRF.Body).Decode(&created); err != nil {
		t.Fatalf("decode created issue: %v", err)
	}
	if created.Issue.Title != "Browser issue" {
		t.Fatalf("created issue = %+v", created.Issue)
	}
}

func loginWebUI(t *testing.T, fixture testFixture) (*http.Cookie, *http.Cookie) {
	t.Helper()

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)
	login := httptest.NewRecorder()
	fixture.Server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			sessionCookie = cookie
		case webCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("login cookies = %+v", login.Result().Cookies())
	}
	return sessionCookie, csrfCookie
}

func TestWebUIRoutesAndAssets(t *testing.T) {
	fixture := newTestFixture(t)

	for _, path := range []string{"/ui/", "/ui/board", "/ui/merge", "/ui/projects/" + fixture.Project.ID + "/issues/i-0001", "/ui/changes/ch-0001", "/ui/sessions/s-0001/terminal", "/ui/workers", "/ui/jobs"} {
		response := httptest.NewRecorder()
		fixture.Server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body: %s", path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "<flow-app>") {
			t.Fatalf("%s did not serve app shell: %s", path, response.Body.String())
		}
	}

	asset := httptest.NewRecorder()
	fixture.Server.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/ui/assets/app.js?v=test-version", nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want 200; body: %s", asset.Code, asset.Body.String())
	}
	if asset.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("asset content type = %q", asset.Header().Get("Content-Type"))
	}
	if asset.Header().Get("Cache-Control") != "max-age=31536000, immutable" {
		t.Fatalf("asset cache control = %q", asset.Header().Get("Cache-Control"))
	}
	if !strings.Contains(asset.Body.String(), "customElements.define") {
		t.Fatalf("asset body missing app script")
	}

	// Unversioned asset requests — notably the browser's native ES module
	// imports (import "./markdown.js"), which carry no ?v= cache key — must
	// revalidate via ETag rather than be cached immutably, so an edited module
	// is never served stale. (The behavior keys on the ?v= query, not the file
	// name, so an unversioned app.js request exercises the same code path.)
	module := httptest.NewRecorder()
	fixture.Server.ServeHTTP(module, httptest.NewRequest(http.MethodGet, "/ui/assets/app.js", nil))
	if module.Code != http.StatusOK {
		t.Fatalf("unversioned asset status = %d, want 200", module.Code)
	}
	if got := module.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unversioned asset cache control = %q, want no-cache", got)
	}
	etag := module.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("unversioned asset missing ETag")
	}

	// A matching If-None-Match must yield 304 Not Modified with an empty body.
	revalidated := httptest.NewRecorder()
	conditional := httptest.NewRequest(http.MethodGet, "/ui/assets/app.js", nil)
	conditional.Header.Set("If-None-Match", etag)
	fixture.Server.ServeHTTP(revalidated, conditional)
	if revalidated.Code != http.StatusNotModified {
		t.Fatalf("conditional asset status = %d, want 304", revalidated.Code)
	}
	if revalidated.Body.Len() != 0 {
		t.Fatalf("304 response should have empty body, got %d bytes", revalidated.Body.Len())
	}
}

func TestWebUITerminalAttachCreatesOwnerBrowserURL(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Web terminal issue")
	if _, err := fixture.Sessions.RegisterTerminalTarget(context.Background(), started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)
	login := httptest.NewRecorder()
	fixture.Server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			sessionCookie = cookie
		case webCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("login cookies = %+v", login.Result().Cookies())
	}

	missingCSRF := httptest.NewRecorder()
	missingCSRFRequest := httptest.NewRequest(http.MethodPost, "/ui/api/v1/sessions/"+started.Session.ID+"/terminal-token", strings.NewReader(`{}`))
	missingCSRFRequest.Header.Set("Content-Type", "application/json")
	missingCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(missingCSRF, missingCSRFRequest)
	if missingCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("missing csrf status = %d, want 401; body: %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/ui/api/v1/sessions/"+started.Session.ID+"/terminal-token", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(webCSRFHeader, csrfCookie.Value)
	request.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("terminal-token status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var access sessionTerminalAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&access); err != nil {
		t.Fatalf("decode terminal access: %v", err)
	}
	if !strings.HasPrefix(access.Access.LoginPath, "/v1/sessions/"+started.Session.ID+"/terminal-login?token=") {
		t.Fatalf("terminal access = %+v", access.Access)
	}
}

func TestBoardIncludesUIIssueCardReadModels(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Card read model issue")
	if _, err := fixture.Sessions.UpdateSessionState(context.Background(), started.Session.ID, coordinator.SessionWaiting); err != nil {
		t.Fatalf("mark waiting: %v", err)
	}
	if _, err := fixture.Status.WriteSessionStatus(context.Background(), started.Session.ID, "Waiting on product decision", "author", coordinator.StatusKindNote); err != nil {
		t.Fatalf("write status: %v", err)
	}
	if _, err := fixture.DB.Exec(`
INSERT INTO handoff_snapshots (
	change_id,
	head_sha,
	present,
	valid,
	summary,
	content,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		started.Change.ID,
		"abc123",
		1,
		1,
		"Waiting for product decision before final polish.",
		"# Flow Handoff\n\n## Current Goal\n\nWaiting for product decision before final polish.\n\nPRIVATE HANDOFF DETAIL - do not expose\n",
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert handoff snapshot: %v", err)
	}
	required := true
	if _, err := fixture.Checks.ReportCheck(context.Background(), coordinator.ReportCheckInput{
		IssueID:  started.Session.IssueID,
		Name:     "unit",
		Required: &required,
		Verdict:  coordinator.CheckBlocked,
		Reporter: "ci",
	}); err != nil {
		t.Fatalf("report check: %v", err)
	}

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, fixture.boardPath(), nil)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("board status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "token_hash") || strings.Contains(response.Body.String(), "TokenHash") {
		t.Fatalf("board response leaked session token hash: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "PRIVATE HANDOFF DETAIL") || strings.Contains(response.Body.String(), `"content"`) {
		t.Fatalf("board response leaked handoff content: %s", response.Body.String())
	}
	var board boardResponse
	if err := json.NewDecoder(response.Body).Decode(&board); err != nil {
		t.Fatalf("decode board: %v", err)
	}

	card, ok := board.IssueCards[started.Session.IssueID]
	if !ok {
		t.Fatalf("issue cards = %+v, missing %s", board.IssueCards, started.Session.IssueID)
	}
	if card.ActiveSession == nil || card.ActiveSession.ID != started.Session.ID || card.ActiveSession.State != coordinator.SessionWaiting {
		t.Fatalf("active session summary = %+v", card.ActiveSession)
	}
	if card.Change == nil || card.Change.ID != started.Change.ID || card.Change.Branch != started.Change.Branch {
		t.Fatalf("change summary = %+v, want %s", card.Change, started.Change.ID)
	}
	if card.LatestStatus == nil || card.LatestStatus.Message != "Waiting on product decision" {
		t.Fatalf("latest status = %+v", card.LatestStatus)
	}
	if card.Handoff == nil || !card.Handoff.Present || !card.Handoff.Valid || card.Handoff.Summary != "Waiting for product decision before final polish." || card.Handoff.HeadSHA != "abc123" {
		t.Fatalf("handoff summary = %+v", card.Handoff)
	}
	if card.RequiredChecks.Total != 1 || card.RequiredChecks.Blocked != 1 {
		t.Fatalf("required check summary = %+v", card.RequiredChecks)
	}
	if card.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("review state = %q, want changes_requested", card.ReviewState)
	}
	if card.BlockingReason != "required check blocked" || card.PrimaryAction != "respond" {
		t.Fatalf("card actions = blocking:%q primary:%q", card.BlockingReason, card.PrimaryAction)
	}
	if card.TerminalAvailable {
		t.Fatal("terminal should not be available before a target is registered")
	}

	if _, err := fixture.Sessions.RegisterTerminalTarget(context.Background(), started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}
	response = httptest.NewRecorder()
	request = authorizedRequest(http.MethodGet, fixture.boardPath(), nil)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("board with terminal status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&board); err != nil {
		t.Fatalf("decode board with terminal: %v", err)
	}
	card = board.IssueCards[started.Session.IssueID]
	if !card.TerminalAvailable {
		t.Fatalf("terminal availability = false, card = %+v", card)
	}
	if card.ActiveSession == nil || !card.ActiveSession.TerminalAvailable {
		t.Fatalf("active session terminal availability = %+v", card.ActiveSession)
	}
}

func TestBoardHidesUIIssueCardsFromSessionTokens(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	source, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Session source"})
	if err != nil {
		t.Fatalf("create source issue: %v", err)
	}
	unrelated := startAuthorSessionForStatusTest(t, fixture, "Unrelated live issue")
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:         "session-token",
		Scope:         coordinator.TokenScopeSession,
		Subject:       "s-session",
		ProjectID:     &fixture.Project.ID,
		SourceIssueID: &source.ID,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, fixture.boardPath(), nil)
	request.Header.Set("Authorization", "Bearer session-token")
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("board status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "issue_cards") || strings.Contains(response.Body.String(), unrelated.Session.ID) || strings.Contains(response.Body.String(), unrelated.Session.WorkerID) {
		t.Fatalf("session board leaked card metadata: %s", response.Body.String())
	}
	var board boardResponse
	if err := json.NewDecoder(response.Body).Decode(&board); err != nil {
		t.Fatalf("decode session board: %v", err)
	}
	if board.LaneStates[unrelated.Session.IssueID] != coordinator.LaneStateInProgress {
		t.Fatalf("session board lane states = %+v, want in_progress for %s", board.LaneStates, unrelated.Session.IssueID)
	}
}

func TestBoardUIIssueCardsShowRelationBlockers(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	blocker, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Finish dependency"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Blocked work"})
	if err != nil {
		t.Fatalf("create blocked issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, blocked.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule blocked issue: %v", err)
	}
	if err := fixture.Issues.LinkIssues(ctx, blocker.ID, blocked.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.UpNext) != 0 {
		t.Fatalf("up_next lane = %+v, want empty for blocked issue", board.Board.UpNext)
	}
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != blocked.ID {
		t.Fatalf("needs_attention lane = %+v, want %s", board.Board.NeedsAttention, blocked.ID)
	}
	if len(board.BlockedIDs) != 1 || board.BlockedIDs[0] != blocked.ID {
		t.Fatalf("blocked ids = %+v, want [%s]", board.BlockedIDs, blocked.ID)
	}
	card, ok := board.IssueCards[blocked.ID]
	if !ok {
		t.Fatalf("issue cards = %+v, missing %s", board.IssueCards, blocked.ID)
	}
	if card.Blockers.Count != 1 || len(card.Blockers.Issues) != 1 || card.Blockers.Issues[0].ID != blocker.ID {
		t.Fatalf("blocker summary = %+v", card.Blockers)
	}
	if card.BlockingReason != "blocked by issue" || card.PrimaryAction != "unblock" {
		t.Fatalf("card actions = blocking:%q primary:%q", card.BlockingReason, card.PrimaryAction)
	}
}

func TestBoardUIIssueCardsIncludeTagsAndRelationSummary(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	parent, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Parent"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Child"})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	related, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Related"})
	if err != nil {
		t.Fatalf("create related: %v", err)
	}
	tag, err := fixture.Issues.CreateTag(ctx, coordinator.CreateTagInput{Slug: "triage-tag", Name: "Triage Tag"})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := fixture.Issues.TagIssue(ctx, child.ID, tag.ID, coordinator.ActorHuman); err != nil {
		t.Fatalf("tag child: %v", err)
	}
	if err := fixture.Issues.LinkIssues(ctx, parent.ID, child.ID, coordinator.RelationParentOf, coordinator.ActorHuman); err != nil {
		t.Fatalf("link parent: %v", err)
	}
	if err := fixture.Issues.LinkIssues(ctx, child.ID, related.ID, coordinator.RelationRelatedTo, coordinator.ActorHuman); err != nil {
		t.Fatalf("link related: %v", err)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	card, ok := board.IssueCards[child.ID]
	if !ok {
		t.Fatalf("issue cards = %+v, missing %s", board.IssueCards, child.ID)
	}
	if len(card.Tags) != 1 || card.Tags[0].Slug != "triage-tag" {
		t.Fatalf("card tags = %+v", card.Tags)
	}
	if card.Relations.Total != 2 || card.Relations.Parents != 1 || card.Relations.Related != 1 {
		t.Fatalf("card relation summary = %+v", card.Relations)
	}
}

func TestIssueDetailReadModelIsOwnerOnly(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Issue detail metadata")
	tag, err := fixture.Issues.CreateTag(ctx, coordinator.CreateTagInput{Slug: "web-ui", Name: "Web UI"})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := fixture.Issues.TagIssue(ctx, started.Session.IssueID, tag.ID, coordinator.ActorHuman); err != nil {
		t.Fatalf("tag issue: %v", err)
	}
	blocker, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Blocker"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := fixture.Issues.LinkIssues(ctx, blocker.ID, started.Session.IssueID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if _, err := fixture.Sessions.RegisterTerminalTarget(ctx, started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}
	required := true
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/unit", reportCheckRequest{
		Kind:     string(coordinator.CheckKindCI),
		Required: &required,
		Verdict:  string(coordinator.CheckPending),
	}, http.StatusOK, nil)

	var owner issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+started.Session.IssueID, nil, http.StatusOK, &owner)
	if owner.Detail == nil {
		t.Fatal("owner issue response missing detail")
	}
	if len(owner.Detail.Tags) != 1 || owner.Detail.Tags[0].Slug != "web-ui" {
		t.Fatalf("detail tags = %+v", owner.Detail.Tags)
	}
	if len(owner.Detail.Relations) != 1 || owner.Detail.Relations[0].SourceIssueID != blocker.ID {
		t.Fatalf("detail relations = %+v", owner.Detail.Relations)
	}
	if owner.Detail.ActiveSession == nil || owner.Detail.ActiveSession.ID != started.Session.ID {
		t.Fatalf("active session = %+v", owner.Detail.ActiveSession)
	}
	if !owner.Detail.TerminalAvailable || !owner.Detail.ActiveSession.TerminalAvailable {
		t.Fatalf("active terminal availability = detail:%t session:%+v", owner.Detail.TerminalAvailable, owner.Detail.ActiveSession)
	}
	if len(owner.Detail.Sessions) != 1 || len(owner.Detail.Changes) != 1 || owner.Detail.Changes[0].ID != started.Change.ID {
		t.Fatalf("sessions/changes = %+v / %+v", owner.Detail.Sessions, owner.Detail.Changes)
	}
	if !owner.Detail.Sessions[0].TerminalAvailable {
		t.Fatalf("session terminal availability = %+v", owner.Detail.Sessions[0])
	}
	if owner.Detail.RequiredChecks.Total != 1 || owner.Detail.RequiredChecks.Pending != 1 || len(owner.Detail.Checks) != 1 {
		t.Fatalf("checks = %+v summary=%+v", owner.Detail.Checks, owner.Detail.RequiredChecks)
	}

	var session issueResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodGet, "/v1/issues/"+started.Session.IssueID, nil, http.StatusOK, &session)
	if session.Detail != nil {
		t.Fatalf("session issue response leaked detail: %+v", session.Detail)
	}
}

func TestWorkerTokenCanReadIssue(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title:              "Reviewer prompt context",
		Body:               "Check jobs fetch issue context with the worker token.",
		AcceptanceCriteria: "Worker-scope issue reads succeed.",
	})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	var worker issueResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/issues/"+issue.ID, nil, http.StatusOK, &worker)
	if worker.Issue.ID != issue.ID || worker.Issue.Body != issue.Body || worker.Issue.AcceptanceCriteria != issue.AcceptanceCriteria {
		t.Fatalf("worker issue response = %+v", worker.Issue)
	}
	if worker.Detail != nil {
		t.Fatalf("worker issue response leaked owner detail: %+v", worker.Detail)
	}
}

func TestSessionTokenCanReadAndCreateConstrainedDiscoveredIssue(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	source, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Current work"})
	if err != nil {
		t.Fatalf("create source issue: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:         "session-token",
		Scope:         coordinator.TokenScopeSession,
		Subject:       "s-1",
		ProjectID:     &fixture.Project.ID,
		SourceIssueID: &source.ID,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	var list issuesResponse
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodGet, "/v1/issues", nil, http.StatusOK, &list)
	if len(list.Issues) != 1 || list.Issues[0].ID != source.ID {
		t.Fatalf("session list = %+v", list.Issues)
	}

	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v1/issues", createIssueRequest{
		Title:         "Bad session schedule",
		ScheduleState: string(coordinator.ScheduleUpNext),
	}, http.StatusForbidden, nil)

	createResponse := issueResponse{}
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v1/issues", createIssueRequest{
		Title: "Discovered blocker",
		Tags: []tagRequest{{
			Slug: "needs-investigation",
			Name: "Needs Investigation",
		}},
		Relations: []relationRequest{{
			TargetIssueID: source.ID,
			Kind:          string(coordinator.RelationBlocks),
		}},
	}, http.StatusCreated, &createResponse, idempotencyHeader, "discover-1")

	discovered := createResponse.Issue
	if discovered.CreatedBy != coordinator.ActorAgent {
		t.Fatalf("CreatedBy = %q, want agent", discovered.CreatedBy)
	}
	if discovered.CreatedBySessionID == nil || *discovered.CreatedBySessionID != "s-1" {
		t.Fatalf("CreatedBySessionID = %v, want s-1", discovered.CreatedBySessionID)
	}
	if discovered.SourceIssueID == nil || *discovered.SourceIssueID != source.ID {
		t.Fatalf("SourceIssueID = %v, want %s", discovered.SourceIssueID, source.ID)
	}
	if discovered.ScheduleState != coordinator.ScheduleBacklog || discovered.TriageState != coordinator.TriagePending {
		t.Fatalf("discovered state = %s/%s, want backlog/triage", discovered.ScheduleState, discovered.TriageState)
	}

	tags, err := fixture.Issues.TagsForIssue(ctx, discovered.ID)
	if err != nil {
		t.Fatalf("tags for discovered issue: %v", err)
	}
	if len(tags) != 1 || tags[0].Slug != "needs-investigation" || tags[0].CreatedBy != coordinator.ActorAgent {
		t.Fatalf("tags = %+v", tags)
	}
	relations, err := fixture.Issues.RelationsForIssue(ctx, source.ID)
	if err != nil {
		t.Fatalf("relations for source issue: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceIssueID != discovered.ID || relations[0].TargetIssueID != source.ID || relations[0].Kind != coordinator.RelationBlocks {
		t.Fatalf("relations = %+v", relations)
	}

	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v1/issues/"+source.ID+"/close", map[string]string{}, http.StatusForbidden, nil)
}

func TestHookTokenCanPostGitEvents(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:         "session-token",
		Scope:         coordinator.TokenScopeSession,
		Subject:       "s-1",
		ProjectID:     &fixture.Project.ID,
		SourceIssueID: nil,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	event := gitEventsRequest{
		OldSHA: "old",
		NewSHA: "new",
		Ref:    "refs/heads/issue/i-0001",
		Actor:  "owner",
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusForbidden, nil)

	var response gitEventsResponse
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusAccepted, &response)
	if response.Recorded != 1 || response.Inserted != 1 {
		t.Fatalf("git event response = %+v", response)
	}

	events, err := fixture.GitEvents.List(ctx)
	if err != nil {
		t.Fatalf("list git events: %v", err)
	}
	if len(events) != 1 || events[0].Ref != "refs/heads/issue/i-0001" || events[0].Source != coordinator.GitEventSourceAPI {
		t.Fatalf("events = %+v", events)
	}
}

func TestDrainGitEventSpoolRecoversMissedPostReceive(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := t.TempDir()
	repointFixtureExchange(t, fixture, exchangePath)

	if err := flowgit.HandlePostReceive(context.Background(), flowgit.HookOptions{
		ExchangeRepoPath: exchangePath,
		Stdin:            bytes.NewBufferString("old new refs/heads/issue/i-0001\n"),
	}); err != nil {
		t.Fatalf("post receive spool: %v", err)
	}

	var response drainGitEventsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/git/events/drain", drainGitEventsRequest{
		ExchangeRepoPath: exchangePath,
	}, http.StatusOK, &response)
	if response.Drained != 1 {
		t.Fatalf("Drained = %d, want 1", response.Drained)
	}

	events, err := fixture.GitEvents.List(context.Background())
	if err != nil {
		t.Fatalf("list git events: %v", err)
	}
	if len(events) != 1 || events[0].Source != coordinator.GitEventSourceSpool {
		t.Fatalf("events = %+v", events)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/git/events/drain", drainGitEventsRequest{
		ExchangeRepoPath: exchangePath,
	}, http.StatusOK, &response)
	if response.Drained != 0 {
		t.Fatalf("second Drained = %d, want 0", response.Drained)
	}
}

func TestGitEventsDeduplicateDirectPostAndSpoolDrain(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := t.TempDir()
	repointFixtureExchange(t, fixture, exchangePath)

	event := gitEventsRequest{
		OldSHA: "old",
		NewSHA: "new",
		Ref:    "refs/heads/issue/i-0001",
		Actor:  "owner",
	}
	var postResponse gitEventsResponse
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusAccepted, &postResponse)
	if postResponse.Inserted != 1 {
		t.Fatalf("Inserted = %d, want 1", postResponse.Inserted)
	}

	t.Setenv("FLOW_GIT_PRINCIPAL", "owner")
	if err := flowgit.HandlePostReceive(context.Background(), flowgit.HookOptions{
		ExchangeRepoPath: exchangePath,
		Stdin:            bytes.NewBufferString("old new refs/heads/issue/i-0001\n"),
	}); err != nil {
		t.Fatalf("post receive spool: %v", err)
	}

	var drainResponse drainGitEventsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/git/events/drain", drainGitEventsRequest{
		ExchangeRepoPath: exchangePath,
	}, http.StatusOK, &drainResponse)
	if drainResponse.Drained != 0 {
		t.Fatalf("Drained = %d, want 0 for duplicate event", drainResponse.Drained)
	}
	events, err := fixture.GitEvents.List(context.Background())
	if err != nil {
		t.Fatalf("list git events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
}

func TestWorkerHTTPLifecycleAndJobDiagnostics(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Worker API issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/jobs", enqueueJobRequest{
		IssueID:        &issue.ID,
		Role:           string(flowworker.RoleAuthor),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
	}, http.StatusForbidden, nil)

	var enqueue jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs", enqueueJobRequest{
		IssueID:        &issue.ID,
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
		Priority:       5,
		Payload:        map[string]any{"entrypoint": "make test"},
	}, http.StatusCreated, &enqueue)
	if enqueue.Job.State != flowworker.JobQueued || enqueue.Job.IssueID == nil || *enqueue.Job.IssueID != issue.ID {
		t.Fatalf("enqueued job = %+v", enqueue.Job)
	}

	var list jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs", nil, http.StatusOK, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != enqueue.Job.ID {
		t.Fatalf("jobs list = %+v", list.Jobs)
	}

	var reapJobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/workers/reap-jobs", nil, http.StatusOK, &reapJobs)
	if len(reapJobs.Jobs) != 1 || reapJobs.Jobs[0].ID != enqueue.Job.ID || reapJobs.Jobs[0].State != flowworker.JobQueued {
		t.Fatalf("worker reap jobs = %+v", reapJobs.Jobs)
	}
	if reapJobs.Jobs[0].Payload != nil {
		t.Fatalf("worker reap job payload = %+v, want omitted", reapJobs.Jobs[0].Payload)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/workers/reap-jobs", nil, http.StatusForbidden, nil)

	var show jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs/"+enqueue.Job.ID, nil, http.StatusOK, &show)
	if show.Job.ID != enqueue.Job.ID {
		t.Fatalf("show job = %+v, want %s", show.Job, enqueue.Job.ID)
	}

	var registered workerResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, &registered)
	if registered.Worker.ID != "w-local" || registered.Worker.LastHeartbeatAt == nil || registered.Worker.ExpiresAt == nil {
		t.Fatalf("registered worker = %+v", registered.Worker)
	}

	var heartbeat workerResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/heartbeat", heartbeatWorkerRequest{
		WorkerID:            "w-local",
		HeartbeatTTLSeconds: 120,
	}, http.StatusOK, &heartbeat)
	if heartbeat.Worker.ID != "w-local" || heartbeat.Worker.ExpiresAt == nil {
		t.Fatalf("heartbeat worker = %+v", heartbeat.Worker)
	}

	var workers workersResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/workers", nil, http.StatusOK, &workers)
	if len(workers.Workers) != 1 || workers.Workers[0].ID != "w-local" {
		t.Fatalf("workers list = %+v", workers.Workers)
	}
	if workers.Queue.Queued != 1 || workers.Queue.PersistentAgent != 1 || workers.Queue.CI != 1 {
		t.Fatalf("worker queue summary = %+v", workers.Queue)
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/workers", nil, http.StatusForbidden, nil)

	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
	if claim.Job.ID != enqueue.Job.ID || claim.Job.State != flowworker.JobClaimed {
		t.Fatalf("claimed job = %+v, want %s claimed", claim.Job, enqueue.Job.ID)
	}
	if claim.Lease.WorkerID != "w-local" {
		t.Fatalf("lease worker = %q, want w-local", claim.Lease.WorkerID)
	}

	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Job.ID != enqueue.Job.ID || running.Job.State != flowworker.JobRunning {
		t.Fatalf("running job = %+v, want %s running", running.Job, enqueue.Job.ID)
	}

	var runningJobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs", nil, http.StatusOK, &runningJobs)
	runningDiagnostics, ok := runningJobs.Diagnostics[enqueue.Job.ID]
	if !ok || runningDiagnostics.Lease == nil || runningDiagnostics.Lease.ID != claim.Lease.ID || !runningDiagnostics.LiveLease || runningDiagnostics.LeaseStatus != "live" || runningDiagnostics.TmuxSession == "" {
		t.Fatalf("running job diagnostics = %+v ok=%t", runningDiagnostics, ok)
	}
	var liveWorkers workersResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/workers", nil, http.StatusOK, &liveWorkers)
	liveWorkerDiagnostics := liveWorkers.Diagnostics["w-local"]
	if liveWorkers.Queue.Queued != 0 || liveWorkerDiagnostics.LiveJobs != 1 || liveWorkerDiagnostics.LivePersistentAgent != 1 {
		t.Fatalf("live worker diagnostics = queue:%+v worker:%+v", liveWorkers.Queue, liveWorkerDiagnostics)
	}

	var renew leaseResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/renew", renewLeaseRequest{
		LeaseID:              claim.Lease.ID,
		LeaseDurationSeconds: 120,
	}, http.StatusOK, &renew)
	if renew.Lease.RenewalCount != 1 {
		t.Fatalf("renewal count = %d, want 1", renew.Lease.RenewalCount)
	}

	var release jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/release", releaseLeaseRequest{
		LeaseID:    claim.Lease.ID,
		FinalState: string(flowworker.JobFinished),
	}, http.StatusOK, &release)
	if release.Job.State != flowworker.JobFinished {
		t.Fatalf("released job state = %q, want finished", release.Job.State)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs/"+enqueue.Job.ID, nil, http.StatusOK, &show)
	if show.Job.State != flowworker.JobFinished {
		t.Fatalf("show released job state = %q, want finished", show.Job.State)
	}
	if show.Diagnostics == nil || show.Diagnostics.Lease == nil || show.Diagnostics.LiveLease || show.Diagnostics.LeaseStatus != "released" {
		t.Fatalf("released job diagnostics = %+v", show.Diagnostics)
	}
}

func TestWorkerJoinTokenCreatesAndRotatesWorkerCredential(t *testing.T) {
	fixture := newTestFixture(t)
	server, err := NewServer(ServerOptions{
		Registry:        fixture.Registry,
		OwnerToken:      "owner-token",
		HookToken:       "hook-token",
		WorkerJoinToken: "join-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	doJSONRequestAs(t, server, "join-token", http.MethodPost, "/v1/workers/join", joinWorkerRequest{
		WorkerID: "w-local",
	}, http.StatusOK, nil)
	if _, err := fixture.Credentials.Authenticate(context.Background(), "worker-token"); err != coordinator.ErrInvalidCredential {
		t.Fatalf("old worker-token authenticate err = %v, want ErrInvalidCredential", err)
	}

	var joined joinWorkerResponse
	doJSONRequestAs(t, server, "join-token", http.MethodPost, "/v1/workers/join", joinWorkerRequest{
		WorkerID: "w-remote",
	}, http.StatusOK, &joined)
	if joined.WorkerID != "w-remote" || strings.TrimSpace(joined.Token) == "" {
		t.Fatalf("joined = %+v, want worker token for w-remote", joined)
	}
	principal, err := fixture.Credentials.Authenticate(context.Background(), joined.Token)
	if err != nil {
		t.Fatalf("authenticate joined token: %v", err)
	}
	if principal.Scope != coordinator.TokenScopeWorker || principal.Subject != "w-remote" {
		t.Fatalf("principal = %+v, want worker w-remote", principal)
	}
}

func TestWorkerJoinDisabledWithoutJoinToken(t *testing.T) {
	fixture := newTestFixture(t)

	doJSONRequestAs(t, fixture.Server, "join-token", http.MethodPost, "/v1/workers/join", joinWorkerRequest{
		WorkerID: "w-local",
	}, http.StatusNotFound, nil)
}

func TestWorkerJoinRejectsInvalidJoinToken(t *testing.T) {
	fixture := newTestFixture(t)
	server, err := NewServer(ServerOptions{
		Registry:        fixture.Registry,
		OwnerToken:      "owner-token",
		HookToken:       "hook-token",
		WorkerJoinToken: "join-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	doJSONRequestAs(t, server, "wrong-token", http.MethodPost, "/v1/workers/join", joinWorkerRequest{
		WorkerID: "w-local",
	}, http.StatusUnauthorized, nil)
}

func TestConsoleAPILifecycleAndScope(t *testing.T) {
	fixture := newTestFixture(t)

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/console", consoleRequest{
		Harness: "codex",
	}, http.StatusCreated, &startedConsole)
	if !startedConsole.Active || startedConsole.Job == nil || startedConsole.Job.Role != flowworker.RoleConsole {
		t.Fatalf("started console response = %+v", startedConsole)
	}
	if startedConsole.Job.IssueID != nil || startedConsole.Job.ChangeID != nil {
		t.Fatalf("console job issue/change = %v/%v, want nil", startedConsole.Job.IssueID, startedConsole.Job.ChangeID)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Role != flowworker.RoleConsole || running.Session.IssueID != "" || running.Session.ChangeID != "" || running.SessionToken == "" {
		t.Fatalf("running console = %+v", running)
	}
	consoleToken := running.SessionToken
	sessionID := running.Session.ID

	var current consoleResponse
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v1/console", nil, http.StatusOK, &current)
	if !current.Active || current.Session == nil || current.Session.ID != sessionID {
		t.Fatalf("current console = %+v", current)
	}
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/sessions/"+sessionID+"/terminal", sessionTerminalRequest{
		TargetURL: "http://127.0.0.1:65535",
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/sessions/"+sessionID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/sessions/"+sessionID+"/status", sessionStatusRequest{
		Message: "unsupported",
	}, http.StatusBadRequest, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/sessions/"+sessionID+"/ready", readySessionRequest{}, http.StatusBadRequest, nil)

	var created issueResponse
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/issues", createIssueRequest{
		Title:         "Console-created issue",
		ScheduleState: string(coordinator.ScheduleBacklog),
		TriageState:   string(coordinator.TriageAccepted),
	}, http.StatusCreated, &created)
	if created.Issue.CreatedBy != coordinator.ActorAgent || created.Issue.CreatedBySessionID == nil || *created.Issue.CreatedBySessionID != sessionID {
		t.Fatalf("console-created issue audit = %+v", created.Issue)
	}
	title := "Console-edited issue"
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPatch, "/v1/issues/"+created.Issue.ID, editIssueRequest{
		Title: &title,
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/issues/"+created.Issue.ID+"/schedule", scheduleIssueRequest{
		State: string(coordinator.ScheduleUpNext),
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/issues/"+created.Issue.ID+"/checks/unit", reportCheckRequest{
		Kind:    string(coordinator.CheckKindCI),
		Verdict: string(coordinator.CheckSatisfied),
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/issues/"+created.Issue.ID+"/merge", map[string]string{}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v1/jobs", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v1/workers", nil, http.StatusForbidden, nil)

	var blocker issueResponse
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/issues", createIssueRequest{
		Title:         "Console blocker",
		ScheduleState: string(coordinator.ScheduleBacklog),
		TriageState:   string(coordinator.TriageAccepted),
	}, http.StatusCreated, &blocker)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v1/issues/"+blocker.Issue.ID+"/relations", relationRequest{
		TargetIssueID: created.Issue.ID,
		Kind:          string(coordinator.RelationBlocks),
	}, http.StatusNoContent, nil)
	relations, err := fixture.Issues.RelationsForIssue(context.Background(), created.Issue.ID)
	if err != nil {
		t.Fatalf("relations for console issue: %v", err)
	}
	if len(relations) != 1 || relations[0].CreatedBy != coordinator.ActorAgent {
		t.Fatalf("relations = %+v, want one agent-created relation", relations)
	}
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodDelete, "/v1/issues/"+blocker.Issue.ID+"/relations", relationRequest{
		TargetIssueID: created.Issue.ID,
		Kind:          string(coordinator.RelationBlocks),
	}, http.StatusNoContent, nil)

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/release", releaseLeaseRequest{
		LeaseID:    claim.Lease.ID,
		FinalState: string(flowworker.JobFinished),
	}, http.StatusBadRequest, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodDelete, "/v1/console", nil, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v1/console", nil, http.StatusUnauthorized, nil)
}

func TestIssueConsoleAPILifecycleAndScope(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Issue recovery console"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	other, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Other issue"})
	if err != nil {
		t.Fatalf("create other issue: %v", err)
	}

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/console", consoleRequest{
		Harness: flowharness.Shell,
	}, http.StatusCreated, &startedConsole)
	if !startedConsole.Active || startedConsole.Job == nil || startedConsole.Job.Role != flowworker.RoleConsole {
		t.Fatalf("started issue console response = %+v", startedConsole)
	}
	if startedConsole.Job.IssueID == nil || *startedConsole.Job.IssueID != issue.ID || startedConsole.Job.ChangeID == nil {
		t.Fatalf("issue console job issue/change = %v/%v, want %s/change", startedConsole.Job.IssueID, startedConsole.Job.ChangeID, issue.ID)
	}
	if got := payloadString(startedConsole.Job.Payload, "console_scope"); got != "issue_recovery" {
		t.Fatalf("console_scope = %q, want issue_recovery", got)
	}
	if got := payloadString(startedConsole.Job.Payload, "session_purpose"); got != "issue_console" {
		t.Fatalf("session_purpose = %q, want issue_console", got)
	}
	if got := payloadString(startedConsole.Job.Payload, "branch"); got != "issue/"+issue.ID {
		t.Fatalf("issue console branch = %q, want issue/%s", got, issue.ID)
	}

	var projectConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/console", nil, http.StatusOK, &projectConsole)
	if projectConsole.Active {
		t.Fatalf("project console should ignore issue console state: %+v", projectConsole)
	}
	var currentIssueConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+issue.ID+"/console", nil, http.StatusOK, &currentIssueConsole)
	if !currentIssueConsole.Active || currentIssueConsole.Job == nil || currentIssueConsole.Job.ID != startedConsole.Job.ID {
		t.Fatalf("current issue console = %+v, want job %s", currentIssueConsole, startedConsole.Job.ID)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/register", registerWorkerRequest{
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim issue console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Role != flowworker.RoleConsole || running.Session.IssueID != issue.ID || running.Session.ChangeID != *startedConsole.Job.ChangeID || running.SessionToken == "" {
		t.Fatalf("running issue console = %+v", running)
	}
	principal, err := fixture.Credentials.Authenticate(ctx, running.SessionToken)
	if err != nil {
		t.Fatalf("authenticate issue console token: %v", err)
	}
	if principal.Scope != coordinator.TokenScopeConsole || principal.SourceIssueID == nil || *principal.SourceIssueID != issue.ID {
		t.Fatalf("issue console principal = %+v", principal)
	}
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodGet, "/v1/issues/"+issue.ID, nil, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodGet, "/v1/issues/"+other.ID, nil, http.StatusForbidden, nil)

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodDelete, "/v1/issues/"+issue.ID+"/console", nil, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodGet, "/v1/issues/"+issue.ID, nil, http.StatusUnauthorized, nil)
}

func TestConsoleAPIStartsShellHarness(t *testing.T) {
	fixture := newTestFixture(t)

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/console", consoleRequest{
		Harness: flowharness.Shell,
	}, http.StatusCreated, &startedConsole)
	if startedConsole.Job == nil {
		t.Fatalf("started console response = %+v", startedConsole)
	}
	if got := payloadString(startedConsole.Job.Payload, "console_harness"); got != flowharness.Shell {
		t.Fatalf("console_harness = %q, want %q", got, flowharness.Shell)
	}
	entrypoint, ok := startedConsole.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("console entrypoint payload = %#v", startedConsole.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 || argv[0] != `exec "${SHELL:-/bin/sh}"` {
		t.Fatalf("console entrypoint argv = %#v", entrypoint["argv"])
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim shell console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Harness != flowharness.Shell {
		t.Fatalf("running shell console session = %+v", running.Session)
	}
}

func TestConsoleTokenIsProjectConfined(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	projectA := bundles[0].Project
	projectB := bundles[1].Project
	if err := server.credentials.EnsureToken(context.Background(), coordinator.CredentialInput{
		Token:     "console-token",
		Scope:     coordinator.TokenScopeConsole,
		Subject:   "s-console",
		ProjectID: &projectA.ID,
	}); err != nil {
		t.Fatalf("store console token: %v", err)
	}

	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v1/projects/"+projectA.ID, nil, http.StatusOK, nil)
	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v1/projects/"+projectB.ID, nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v1/projects/"+projectB.ID+"/board", nil, http.StatusForbidden, nil)
}

func TestDiagnosticsDistinguishExpiredUnreleasedLeases(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Expired lease diagnostics"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	var enqueue jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs", enqueueJobRequest{
		IssueID:        &issue.ID,
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
	}, http.StatusCreated, &enqueue)
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claim.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	var jobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs", nil, http.StatusOK, &jobs)
	diagnostic := jobs.Diagnostics[enqueue.Job.ID]
	if diagnostic.Lease == nil || diagnostic.Lease.ReleasedAt != nil || diagnostic.LiveLease || diagnostic.LeaseStatus != "expired" {
		t.Fatalf("expired job diagnostics = %+v", diagnostic)
	}

	var workers workersResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/workers", nil, http.StatusOK, &workers)
	workerDiagnostic := workers.Diagnostics["w-local"]
	if workerDiagnostic.LiveJobs != 0 || workerDiagnostic.ExpiredUnreleasedJobs != 1 || workerDiagnostic.ExpiredUnreleasedPersistentAgent != 1 {
		t.Fatalf("expired worker diagnostics = %+v", workerDiagnostic)
	}
}

// TestJobsListAggregateOrdersByUpdatedAndStampsProject verifies the aggregate
// /v1/jobs response carries each job's project name in its diagnostics and is
// ordered globally by updated_at descending across projects (rather than
// concatenating per-project lists in registry order).
func TestJobsListAggregateOrdersByUpdatedAndStampsProject(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	projectA, projectB := bundles[0].Project, bundles[1].Project
	ctx := context.Background()

	// Enqueue one CI job in each project.
	var jobA jobResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodPost, "/v1/jobs?project="+projectA.ID, enqueueJobRequest{
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketEphemeral),
	}, http.StatusCreated, &jobA)
	var jobB jobResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodPost, "/v1/jobs?project="+projectB.ID, enqueueJobRequest{
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketEphemeral),
	}, http.StatusCreated, &jobB)

	// Force distinct updated_at timestamps: project B's job is the most recently
	// updated even though project A was registered first, so the global sort must
	// place it ahead of project A's job.
	now := time.Now().UTC()
	if _, err := bundles[0].Store.DB().ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, now.Add(-2*time.Hour).Format(time.RFC3339Nano), jobA.Job.ID); err != nil {
		t.Fatalf("stamp jobA updated_at: %v", err)
	}
	if _, err := bundles[1].Store.DB().ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, now.Add(-time.Hour).Format(time.RFC3339Nano), jobB.Job.ID); err != nil {
		t.Fatalf("stamp jobB updated_at: %v", err)
	}

	var list jobsResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v1/jobs", nil, http.StatusOK, &list)
	if len(list.Jobs) != 2 {
		t.Fatalf("jobs list = %+v, want 2 jobs", list.Jobs)
	}
	// Global updated_at desc: project B's job (newer) first, then project A's.
	if list.Jobs[0].ID != jobB.Job.ID || list.Jobs[1].ID != jobA.Job.ID {
		t.Fatalf("jobs order = %s, %s; want %s, %s (updated_at desc)", list.Jobs[0].ID, list.Jobs[1].ID, jobB.Job.ID, jobA.Job.ID)
	}

	// Each job's diagnostics carry its owning project's name and id.
	diagA := list.Diagnostics[jobA.Job.ID]
	if diagA.ProjectName != projectA.Name || diagA.ProjectID != projectA.ID {
		t.Fatalf("jobA project diagnostics = %+v, want project %s (%s)", diagA, projectA.Name, projectA.ID)
	}
	diagB := list.Diagnostics[jobB.Job.ID]
	if diagB.ProjectName != projectB.Name || diagB.ProjectID != projectB.ID {
		t.Fatalf("jobB project diagnostics = %+v, want project %s (%s)", diagB, projectB.Name, projectB.ID)
	}
}

func TestSidebarStatusSummarizesLiveBoardWorkersAndJobs(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if _, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title:       "Pending triage",
		TriageState: coordinator.TriagePending,
	}); err != nil {
		t.Fatalf("create triage issue: %v", err)
	}

	feedback := startAuthorSessionForStatusTestWithWorker(t, fixture, "Waiting for feedback", "w-feedback")
	if _, err := fixture.Sessions.UpdateSessionState(ctx, feedback.Session.ID, coordinator.SessionWaiting); err != nil {
		t.Fatalf("mark feedback session waiting: %v", err)
	}

	merge := startAuthorSessionForStatusTestWithWorker(t, fixture, "Ready for merge", "w-merge")
	if _, err := fixture.Sessions.ReadyAuthorSession(ctx, merge.Session.ID); err != nil {
		t.Fatalf("ready merge session: %v", err)
	}
	required := true
	if _, err := fixture.Checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:  merge.Session.IssueID,
		Name:     "reviewer",
		Kind:     coordinator.CheckKindReviewer,
		Required: &required,
		Verdict:  coordinator.CheckSatisfied,
		Reporter: "test",
	}); err != nil {
		t.Fatalf("satisfy reviewer check: %v", err)
	}
	if _, err := fixture.Checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		IssueID:  merge.Session.IssueID,
		Name:     "verifier",
		Kind:     coordinator.CheckKindVerifier,
		Required: &required,
		Verdict:  coordinator.CheckSatisfied,
		Reporter: "test",
	}); err != nil {
		t.Fatalf("satisfy verifier check: %v", err)
	}

	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-extra",
		CapacityEphemeral: 3,
	}); err != nil {
		t.Fatalf("register extra worker: %v", err)
	}
	activeJob, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue active job: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, "w-extra", activeJob.ID, []flowworker.CapacityBucket{flowworker.BucketEphemeral})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark active job running: %v", err)
	}
	if _, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	}); err != nil {
		t.Fatalf("enqueue queued job: %v", err)
	}

	var sidebar sidebarResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/sidebar", nil, http.StatusOK, &sidebar)
	if sidebar.Triage != 1 || sidebar.Feedback != 2 || sidebar.Merge != 1 {
		t.Fatalf("sidebar issue counts = triage:%d attention:%d merge:%d, want 1/2/1", sidebar.Triage, sidebar.Feedback, sidebar.Merge)
	}
	if sidebar.Workers.InUse != 2 || sidebar.Workers.Capacity != 5 {
		t.Fatalf("sidebar worker summary = %+v, want 2/5", sidebar.Workers)
	}
	if sidebar.Jobs.Active != 2 || sidebar.Jobs.Queued != 1 {
		t.Fatalf("sidebar job summary = %+v, want active 2 queued 1", sidebar.Jobs)
	}
}

func TestScheduleStartsAuthorJobAndSessionReadyReleasesSlot(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Author API issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	var scheduled issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/schedule", scheduleIssueRequest{
		State: string(coordinator.ScheduleUpNext),
	}, http.StatusOK, &scheduled)
	if scheduled.Issue.ScheduleState != coordinator.ScheduleUpNext {
		t.Fatalf("scheduled issue = %+v", scheduled.Issue)
	}
	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Role != flowworker.RoleAuthor || jobs[0].ChangeID == nil {
		t.Fatalf("jobs after schedule = %+v", jobs)
	}

	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Lease == nil || claim.Job.ID != jobs[0].ID {
		t.Fatalf("claim response = %+v, want author job %s", claim, jobs[0].ID)
	}

	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Change == nil || running.SessionToken == "" {
		t.Fatalf("running response missing session metadata: %+v", running)
	}
	if running.Session.IssueID != issue.ID || running.Session.ChangeID != *jobs[0].ChangeID {
		t.Fatalf("running session = %+v", running.Session)
	}
	if _, err := fixture.Credentials.Authenticate(ctx, running.SessionToken); err != nil {
		t.Fatalf("authenticate session token: %v", err)
	}
	if _, err := fixture.Sessions.RegisterTerminalTarget(ctx, running.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}

	var event sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, &event)
	if event.Session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("event session = %+v", event.Session)
	}
	var authorJobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs", nil, http.StatusOK, &authorJobs)
	authorDiagnostics, ok := authorJobs.Diagnostics[jobs[0].ID]
	if !ok || authorDiagnostics.Session == nil || authorDiagnostics.Session.State != coordinator.SessionWaiting || authorDiagnostics.Change == nil || authorDiagnostics.Change.ID != *jobs[0].ChangeID {
		t.Fatalf("author job diagnostics = %+v ok=%t", authorDiagnostics, ok)
	}
	if !authorDiagnostics.Session.TerminalAvailable {
		t.Fatalf("author job session terminal availability = %+v", authorDiagnostics.Session)
	}
	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != issue.ID {
		t.Fatalf("needs_attention board = %+v", board.Board.NeedsAttention)
	}
	if board.LaneStates[issue.ID] != coordinator.LaneStateInProgress || board.WaitReasons[issue.ID] != coordinator.WaitReasonQuestion {
		t.Fatalf("lane state/wait reason = %q/%q, want in_progress/question", board.LaneStates[issue.ID], board.WaitReasons[issue.ID])
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/release", releaseLeaseRequest{
		LeaseID:    claim.Lease.ID,
		FinalState: string(flowworker.JobFailed),
	}, http.StatusBadRequest, nil)
	stillRunning, err := fixture.Workers.GetJob(ctx, jobs[0].ID)
	if err != nil {
		t.Fatalf("get still-running job: %v", err)
	}
	if stillRunning.State != flowworker.JobRunning {
		t.Fatalf("author job state after release attempt = %q, want running", stillRunning.State)
	}
	stillLeased, err := fixture.Workers.GetLease(ctx, claim.Lease.ID)
	if err != nil {
		t.Fatalf("get still-live lease: %v", err)
	}
	if stillLeased.ReleasedAt != nil {
		t.Fatalf("author lease released by worker release attempt: %+v", stillLeased)
	}

	var ready sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/ready", map[string]string{}, http.StatusOK, &ready)
	if ready.Session.RuntimeState != coordinator.SessionFinished {
		t.Fatalf("ready session = %+v", ready.Session)
	}
	if _, err := fixture.Credentials.Authenticate(ctx, running.SessionToken); !errors.Is(err, coordinator.ErrInvalidCredential) {
		t.Fatalf("revoked session token err = %v, want ErrInvalidCredential", err)
	}
	releasedJob, err := fixture.Workers.GetJob(ctx, jobs[0].ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if releasedJob.State != flowworker.JobFinished {
		t.Fatalf("released job state = %q, want finished", releasedJob.State)
	}
	releasedLease, err := fixture.Workers.GetLease(ctx, claim.Lease.ID)
	if err != nil {
		t.Fatalf("get released lease: %v", err)
	}
	if releasedLease.ReleasedAt == nil {
		t.Fatal("released lease ReleasedAt is nil")
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.InProgress) != 1 || board.Board.InProgress[0].ID != issue.ID {
		t.Fatalf("in_progress board = %+v", board.Board.InProgress)
	}
	if board.LaneStates[issue.ID] != coordinator.LaneStateInReview {
		t.Fatalf("lane state = %q, want in_review", board.LaneStates[issue.ID])
	}
}

// startRunningAuthorSession schedules an issue up_next, claims its author job,
// marks it running, and returns the running session metadata. It is the shared
// preamble for the session-event regression tests below.
func startRunningAuthorSession(t *testing.T, fixture testFixture, issueID string) jobResponse {
	t.Helper()
	ctx := context.Background()

	var scheduled issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issueID+"/schedule", scheduleIssueRequest{
		State: string(coordinator.ScheduleUpNext),
	}, http.StatusOK, &scheduled)
	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Role != flowworker.RoleAuthor {
		t.Fatalf("jobs after schedule = %+v", jobs)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.SessionToken == "" {
		t.Fatalf("running response missing session metadata: %+v", running)
	}
	return running
}

func countTransitions(t *testing.T, fixture testFixture, issueID, eventKind string) int {
	t.Helper()
	var n int
	if err := fixture.DB.QueryRow(`
SELECT COUNT(*)
FROM transitions
WHERE issue_id = ?
	AND event_kind = ?`, issueID, eventKind).Scan(&n); err != nil {
		t.Fatalf("count %s transitions: %v", eventKind, err)
	}
	return n
}

// TestSessionEventRoutesThroughEngine is the regression for routing the
// working->waiting flip through the lifecycle engine: it must log a
// session_state_changed transition while keeping the issue in authoring; the
// human wait is a board/status overlay, not a workflow phase. A repeated
// same-state report must be a no-op (no second transition row).
func TestSessionEventRoutesThroughEngine(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Session event issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)

	var event sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, &event)
	if event.Session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("event session = %+v, want waiting", event.Session)
	}

	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 1 {
		t.Fatalf("session_state_changed transitions = %d, want 1", got)
	}
	var toPhase string
	if err := fixture.DB.QueryRow(`
SELECT to_phase
FROM transitions
WHERE issue_id = ?
	AND event_kind = ?`, issue.ID, string(lifecycle.EventSessionStateChanged)).Scan(&toPhase); err != nil {
		t.Fatalf("read session_state_changed transition: %v", err)
	}
	if toPhase != string(coordinator.PhaseAuthoring) {
		t.Fatalf("session_state_changed to_phase = %q, want authoring", toPhase)
	}

	// Re-posting the same state is the watchdog's per-poll re-report: the no-op
	// fast path returns the session unchanged without a new transition row.
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, &event)
	if event.Session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("re-post session = %+v, want waiting", event.Session)
	}
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 1 {
		t.Fatalf("session_state_changed transitions after re-post = %d, want 1 (no-op fast path)", got)
	}
}

func TestSessionSignalActivityTouchesAgentActivityOnly(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Session signal activity issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)
	backdated := time.Now().UTC().Add(-time.Hour)
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE sessions SET last_agent_activity_at = ? WHERE id = ?`, backdated.Format(time.RFC3339Nano), running.Session.ID); err != nil {
		t.Fatalf("backdate agent activity: %v", err)
	}

	var response sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/signal", sessionSignalRequest{
		Signal: string(coordinator.SessionSignalActivity),
		Source: coordinator.SessionEventSourceNativeHook,
	}, http.StatusOK, &response)
	if response.Session.RuntimeState != running.Session.RuntimeState {
		t.Fatalf("activity signal state = %q, want unchanged %q", response.Session.RuntimeState, running.Session.RuntimeState)
	}
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 0 {
		t.Fatalf("session_state_changed transitions after activity = %d, want 0", got)
	}
	updated, err := fixture.Sessions.GetSession(ctx, running.Session.ID)
	if err != nil {
		t.Fatalf("get updated session: %v", err)
	}
	if updated.LastAgentActivityAt == nil {
		t.Fatalf("LastAgentActivityAt after activity signal = nil, want a timestamp")
	}
	if !updated.LastAgentActivityAt.After(backdated) {
		t.Fatalf("activity signal did not advance LastAgentActivityAt past %v: got %v", backdated, updated.LastAgentActivityAt)
	}
}

func TestSessionSignalNativeHookWorkingBypassesHumanWaitWatchdogLatch(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Native hook resume issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)

	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/status", sessionStatusRequest{
		Kind:    coordinator.StatusKindPlan,
		Message: "Plan: inspect, implement, test",
	}, http.StatusOK, nil)

	var response sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/signal", sessionSignalRequest{
		Signal:        string(coordinator.SessionSignalWorking),
		Source:        coordinator.SessionEventSourceNativeHook,
		Harness:       "codex",
		HookEventName: "UserPromptSubmit",
	}, http.StatusOK, &response)
	if response.Session.RuntimeState != coordinator.SessionWorking {
		t.Fatalf("native hook working state = %q, want working", response.Session.RuntimeState)
	}
	board, err := fixture.Issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after native hook working: %v", err)
	}
	if len(board.Board.InProgress) != 1 || board.Board.InProgress[0].ID != issue.ID || board.LaneStates[issue.ID] != coordinator.LaneStatePlanning {
		t.Fatalf("board after native hook working = %+v lane=%q, want planning", board.Board, board.LaneStates[issue.ID])
	}
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 2 {
		t.Fatalf("session_state_changed transitions after native hook working = %d, want 2", got)
	}
}

func TestSessionSignalNativeHookLoopIsSuppressed(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Native hook loop issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)

	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/status", sessionStatusRequest{
		Kind:    coordinator.StatusKindPlan,
		Message: "Plan: inspect, implement, test",
	}, http.StatusOK, nil)

	var response sessionResponse
	for i := 0; i < nativeHookStateLoopTransitionThreshold+4; i++ {
		doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/signal", sessionSignalRequest{
			Signal:        string(coordinator.SessionSignalWorking),
			Source:        coordinator.SessionEventSourceNativeHook,
			Harness:       "harness",
			HookEventName: "UserPromptSubmit",
		}, http.StatusOK, &response)
		doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/signal", sessionSignalRequest{
			Signal:        string(coordinator.SessionSignalWaiting),
			Source:        coordinator.SessionEventSourceNativeHook,
			Harness:       "harness",
			HookEventName: "Stop",
		}, http.StatusOK, &response)
	}
	if response.Session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("session state after native hook loop = %q, want waiting", response.Session.RuntimeState)
	}
	transitionCount := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged))
	if transitionCount >= 1+2*(nativeHookStateLoopTransitionThreshold+4) {
		t.Fatalf("session_state_changed transitions = %d, want suppressed loop below posted signal count", transitionCount)
	}

	entries, err := fixture.Status.ListForIssue(ctx, issue.ID, 20)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	loopStatusCount := 0
	for _, entry := range entries {
		if entry.SessionID == running.Session.ID && entry.Kind == coordinator.StatusKindBlocker && entry.Message == nativeHookStateLoopStatusMessage {
			loopStatusCount++
		}
	}
	if loopStatusCount != 1 {
		t.Fatalf("native hook loop status count = %d, want 1; entries = %+v", loopStatusCount, entries)
	}

	board, err := fixture.Issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after native hook loop: %v", err)
	}
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != issue.ID || board.WaitReasons[issue.ID] != coordinator.WaitReasonPlanApproval {
		t.Fatalf("board after native hook loop = %+v reasons=%+v, want needs attention for plan approval", board.Board, board.WaitReasons)
	}
}

func TestSessionSignalWatchdogWorkingSuppressedByHumanWaitLatch(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Watchdog signal latch issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)

	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/status", sessionStatusRequest{
		Kind:    coordinator.StatusKindPlan,
		Message: "Plan: inspect, implement, test",
	}, http.StatusOK, nil)

	var response sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/signal", sessionSignalRequest{
		Signal: string(coordinator.SessionSignalWorking),
		Source: coordinator.SessionEventSourceWatchdog,
	}, http.StatusOK, &response)
	if response.Session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("watchdog working state = %q, want waiting", response.Session.RuntimeState)
	}
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 1 {
		t.Fatalf("session_state_changed transitions after suppressed watchdog working = %d, want 1", got)
	}
}

func TestSessionSignalRejectsInvalidSignal(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Invalid signal issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)

	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/signal", sessionSignalRequest{
		Signal: "finished",
	}, http.StatusBadRequest, nil)
}

func TestPlanStatusMovesSessionToNeedsAttentionThenWorkingResumes(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Plan approval issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)

	var status statusResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/status", sessionStatusRequest{
		Kind:    coordinator.StatusKindPlan,
		Message: "Plan: inspect, implement, test",
	}, http.StatusOK, &status)
	if status.Status.Kind != coordinator.StatusKindPlan {
		t.Fatalf("status kind = %q, want plan", status.Status.Kind)
	}

	session, err := fixture.Sessions.GetSession(ctx, running.Session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("session state after plan status = %q, want waiting", session.RuntimeState)
	}
	afterPlanStatus, err := fixture.Issues.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("get issue after plan status: %v", err)
	}
	if afterPlanStatus.PlanApprovedAt != nil {
		t.Fatalf("PlanApprovedAt after plan status = %v, want nil", afterPlanStatus.PlanApprovedAt)
	}
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 1 {
		t.Fatalf("session_state_changed transitions = %d, want 1", got)
	}
	board, err := fixture.Issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != issue.ID || board.LaneStates[issue.ID] != coordinator.LaneStatePlanning || board.WaitReasons[issue.ID] != coordinator.WaitReasonPlanApproval {
		t.Fatalf("board after plan status = %+v lane=%q reason=%q, want needs attention planning/plan approval", board.Board, board.LaneStates[issue.ID], board.WaitReasons[issue.ID])
	}

	var event sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/event", sessionEventRequest{
		State:  string(coordinator.SessionWorking),
		Source: coordinator.SessionEventSourceWatchdog,
	}, http.StatusOK, &event)
	if event.Session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("session state after stale watchdog working = %q, want waiting", event.Session.RuntimeState)
	}
	afterStaleWatchdog, err := fixture.Issues.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("get issue after stale watchdog working: %v", err)
	}
	if afterStaleWatchdog.PlanApprovedAt != nil {
		t.Fatalf("PlanApprovedAt after stale watchdog working = %v, want nil", afterStaleWatchdog.PlanApprovedAt)
	}
	board, err = fixture.Issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after stale watchdog working: %v", err)
	}
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != issue.ID || board.LaneStates[issue.ID] != coordinator.LaneStatePlanning || board.WaitReasons[issue.ID] != coordinator.WaitReasonPlanApproval {
		t.Fatalf("board after stale watchdog working = %+v lane=%q reason=%q, want needs attention planning/plan approval", board.Board, board.LaneStates[issue.ID], board.WaitReasons[issue.ID])
	}
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 1 {
		t.Fatalf("session_state_changed transitions after stale watchdog working = %d, want 1", got)
	}

	// After the stale report is consumed, the next watchdog working signal
	// represents fresh terminal activity after the human response.
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/event", sessionEventRequest{
		State:  string(coordinator.SessionWorking),
		Source: coordinator.SessionEventSourceWatchdog,
	}, http.StatusOK, &event)
	if event.Session.RuntimeState != coordinator.SessionWorking {
		t.Fatalf("session state after resume = %q, want working", event.Session.RuntimeState)
	}
	afterResume, err := fixture.Issues.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("get issue after resume: %v", err)
	}
	if afterResume.PlanApprovedAt != nil {
		t.Fatalf("PlanApprovedAt after resume = %v, want nil until planning ready", afterResume.PlanApprovedAt)
	}
	board, err = fixture.Issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after resume: %v", err)
	}
	if len(board.Board.InProgress) != 1 || board.Board.InProgress[0].ID != issue.ID || board.LaneStates[issue.ID] != coordinator.LaneStatePlanning || board.WaitReasons[issue.ID] != "" {
		t.Fatalf("board after resume = %+v lane=%q reason=%q, want in-progress planning without wait", board.Board, board.LaneStates[issue.ID], board.WaitReasons[issue.ID])
	}
}

func TestPlanningReadyApprovesPlanAndEnqueuesAuthoringSession(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Planning ready issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)
	if got := payloadString(running.Job.Payload, "session_purpose"); got != string(coordinator.AuthorSessionPurposePlanning) {
		t.Fatalf("planning job purpose = %q, want planning", got)
	}

	planBody := "Plan: inspect, implement, test"
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/status", sessionStatusRequest{
		Kind:    coordinator.StatusKindPlan,
		Message: planBody,
	}, http.StatusOK, nil)

	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/ready", readySessionRequest{}, http.StatusBadRequest, nil)
	afterReadyFailure, err := fixture.Issues.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("get issue after failed planning ready: %v", err)
	}
	if afterReadyFailure.PlanApprovedAt != nil {
		t.Fatalf("PlanApprovedAt after failed planning ready = %v, want nil", afterReadyFailure.PlanApprovedAt)
	}

	var approvedResponse issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/plan/approve", map[string]string{}, http.StatusOK, &approvedResponse)
	finished, err := fixture.Sessions.GetSession(ctx, running.Session.ID)
	if err != nil {
		t.Fatalf("get planning session after approval: %v", err)
	}
	if finished.RuntimeState != coordinator.SessionFinished || finished.FinishedAt == nil {
		t.Fatalf("planning session after approval = %+v, want finished", finished)
	}
	approved, err := fixture.Issues.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("get issue after plan approval: %v", err)
	}
	if approved.PlanBody != planBody || approved.PlanApprovedAt == nil {
		t.Fatalf("approved plan body/at = %q/%v, want stored approved plan", approved.PlanBody, approved.PlanApprovedAt)
	}
	change, err := fixture.Sessions.GetChange(ctx, running.Session.ChangeID)
	if err != nil {
		t.Fatalf("get planning change: %v", err)
	}
	if change.ReadyAt != nil {
		t.Fatalf("planning ready marked change ready_at = %v, want nil", change.ReadyAt)
	}

	jobs := liveAuthorJobsForIssue(t, fixture, issue.ID)
	if len(jobs) != 1 {
		t.Fatalf("live author jobs after planning ready = %+v, want one implementation job", jobs)
	}
	if jobs[0].ID == running.Job.ID {
		t.Fatalf("implementation job reused planning job %s", jobs[0].ID)
	}
	if got := payloadString(jobs[0].Payload, "session_purpose"); got != string(coordinator.AuthorSessionPurposeAuthoring) {
		t.Fatalf("implementation job purpose = %q, want authoring", got)
	}
	board, err := fixture.Issues.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board after planning ready: %v", err)
	}
	if board.LaneStates[issue.ID] != coordinator.LaneStateUpNext || board.WaitReasons[issue.ID] != "" {
		t.Fatalf("board after planning ready lane=%q reason=%q, want up_next without wait", board.LaneStates[issue.ID], board.WaitReasons[issue.ID])
	}
}

func TestPlanRejectQueuesCommentsToLivePlanningSession(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Planning reject issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)
	planBody := "Plan: inspect, implement, test"
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/status", sessionStatusRequest{
		Kind:    coordinator.StatusKindPlan,
		Message: planBody,
	}, http.StatusOK, nil)

	var rejected issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/plan/reject", planRejectRequest{
		Comments: "Please split the risky UI and worker changes.",
	}, http.StatusOK, &rejected)
	if rejected.Issue.PlanBody != "" || rejected.Issue.PlanSubmittedAt != nil || rejected.Issue.PlanApprovedAt != nil || rejected.Issue.PlanStatusLogID != nil {
		t.Fatalf("rejected plan fields = body:%q submitted:%v approved:%v status:%v, want cleared pending plan", rejected.Issue.PlanBody, rejected.Issue.PlanSubmittedAt, rejected.Issue.PlanApprovedAt, rejected.Issue.PlanStatusLogID)
	}
	session, err := fixture.Sessions.GetSession(ctx, running.Session.ID)
	if err != nil {
		t.Fatalf("get planning session after reject: %v", err)
	}
	if session.RuntimeState != coordinator.SessionWaiting {
		t.Fatalf("planning session after reject = %q, want waiting for queued comments", session.RuntimeState)
	}
	messages, err := fixture.Sessions.ListPendingSessionMessages(ctx, coordinator.ListPendingSessionMessagesInput{
		SessionID: running.Session.ID,
		LeaseID:   running.Session.LeaseID,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("list pending messages: %v", err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "Please split the risky UI and worker changes.") || !strings.Contains(messages[0].Body, "record a new plan") {
		t.Fatalf("pending messages = %+v, want rejected plan comments and revision instructions", messages)
	}
	entries, err := fixture.Status.ListForIssue(ctx, issue.ID, 5)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	if len(entries) == 0 || entries[0].Kind != coordinator.StatusKindQuestion || !strings.Contains(entries[0].Message, "Plan rejected") {
		t.Fatalf("status entries = %+v, want plan rejection question", entries)
	}
}

// TestAttentionReplyRejectsForeignStatusLogID is the regression for the
// unvalidated client status_log_id finding: a status entry that belongs to a
// different issue must be rejected with 400 before any write, so no orphaned
// "Human response" status row is created on the target issue.
func TestAttentionReplyRejectsForeignStatusLogID(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Attention reply issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	startRunningAuthorSession(t, fixture, issue.ID)

	other, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Other issue"})
	if err != nil {
		t.Fatalf("create other issue: %v", err)
	}
	foreign, err := fixture.Status.Write(ctx, coordinator.WriteStatusInput{
		IssueID: other.ID,
		Actor:   "agent",
		Kind:    coordinator.StatusKindQuestion,
		Message: "Foreign question",
	})
	if err != nil {
		t.Fatalf("write foreign status: %v", err)
	}

	var resp errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/attention/reply", attentionReplyRequest{
		Message:     "My answer",
		StatusLogID: &foreign.ID,
	}, http.StatusBadRequest, &resp)
	if resp.Error.Code != "invalid_status_log_id" {
		t.Fatalf("error code = %q, want invalid_status_log_id", resp.Error.Code)
	}

	entries, err := fixture.Status.ListForIssue(ctx, issue.ID, 20)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Message, "Human response:") {
			t.Fatalf("found orphaned human response status entry on target issue: %+v", entry)
		}
	}
}

// TestAttentionReplyRejectsNonExistentStatusLogID is the regression for the
// finding that a non-existent status_log_id tripped the message FK only after an
// orphaned status row had been written. Validation must now reject it with 400
// before any write.
func TestAttentionReplyRejectsNonExistentStatusLogID(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Attention reply issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	startRunningAuthorSession(t, fixture, issue.ID)

	missing := int64(987654321)
	var resp errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/attention/reply", attentionReplyRequest{
		Message:     "My answer",
		StatusLogID: &missing,
	}, http.StatusBadRequest, &resp)
	if resp.Error.Code != "invalid_status_log_id" {
		t.Fatalf("error code = %q, want invalid_status_log_id", resp.Error.Code)
	}

	entries, err := fixture.Status.ListForIssue(ctx, issue.ID, 20)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Message, "Human response:") {
			t.Fatalf("found status entry written before validation: %+v", entry)
		}
	}
}

// TestAttentionReplyLinksOwnIssueStatusLogID is the regression confirming that a
// valid status_log_id belonging to the issue is accepted and threaded onto the
// queued session message.
func TestAttentionReplyLinksOwnIssueStatusLogID(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Attention reply issue", PlanMode: true})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	startRunningAuthorSession(t, fixture, issue.ID)

	question, err := fixture.Status.Write(ctx, coordinator.WriteStatusInput{
		IssueID: issue.ID,
		Actor:   "agent",
		Kind:    coordinator.StatusKindQuestion,
		Message: "What database should I use?",
	})
	if err != nil {
		t.Fatalf("write question status: %v", err)
	}

	var resp sessionMessageResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/attention/reply", attentionReplyRequest{
		Message:     "Use sqlite",
		StatusLogID: &question.ID,
	}, http.StatusOK, &resp)
	if !resp.Queued {
		t.Fatalf("reply queued = false, want true for live session")
	}
	if resp.Message.StatusLogID == nil || *resp.Message.StatusLogID != question.ID {
		t.Fatalf("message status_log_id = %v, want %d", resp.Message.StatusLogID, question.ID)
	}
}

// TestCloseIssueRoutesThroughEngine is the regression for routing issue close
// through the lifecycle engine: it must log a close_issue transition and the
// issue must land in the abandoned phase.
func TestCloseIssueRoutesThroughEngine(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Close issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	var closed issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/close", map[string]string{}, http.StatusOK, &closed)
	if closed.Issue.ScheduleState != coordinator.ScheduleClosed {
		t.Fatalf("closed ScheduleState = %q, want closed", closed.Issue.ScheduleState)
	}

	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventCloseIssue)); got != 1 {
		t.Fatalf("close_issue transitions = %d, want 1", got)
	}
	var toPhase string
	var actor string
	var payloadJSON string
	if err := fixture.DB.QueryRow(`
SELECT to_phase, actor, payload_json
FROM transitions
WHERE issue_id = ?
	AND event_kind = ?`, issue.ID, string(lifecycle.EventCloseIssue)).Scan(&toPhase, &actor, &payloadJSON); err != nil {
		t.Fatalf("read close_issue transition: %v", err)
	}
	if toPhase != string(coordinator.PhaseAbandoned) {
		t.Fatalf("close_issue to_phase = %q, want abandoned", toPhase)
	}
	if actor != "owner:owner" {
		t.Fatalf("close_issue actor = %q, want owner:owner", actor)
	}
	var payload struct {
		Audit lifecycle.EventAudit `json:"audit"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("decode close_issue payload: %v", err)
	}
	assertLifecycleAudit(t, payload.Audit, lifecycle.EventAudit{
		Method:      http.MethodPost,
		Path:        "/v1/issues/" + issue.ID + "/close",
		Principal:   "owner:owner",
		ProjectID:   fixture.Project.ID,
		ProjectName: fixture.Project.Name,
		IssueID:     issue.ID,
	})
	var eventJSON string
	if err := fixture.DB.QueryRow(`
SELECT event_json
FROM event_inbox
WHERE issue_id = ?
ORDER BY created_at DESC
LIMIT 1`, issue.ID).Scan(&eventJSON); err != nil {
		t.Fatalf("read close_issue inbox event: %v", err)
	}
	var stored struct {
		Kind  lifecycle.EventKind   `json:"kind"`
		Actor coordinator.Principal `json:"actor"`
		Audit lifecycle.EventAudit  `json:"audit"`
	}
	if err := json.Unmarshal([]byte(eventJSON), &stored); err != nil {
		t.Fatalf("decode close_issue inbox event: %v", err)
	}
	if stored.Kind != lifecycle.EventCloseIssue {
		t.Fatalf("inbox event kind = %q, want %q", stored.Kind, lifecycle.EventCloseIssue)
	}
	if stored.Actor.Scope != coordinator.TokenScopeOwner || stored.Actor.Subject != "owner" {
		t.Fatalf("inbox actor = %+v, want owner principal", stored.Actor)
	}
	assertLifecycleAudit(t, stored.Audit, payload.Audit)

	var phase string
	if err := fixture.DB.QueryRow(`SELECT phase FROM workflow_state WHERE issue_id = ?`, issue.ID).Scan(&phase); err != nil {
		t.Fatalf("read workflow_state: %v", err)
	}
	if phase != string(coordinator.PhaseAbandoned) {
		t.Fatalf("workflow_state phase = %q, want abandoned", phase)
	}
}

func TestWebLifecycleAuditRecordsWebSession(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Browser close issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	sessionCookie, csrfCookie := loginWebUI(t, fixture)
	var webSessionID string
	if err := fixture.GlobalDB.QueryRow(`SELECT id FROM web_sessions ORDER BY created_at DESC LIMIT 1`).Scan(&webSessionID); err != nil {
		t.Fatalf("read web session id: %v", err)
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, webAPIPrefix+"/v1/issues/"+issue.ID+"/close", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "FlowTest/1.0")
	request.Header.Set(webCSRFHeader, csrfCookie.Value)
	request.AddCookie(sessionCookie)
	request.AddCookie(csrfCookie)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("web close status = %d, want 200; body: %s", response.Code, response.Body.String())
	}

	var actor string
	var payloadJSON string
	if err := fixture.DB.QueryRow(`
SELECT actor, payload_json
FROM transitions
WHERE issue_id = ?
	AND event_kind = ?`, issue.ID, string(lifecycle.EventCloseIssue)).Scan(&actor, &payloadJSON); err != nil {
		t.Fatalf("read web close transition: %v", err)
	}
	if actor != "owner:web" {
		t.Fatalf("web close actor = %q, want owner:web", actor)
	}
	var payload struct {
		Audit lifecycle.EventAudit `json:"audit"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("decode web close payload: %v", err)
	}
	assertLifecycleAudit(t, payload.Audit, lifecycle.EventAudit{
		Method:       http.MethodPost,
		Path:         "/v1/issues/" + issue.ID + "/close",
		Principal:    "owner:web",
		ProjectID:    fixture.Project.ID,
		ProjectName:  fixture.Project.Name,
		IssueID:      issue.ID,
		UserAgent:    "FlowTest/1.0",
		WebSessionID: webSessionID,
	})
}

func TestIssuePauseAndResumeEndpointsSuspendAndRequeueTask(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Pause issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)
	if running.Session == nil || running.Job.ID == "" {
		t.Fatalf("running response = %+v", running)
	}

	var paused issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/pause", nil, http.StatusOK, &paused)
	if paused.Issue.ID != issue.ID || paused.Issue.ScheduleState != coordinator.ScheduleUpNext {
		t.Fatalf("paused issue = %+v", paused.Issue)
	}
	session, err := fixture.Sessions.GetSession(ctx, running.Session.ID)
	if err != nil {
		t.Fatalf("get paused session: %v", err)
	}
	if session.RuntimeState != coordinator.SessionAbandoned || session.FinishedAt == nil {
		t.Fatalf("paused session = %+v, want abandoned with finished_at", session)
	}
	job, err := fixture.Workers.GetJob(ctx, running.Job.ID)
	if err != nil {
		t.Fatalf("get paused job: %v", err)
	}
	if job.State != flowworker.JobCanceled {
		t.Fatalf("paused job state = %q, want canceled", job.State)
	}
	if _, err := fixture.Credentials.Authenticate(ctx, running.SessionToken); !errors.Is(err, coordinator.ErrInvalidCredential) {
		t.Fatalf("authenticate paused token err = %v, want ErrInvalidCredential", err)
	}
	var pausedDetail issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+issue.ID, nil, http.StatusOK, &pausedDetail)
	if pausedDetail.Detail == nil || !pausedDetail.Detail.Paused {
		t.Fatalf("paused detail = %+v, want paused", pausedDetail.Detail)
	}

	var resumed issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/resume", nil, http.StatusOK, &resumed)
	if resumed.Issue.ID != issue.ID {
		t.Fatalf("resumed issue = %+v", resumed.Issue)
	}
	resumeJob, ok, err := fixture.Workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job after resume: %v", err)
	}
	if !ok || resumeJob.ID == running.Job.ID || resumeJob.ChangeID == nil || *resumeJob.ChangeID != running.Session.ChangeID {
		t.Fatalf("resume job = %+v ok=%t, want new job for change %s", resumeJob, ok, running.Session.ChangeID)
	}
	var resumedDetail issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+issue.ID, nil, http.StatusOK, &resumedDetail)
	if resumedDetail.Detail == nil || resumedDetail.Detail.Paused {
		t.Fatalf("resumed detail = %+v, want not paused", resumedDetail.Detail)
	}
}

func assertLifecycleAudit(t *testing.T, got lifecycle.EventAudit, want lifecycle.EventAudit) {
	t.Helper()
	if got != want {
		t.Fatalf("lifecycle audit = %+v, want %+v", got, want)
	}
}

func TestSessionReadyRejectsExpiredAuthorLease(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Expired ready issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start author session: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claimed.Lease.ID); err != nil {
		t.Fatalf("expire author lease: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", map[string]string{}, http.StatusBadRequest, nil)
	crashed, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get crashed session: %v", err)
	}
	if crashed.RuntimeState != coordinator.SessionCrashed {
		t.Fatalf("session state = %q, want crashed", crashed.RuntimeState)
	}
	if _, err := fixture.Credentials.Authenticate(ctx, started.Token); !errors.Is(err, coordinator.ErrInvalidCredential) {
		t.Fatalf("authenticate crashed token err = %v, want ErrInvalidCredential", err)
	}
	resumeJob, ok, err := fixture.Workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job: %v", err)
	}
	if !ok || resumeJob.ChangeID == nil || *resumeJob.ChangeID != started.Change.ID {
		t.Fatalf("resume job = %+v ok=%t, want change %s", resumeJob, ok, started.Change.ID)
	}
}

func TestBoardReconcilesExpiredAuthorSession(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Board crash resume issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start author session: %v", err)
	}
	if _, err := fixture.Sessions.UpdateSessionState(ctx, started.Session.ID, coordinator.SessionWaiting); err != nil {
		t.Fatalf("mark waiting: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claimed.Lease.ID); err != nil {
		t.Fatalf("expire author lease: %v", err)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.UpNext) != 1 || board.Board.UpNext[0].ID != issue.ID {
		t.Fatalf("up_next board = %+v", board.Board.UpNext)
	}
	if len(board.Board.InProgress) != 0 || len(board.Board.NeedsAttention) != 0 {
		t.Fatalf("active board lanes after reconcile = in_progress:%+v needs_attention:%+v", board.Board.InProgress, board.Board.NeedsAttention)
	}
	crashed, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get crashed session: %v", err)
	}
	if crashed.RuntimeState != coordinator.SessionCrashed {
		t.Fatalf("session state = %q, want crashed", crashed.RuntimeState)
	}
	resumeJob, ok, err := fixture.Workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job: %v", err)
	}
	if !ok || resumeJob.ChangeID == nil || *resumeJob.ChangeID != started.Change.ID {
		t.Fatalf("resume job = %+v ok=%t, want change %s", resumeJob, ok, started.Change.ID)
	}
}

func TestRetryCrashedAuthorJobClearsCrashHold(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Retry crash hold"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	first, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE id = ?`, string(flowworker.JobCanceled), first.Job.ID); err != nil {
		t.Fatalf("cancel first job: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO status_log (issue_id, actor, message, kind, created_at)
VALUES (?, ?, ?, ?, ?)`,
		issue.ID,
		"system",
		"Crashed author job reached 2 automatic restarts; human intervention required.",
		coordinator.StatusKindBlocker,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert crash blocker: %v", err)
	}

	var held boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &held)
	if got := held.WaitReasons[issue.ID]; got != coordinator.WaitReasonCrashLoop {
		t.Fatalf("wait reason before retry = %q, want %q", got, coordinator.WaitReasonCrashLoop)
	}

	var response issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/retry", map[string]string{}, http.StatusOK, &response)
	if response.Issue.ID != issue.ID {
		t.Fatalf("retry response issue = %+v", response.Issue)
	}
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventRetryCrashedAuthorJob)); got != 1 {
		t.Fatalf("retry transitions = %d, want 1", got)
	}
	live, ok, err := fixture.Workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job after retry: %v", err)
	}
	if !ok || live.ID == first.Job.ID || live.ChangeID == nil || *live.ChangeID != first.Change.ID {
		t.Fatalf("live job after retry = %+v ok=%t, want new job for change %s", live, ok, first.Change.ID)
	}
	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if got := board.WaitReasons[issue.ID]; got != "" {
		t.Fatalf("wait reason after retry = %q, want empty", got)
	}
	card := board.IssueCards[issue.ID]
	if card.LatestStatus == nil || card.LatestStatus.Kind != coordinator.StatusKindProgress {
		t.Fatalf("latest status after retry = %+v, want progress note", card.LatestStatus)
	}
}

func TestSessionStatusIsVisibleInIssueDetail(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Status issue")

	var written statusResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "Running focused tests",
	}, http.StatusOK, &written)
	if written.Status.IssueID != started.Session.IssueID || written.Status.ChangeID != started.Session.ChangeID {
		t.Fatalf("written status = %+v, want issue %s change %s", written.Status, started.Session.IssueID, started.Session.ChangeID)
	}

	var issue issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+started.Session.IssueID, nil, http.StatusOK, &issue)
	if len(issue.StatusLog) != 1 || issue.StatusLog[0].Message != "Running focused tests" {
		t.Fatalf("issue status log = %+v", issue.StatusLog)
	}
	if issue.StatusLog[0].Kind != coordinator.StatusKindNote {
		t.Fatalf("default status kind = %q, want %q", issue.StatusLog[0].Kind, coordinator.StatusKindNote)
	}
}

func TestSessionStatusAcceptsKind(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Status kind issue")

	var written statusResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "Which datastore should I use?",
		Kind:    coordinator.StatusKindQuestion,
	}, http.StatusOK, &written)
	if written.Status.Kind != coordinator.StatusKindQuestion {
		t.Fatalf("written status kind = %q, want %q", written.Status.Kind, coordinator.StatusKindQuestion)
	}

	var issue issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+started.Session.IssueID, nil, http.StatusOK, &issue)
	if len(issue.StatusLog) != 1 || issue.StatusLog[0].Kind != coordinator.StatusKindQuestion {
		t.Fatalf("issue status log = %+v", issue.StatusLog)
	}
}

func TestSessionStatusRejectsBadKind(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Bad kind issue")

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "boom",
		Kind:    "urgent",
	}, http.StatusBadRequest, nil)
}

// TestSessionStatusTouchesAgentActivity is the regression for agent-level
// liveness: writing a status entry is agent activity and must stamp
// last_agent_activity_at, which is nil before the first signal.
func TestSessionStatusTouchesAgentActivity(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Liveness status issue")

	before, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session before status: %v", err)
	}
	if before.LastAgentActivityAt != nil {
		t.Fatalf("LastAgentActivityAt before status = %v, want nil", before.LastAgentActivityAt)
	}

	var written statusResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "Running focused tests",
	}, http.StatusOK, &written)

	after, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session after status: %v", err)
	}
	if after.LastAgentActivityAt == nil {
		t.Fatalf("LastAgentActivityAt after status = nil, want a timestamp")
	}
}

// TestSessionEventTouchesAgentActivity asserts the no-op same-state fast path in
// handleSessionEvent still records agent activity: even a repeated state report
// proves the agent is alive, so last_agent_activity_at must advance.
func TestSessionEventTouchesAgentActivity(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Liveness event issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, issue.ID)

	// First flip working->waiting (engine path) stamps activity.
	var event sessionResponse
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, &event)
	firstTouch, err := fixture.Sessions.GetSession(ctx, running.Session.ID)
	if err != nil {
		t.Fatalf("get session after first event: %v", err)
	}
	if firstTouch.LastAgentActivityAt == nil {
		t.Fatalf("LastAgentActivityAt after engine-path event = nil, want a timestamp")
	}

	// Backdate the stamp so the no-op fast path's advance is observable.
	backdated := time.Now().UTC().Add(-time.Hour)
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE sessions SET last_agent_activity_at = ? WHERE id = ?`, backdated.Format(time.RFC3339Nano), running.Session.ID); err != nil {
		t.Fatalf("backdate agent activity: %v", err)
	}

	// Re-post the same state: the no-op fast path returns the session unchanged
	// but must still stamp last_agent_activity_at.
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v1/sessions/"+running.Session.ID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, &event)
	if got := countTransitions(t, fixture, issue.ID, string(lifecycle.EventSessionStateChanged)); got != 1 {
		t.Fatalf("session_state_changed transitions after no-op re-post = %d, want 1", got)
	}
	noopTouch, err := fixture.Sessions.GetSession(ctx, running.Session.ID)
	if err != nil {
		t.Fatalf("get session after no-op event: %v", err)
	}
	if noopTouch.LastAgentActivityAt == nil {
		t.Fatalf("LastAgentActivityAt after no-op event = nil, want a timestamp")
	}
	if !noopTouch.LastAgentActivityAt.After(backdated) {
		t.Fatalf("no-op event did not advance LastAgentActivityAt past %v: got %v", backdated, noopTouch.LastAgentActivityAt)
	}
}

func TestSessionAttachRequiresOwnerToken(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Attach issue")
	if _, err := fixture.Sessions.RegisterTerminalTarget(context.Background(), started.Session.ID, "http://127.0.0.1:7777", "/tmp/flow-session.sock"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodGet, "/v1/sessions/"+started.Session.ID+"/attach", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/attach", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/attach", nil, http.StatusForbidden, nil)

	var response attachResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/attach", nil, http.StatusOK, &response)
	if response.Attach.SessionID != started.Session.ID || response.Attach.TmuxSession == "" {
		t.Fatalf("attach response = %+v", response.Attach)
	}
	if len(response.Attach.Command) != 6 || response.Attach.Command[0] != "tmux" || response.Attach.Command[1] != "-S" || response.Attach.Command[2] != "/tmp/flow-session.sock" || response.Attach.Command[5] != response.Attach.TmuxSession {
		t.Fatalf("attach command = %#v", response.Attach.Command)
	}
	if response.Attach.ProxyPath != "/v1/sessions/"+started.Session.ID+"/terminal" {
		t.Fatalf("proxy path = %q", response.Attach.ProxyPath)
	}
}

func TestJobAttachAllowsLiveReviewerJobs(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Reviewer attach issue")
	reviewer := startLiveCheckJobForIssue(t, fixture, "reviewer-token", "w-review-attach", started.Session.IssueID, started.Change.ID, "head-1", "reviewer", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	if _, err := fixture.Sessions.RegisterJobTerminalTarget(context.Background(), reviewer.Job.ID, reviewer.Lease.ID, "http://127.0.0.1:7778", "/tmp/flow-job.sock"); err != nil {
		t.Fatalf("register job terminal target: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/jobs/"+reviewer.Job.ID+"/attach", nil, http.StatusForbidden, nil)

	var response attachResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs/"+reviewer.Job.ID+"/attach", nil, http.StatusOK, &response)
	if response.Attach.SessionID != "" || response.Attach.JobID != reviewer.Job.ID || response.Attach.TmuxSession != "flow-"+reviewer.Job.ID {
		t.Fatalf("job attach response = %+v", response.Attach)
	}
	if len(response.Attach.Command) != 6 || response.Attach.Command[0] != "tmux" || response.Attach.Command[1] != "-S" || response.Attach.Command[2] != "/tmp/flow-job.sock" || response.Attach.Command[5] != response.Attach.TmuxSession {
		t.Fatalf("job attach command = %#v", response.Attach.Command)
	}

	if _, err := fixture.Workers.ReleaseLease(context.Background(), reviewer.Lease.ID, flowworker.JobFinished); err != nil {
		t.Fatalf("release reviewer lease: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs/"+reviewer.Job.ID+"/attach", nil, http.StatusBadRequest, nil)
}

func TestJobTerminalProxyAllowsLiveReviewerJobs(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Reviewer terminal issue")
	reviewer := startLiveCheckJobForIssue(t, fixture, "reviewer-terminal-token", "w-review-terminal", started.Session.IssueID, started.Change.ID, "head-1", "reviewer", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pty" || r.URL.RawQuery != "q=1" {
			t.Fatalf("proxied request URL = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("proxy forwarded authorization header")
		}
		w.Header().Set("X-Terminal-Target", "ok")
		_, _ = w.Write([]byte("proxied job terminal"))
	}))
	t.Cleanup(target.Close)

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs/"+reviewer.Job.ID+"/terminal", jobTerminalRequest{
		LeaseID:   reviewer.Lease.ID,
		TargetURL: target.URL,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/jobs/"+reviewer.Job.ID+"/terminal", jobTerminalRequest{
		LeaseID:   reviewer.Lease.ID,
		TargetURL: target.URL,
	}, http.StatusForbidden, nil)

	var registered jobTerminalResponse
	doJSONRequestAs(t, fixture.Server, "reviewer-terminal-token", http.MethodPost, "/v1/jobs/"+reviewer.Job.ID+"/terminal", jobTerminalRequest{
		LeaseID:   reviewer.Lease.ID,
		TargetURL: target.URL,
	}, http.StatusOK, &registered)
	if registered.Terminal.JobID != reviewer.Job.ID || registered.Terminal.LeaseID != reviewer.Lease.ID || registered.Terminal.TargetURL != target.URL {
		t.Fatalf("registered job terminal = %+v", registered.Terminal)
	}

	var jobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/jobs", nil, http.StatusOK, &jobs)
	if !jobs.Diagnostics[reviewer.Job.ID].TerminalAvailable {
		t.Fatalf("job terminal availability = %+v", jobs.Diagnostics[reviewer.Job.ID])
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	card := board.IssueCards[started.Session.IssueID]
	if !card.TerminalAvailable || card.TerminalJobID != reviewer.Job.ID {
		t.Fatalf("issue card terminal metadata = %+v", card)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/jobs/"+reviewer.Job.ID+"/terminal-token", map[string]string{}, http.StatusForbidden, nil)
	var access jobTerminalAccessResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs/"+reviewer.Job.ID+"/terminal-token", map[string]string{}, http.StatusOK, &access)
	if access.Access.LoginPath == "" || !strings.Contains(access.Access.LoginPath, "/v1/jobs/"+reviewer.Job.ID+"/terminal-login?token=") {
		t.Fatalf("job terminal access = %+v", access.Access)
	}

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodGet, access.Access.LoginPath, nil)
	fixture.Server.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("job terminal login status = %d body = %q", login.Code, login.Body.String())
	}
	if login.Header().Get("Location") != "/v1/jobs/"+reviewer.Job.ID+"/terminal" {
		t.Fatalf("job terminal login location = %q", login.Header().Get("Location"))
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != terminalAccessCookie || !cookies[0].HttpOnly || cookies[0].Path != "/v1/jobs/"+reviewer.Job.ID+"/terminal" {
		t.Fatalf("job terminal login cookies = %+v", cookies)
	}

	cookieProxyRequest := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+reviewer.Job.ID+"/terminal/pty?q=1", nil)
	cookieProxyRequest.AddCookie(cookies[0])
	cookieProxyResponse := httptest.NewRecorder()
	fixture.Server.ServeHTTP(cookieProxyResponse, cookieProxyRequest)
	if cookieProxyResponse.Code != http.StatusOK {
		t.Fatalf("job terminal cookie proxy status = %d body = %q", cookieProxyResponse.Code, cookieProxyResponse.Body.String())
	}
	if cookieProxyResponse.Body.String() != "proxied job terminal" {
		t.Fatalf("job terminal cookie proxy body = %q", cookieProxyResponse.Body.String())
	}
	if cookieProxyResponse.Header().Get("Content-Security-Policy") != terminalSandboxCSP {
		t.Fatalf("job terminal cookie proxy CSP = %q, want %q", cookieProxyResponse.Header().Get("Content-Security-Policy"), terminalSandboxCSP)
	}

	if _, err := fixture.Workers.ReleaseLease(context.Background(), reviewer.Lease.ID, flowworker.JobFinished); err != nil {
		t.Fatalf("release reviewer lease: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs/"+reviewer.Job.ID+"/terminal-token", map[string]string{}, http.StatusBadRequest, nil)
}

func TestSessionTerminalProxyRequiresOwnerAndProxiesRegisteredTarget(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Terminal proxy issue")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pty" || r.URL.RawQuery != "q=1" {
			t.Fatalf("proxied request URL = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("proxy forwarded authorization header")
		}
		w.Header().Set("X-Terminal-Target", "ok")
		_, _ = w.Write([]byte("proxied terminal"))
	}))
	t.Cleanup(target.Close)

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/sessions/"+started.Session.ID+"/terminal", sessionTerminalRequest{
		TargetURL: target.URL,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/sessions/"+started.Session.ID+"/terminal", sessionTerminalRequest{
		TargetURL: target.URL,
	}, http.StatusForbidden, nil)

	var registered sessionTerminalResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/terminal", sessionTerminalRequest{
		TargetURL: target.URL,
	}, http.StatusOK, &registered)
	if registered.Terminal.TargetURL != target.URL {
		t.Fatalf("registered terminal = %+v", registered.Terminal)
	}

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodGet, "/v1/sessions/"+started.Session.ID+"/terminal/pty?q=1", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/terminal/pty?q=1", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/terminal/pty?q=1", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/sessions/"+started.Session.ID+"/terminal-token", map[string]string{}, http.StatusForbidden, nil)

	var access sessionTerminalAccessResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/sessions/"+started.Session.ID+"/terminal-token", map[string]string{}, http.StatusOK, &access)
	if access.Access.LoginPath == "" || !strings.Contains(access.Access.LoginPath, "/terminal-login?token=") {
		t.Fatalf("terminal access = %+v", access.Access)
	}
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodGet, access.Access.LoginPath, nil)
	fixture.Server.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d body = %q", login.Code, login.Body.String())
	}
	if login.Header().Get("Location") != "/v1/sessions/"+started.Session.ID+"/terminal" {
		t.Fatalf("login location = %q", login.Header().Get("Location"))
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != terminalAccessCookie || !cookies[0].HttpOnly {
		t.Fatalf("login cookies = %+v", cookies)
	}

	cookieProxyRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+started.Session.ID+"/terminal/pty?q=1", nil)
	cookieProxyRequest.AddCookie(cookies[0])
	cookieProxyResponse := httptest.NewRecorder()
	fixture.Server.ServeHTTP(cookieProxyResponse, cookieProxyRequest)
	if cookieProxyResponse.Code != http.StatusOK {
		t.Fatalf("cookie proxy status = %d body = %q", cookieProxyResponse.Code, cookieProxyResponse.Body.String())
	}
	if cookieProxyResponse.Body.String() != "proxied terminal" {
		t.Fatalf("cookie proxy body = %q", cookieProxyResponse.Body.String())
	}
	if cookieProxyResponse.Header().Get("Content-Security-Policy") != terminalSandboxCSP {
		t.Fatalf("cookie proxy CSP = %q, want %q", cookieProxyResponse.Header().Get("Content-Security-Policy"), terminalSandboxCSP)
	}
	if !strings.Contains(cookieProxyResponse.Header().Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatalf("cookie proxy CSP = %q, want allow-same-origin so ttyd token fetch remains same-origin", cookieProxyResponse.Header().Get("Content-Security-Policy"))
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+started.Session.ID+"/terminal/pty?q=1", nil)
	request.Header.Set("Authorization", "Bearer owner-token")
	request.Header.Set(protocolHeader, fixture.Server.protocolVersion)
	response := httptest.NewRecorder()
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %q", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Terminal-Target") != "ok" || response.Body.String() != "proxied terminal" {
		t.Fatalf("proxy response header=%q body=%q", response.Header().Get("X-Terminal-Target"), response.Body.String())
	}
	if response.Header().Get("Content-Security-Policy") != terminalSandboxCSP {
		t.Fatalf("proxy CSP = %q, want %q", response.Header().Get("Content-Security-Policy"), terminalSandboxCSP)
	}
}

func TestSessionAttachRejectsInactiveOrNonLiveSessions(t *testing.T) {
	t.Run("finished", func(t *testing.T) {
		fixture := newTestFixture(t)
		started := startAuthorSessionForStatusTest(t, fixture, "Finished attach issue")
		if _, err := fixture.Sessions.ReadyAuthorSession(context.Background(), started.Session.ID); err != nil {
			t.Fatalf("ready session: %v", err)
		}

		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/attach", nil, http.StatusBadRequest, nil)
	})

	t.Run("crashed", func(t *testing.T) {
		fixture := newTestFixture(t)
		started := startAuthorSessionForStatusTest(t, fixture, "Crashed attach issue")
		if _, err := fixture.Sessions.UpdateSessionState(context.Background(), started.Session.ID, coordinator.SessionCrashed); err != nil {
			t.Fatalf("crash session: %v", err)
		}

		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/attach", nil, http.StatusBadRequest, nil)
	})

	t.Run("released lease", func(t *testing.T) {
		fixture := newTestFixture(t)
		started := startAuthorSessionForStatusTest(t, fixture, "Released lease attach issue")
		if _, err := fixture.DB.ExecContext(context.Background(), `
UPDATE leases
SET released_at = ?
WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), started.Session.LeaseID); err != nil {
			t.Fatalf("release lease: %v", err)
		}

		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/sessions/"+started.Session.ID+"/attach", nil, http.StatusBadRequest, nil)
	})
}

func TestReviewThreadAPILifecycle(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Thread API issue")

	var created threadResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/changes/"+started.Change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/app.go",
		Line:            12,
		Context:         "return value",
		Body:            "Please handle nil.",
	}, http.StatusCreated, &created)
	if created.Thread.State != coordinator.ThreadOpen || len(created.Thread.Comments) != 1 {
		t.Fatalf("created thread = %+v", created.Thread)
	}

	var listed threadsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/changes/"+started.Change.ID+"/threads", nil, http.StatusOK, &listed)
	if len(listed.Threads) != 1 || listed.Threads[0].ID != created.Thread.ID {
		t.Fatalf("listed threads = %+v", listed.Threads)
	}

	var replied threadResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body: "I will fix it.",
	}, http.StatusOK, &replied)
	if len(replied.Thread.Comments) != 2 {
		t.Fatalf("replied thread = %+v", replied.Thread)
	}

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/threads/"+created.Thread.ID+"/claims", threadClaimRequest{
		Kind: string(coordinator.ClaimNotWarranted),
	}, http.StatusBadRequest, nil)

	var claimed threadResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/threads/"+created.Thread.ID+"/claims", threadClaimRequest{
		Kind:           string(coordinator.ClaimFixed),
		ClaimCommitSHA: "def456",
	}, http.StatusOK, &claimed)
	if claimed.Thread.State != coordinator.ThreadClaimed || claimed.Thread.ClaimCommitSHA == nil || *claimed.Thread.ClaimCommitSHA != "def456" {
		t.Fatalf("claimed thread = %+v", claimed.Thread)
	}

	verifier := startLiveWorkerJobForIssue(t, fixture, "verifier-token", "w-verifier", started.Session.IssueID, started.Change.ID, flowworker.RoleVerifier)

	var certified threadResponse
	doJSONRequestAs(t, fixture.Server, "verifier-token", http.MethodPost, "/v1/threads/"+created.Thread.ID+"/certify", threadCommentRequest{
		Body:    "Verified.",
		LeaseID: verifier.Lease.ID,
	}, http.StatusOK, &certified)
	if certified.Thread.State != coordinator.ThreadCertified {
		t.Fatalf("certified thread = %+v", certified.Thread)
	}

	var reopened threadResponse
	doJSONRequestAs(t, fixture.Server, "verifier-token", http.MethodPost, "/v1/threads/"+created.Thread.ID+"/reopen", threadCommentRequest{
		Body:    "Still broken.",
		LeaseID: verifier.Lease.ID,
	}, http.StatusOK, &reopened)
	if reopened.Thread.State != coordinator.ThreadReopened {
		t.Fatalf("reopened thread = %+v", reopened.Thread)
	}
}

func TestReviewThreadAPIRestrictsSessionAndWorkerAccess(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Thread auth issue")
	other := startAuthorSessionForStatusTestWithWorker(t, fixture, "Other thread auth issue", "w-other-author")

	var created threadResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/changes/"+started.Change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/app.go",
		Line:            12,
		Body:            "Please handle nil.",
	}, http.StatusCreated, &created)

	doJSONRequestAs(t, fixture.Server, other.Token, http.MethodPost, "/v1/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body: "Cross-issue reply.",
	}, http.StatusForbidden, nil)

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/threads/"+created.Thread.ID+"/certify", threadCommentRequest{
		Body: "Author cannot verify.",
	}, http.StatusForbidden, nil)

	reviewer := startLiveWorkerJobForIssue(t, fixture, "reviewer-token", "w-reviewer", started.Session.IssueID, started.Change.ID, flowworker.RoleReviewer)
	doJSONRequestAs(t, fixture.Server, "reviewer-token", http.MethodPost, "/v1/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body:    "Reviewer reply.",
		LeaseID: reviewer.Lease.ID,
	}, http.StatusOK, nil)

	doJSONRequestAs(t, fixture.Server, "reviewer-token", http.MethodPost, "/v1/threads/"+created.Thread.ID+"/certify", threadCommentRequest{
		Body:    "Wrong role.",
		LeaseID: reviewer.Lease.ID,
	}, http.StatusForbidden, nil)

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body: "No live lease.",
	}, http.StatusForbidden, nil)
}

func TestReviewerAndVerifierJobsIncludeReviewContext(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Review context issue")
	thread, err := fixture.Threads.CreateThread(ctx, coordinator.CreateThreadInput{
		ChangeID:        started.Change.ID,
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/api/server.go",
		Line:            42,
		Body:            "Please verify this.",
		Actor:           "owner",
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	for _, role := range []flowworker.JobRole{flowworker.RoleReviewer, flowworker.RoleVerifier} {
		var response jobResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs", enqueueJobRequest{
			IssueID:        &started.Session.IssueID,
			Role:           string(role),
			CapacityBucket: string(flowworker.BucketPersistentAgent),
			Payload:        map[string]any{"existing": "value"},
		}, http.StatusCreated, &response)
		if response.Job.Payload["existing"] != "value" {
			t.Fatalf("%s payload = %+v, want existing key preserved", role, response.Job.Payload)
		}
		reviewContext, ok := response.Job.Payload["review_context"].(map[string]any)
		if !ok {
			t.Fatalf("%s payload review_context = %#v", role, response.Job.Payload["review_context"])
		}
		if reviewContext["issue_id"] != started.Session.IssueID {
			t.Fatalf("%s review_context issue_id = %#v, want %s", role, reviewContext["issue_id"], started.Session.IssueID)
		}
		threads, ok := reviewContext["threads"].([]any)
		if !ok || len(threads) != 1 {
			t.Fatalf("%s review_context threads = %#v", role, reviewContext["threads"])
		}
		threadPayload, ok := threads[0].(map[string]any)
		if !ok || threadPayload["id"] != thread.ID {
			t.Fatalf("%s review_context thread = %#v, want %s", role, threads[0], thread.ID)
		}
	}
}

func TestBlockedCheckEnqueuesSingleFixAuthorJobAndReadyResetsCheck(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	// This test drives the blocked-check fix loop with synthetic head SHAs, so
	// it models a project with no exchange-configured checks: clear the
	// exchange path so ready skips the check-config listing.
	repointFixtureExchange(t, fixture, "")
	started := startAuthorSessionForStatusTest(t, fixture, "Fix loop issue")
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: "head-1",
	}, http.StatusOK, nil)

	required := true
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/ci", reportCheckRequest{
		Kind:     string(coordinator.CheckKindCI),
		Required: &required,
		Verdict:  string(coordinator.CheckSatisfied),
		Details:  "Passed.",
	}, http.StatusOK, nil)
	var blocked checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/reviewer", reportCheckRequest{
		Kind:     string(coordinator.CheckKindReviewer),
		Required: &required,
		Verdict:  string(coordinator.CheckBlocked),
		Details:  "Needs a fix.",
	}, http.StatusOK, &blocked)
	if blocked.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("review state = %q, want changes_requested", blocked.ReviewState)
	}

	fixJob, ok, err := fixture.Workers.LiveAuthorJobForIssue(ctx, started.Session.IssueID)
	if err != nil {
		t.Fatalf("live author job: %v", err)
	}
	if !ok || fixJob.ChangeID == nil || *fixJob.ChangeID != started.Change.ID {
		t.Fatalf("fix job = %+v ok=%t, want change %s", fixJob, ok, started.Change.ID)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/reviewer", reportCheckRequest{
		Kind:     string(coordinator.CheckKindReviewer),
		Required: &required,
		Verdict:  string(coordinator.CheckBlocked),
		Details:  "Still needs a fix.",
	}, http.StatusOK, nil)
	liveJobs := liveAuthorJobsForIssue(t, fixture, started.Session.IssueID)
	if len(liveJobs) != 1 || liveJobs[0].ID != fixJob.ID {
		t.Fatalf("live fix jobs = %+v, want only %s", liveJobs, fixJob.ID)
	}

	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-fix",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register fix worker: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, "w-fix", fixJob.ID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark fix running: %v", err)
	}
	fixSession, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-fix",
	})
	if err != nil {
		t.Fatalf("start fix session: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, fixSession.Token, http.MethodPost, "/v1/sessions/"+fixSession.Session.ID+"/ready", readySessionRequest{
		HeadSHA: "head-2",
	}, http.StatusOK, nil)
	resetReviewer, err := fixture.Checks.GetCheck(ctx, started.Session.IssueID, "reviewer")
	if err != nil {
		t.Fatalf("get reset reviewer: %v", err)
	}
	if resetReviewer.Verdict != coordinator.CheckPending || resetReviewer.SourceJobID != nil {
		t.Fatalf("reset reviewer = %+v", resetReviewer)
	}
	resetCI, err := fixture.Checks.GetCheck(ctx, started.Session.IssueID, "ci")
	if err != nil {
		t.Fatalf("get reset ci: %v", err)
	}
	if resetCI.Verdict != coordinator.CheckPending || resetCI.SourceJobID != nil {
		t.Fatalf("reset ci = %+v", resetCI)
	}

	var regression checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/ci", reportCheckRequest{
		Kind:     string(coordinator.CheckKindCI),
		Required: &required,
		Verdict:  string(coordinator.CheckBlocked),
		Details:  "Regression after fix.",
	}, http.StatusOK, &regression)
	if regression.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("regression review state = %q, want changes_requested", regression.ReviewState)
	}
}

func TestReadySchedulesRepoConfiguredCritiqueChecks(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath, headSHA := createCheckConfigExchange(t)
	repointFixtureExchange(t, fixture, exchangePath)
	started := startAuthorSessionForStatusTest(t, fixture, "Configured ready issue")
	if _, err := fixture.Sessions.UpdateChangeHead(context.Background(), started.Change.ID, headSHA); err != nil {
		t.Fatalf("record change head before ready: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, http.StatusOK, nil)

	assertAPICheck(t, fixture, started.Session.IssueID, "unit", coordinator.CheckKindCI, coordinator.CheckPending)
	assertAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer, coordinator.CheckPending)
	assertAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier, coordinator.CheckPending)
	assertAPILiveJobs(t, fixture, started.Session.IssueID, map[flowworker.JobRole]int{
		flowworker.RoleCI:       1,
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})
}

func TestBoardRoutesPendingHumanReviewToNeedsAttention(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Human review board"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO changes (id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at)
VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`,
		"ch-human-review",
		issue.ID,
		"issue/"+issue.ID,
		strings.Repeat("1", 40),
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert ready change: %v", err)
	}
	required := true
	for _, input := range []coordinator.ReportCheckInput{
		{IssueID: issue.ID, Name: "reviewer", Kind: coordinator.CheckKindReviewer, Required: &required, Verdict: coordinator.CheckSatisfied, Reporter: "reviewer"},
		{IssueID: issue.ID, Name: "human-review", Kind: coordinator.CheckKindHuman, Required: &required, Verdict: coordinator.CheckPending, Reporter: "coordinator"},
		{IssueID: issue.ID, Name: "verifier", Kind: coordinator.CheckKindVerifier, Required: &required, Verdict: coordinator.CheckPending, Reporter: "coordinator"},
	} {
		if _, err := fixture.Checks.ReportCheck(ctx, input); err != nil {
			t.Fatalf("report %s check: %v", input.Name, err)
		}
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.InProgress) != 0 {
		t.Fatalf("in_progress board = %+v, want empty while human review is pending", board.Board.InProgress)
	}
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != issue.ID {
		t.Fatalf("needs_attention board = %+v, want %s", board.Board.NeedsAttention, issue.ID)
	}
	if board.LaneStates[issue.ID] != coordinator.LaneStateInReview || board.WaitReasons[issue.ID] != coordinator.WaitReasonHumanReview {
		t.Fatalf("lane state/wait reason = %q/%q, want in_review/human_review", board.LaneStates[issue.ID], board.WaitReasons[issue.ID])
	}
	card := board.IssueCards[issue.ID]
	if !card.RequiredChecks.PendingHumanReview || card.BlockingReason != "human review pending" || card.PrimaryAction != "review" {
		t.Fatalf("card = %+v, want pending human review action", card)
	}
}

func TestRunReviewSchedulesDefaultChecksForLegacyReadyChange(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Legacy zero-check issue")
	requiresHuman := false
	if _, err := fixture.Issues.EditIssue(ctx, started.Session.IssueID, coordinator.EditIssueInput{RequiresHumanReview: &requiresHuman}); err != nil {
		t.Fatalf("disable human review: %v", err)
	}
	exchangePath, headSHA := createMergeExchange(t, started.Change.Branch)
	repointFixtureExchange(t, fixture, exchangePath)
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, headSHA); err != nil {
		t.Fatalf("record change head before ready: %v", err)
	}
	if _, err := fixture.Sessions.ReadyAuthorSession(ctx, started.Session.ID); err != nil {
		t.Fatalf("mark session ready: %v", err)
	}

	existing, err := fixture.Checks.ListChecks(ctx, started.Session.IssueID)
	if err != nil {
		t.Fatalf("list existing checks: %v", err)
	}
	if len(existing) != 0 {
		t.Fatalf("existing checks = %+v, want none before manual review run", existing)
	}

	var response reviewRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/review/run", nil, http.StatusOK, &response)
	if response.Change.ID != started.Change.ID {
		t.Fatalf("response change = %+v, want %s", response.Change, started.Change.ID)
	}
	if response.Scheduled.ChecksCreated != 2 || response.Scheduled.JobsEnqueued != 1 {
		t.Fatalf("scheduled = %+v, want two checks and one reviewer job", response.Scheduled)
	}
	if response.ReviewState != coordinator.ReviewInReview {
		t.Fatalf("review state = %q, want in_review", response.ReviewState)
	}

	assertAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer, coordinator.CheckPending)
	assertAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier, coordinator.CheckPending)
	assertAPILiveJobs(t, fixture, started.Session.IssueID, map[flowworker.JobRole]int{
		flowworker.RoleReviewer: 1,
		flowworker.RoleVerifier: 0,
	})
}

func TestReadyPreflightsInvalidCheckConfigBeforeFinishingSession(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath, headSHA := createInvalidCheckConfigExchange(t)
	repointFixtureExchange(t, fixture, exchangePath)
	started := startAuthorSessionForStatusTest(t, fixture, "Invalid check config ready issue")

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, http.StatusBadRequest, nil)

	session, err := fixture.Sessions.GetSession(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.RuntimeState == coordinator.SessionFinished {
		t.Fatalf("session finished despite check config preflight failure: %+v", session)
	}
	if _, err := fixture.Credentials.Authenticate(context.Background(), started.Token); err != nil {
		t.Fatalf("session token revoked after preflight failure: %v", err)
	}
}

func TestWorkerCheckReportRejectsSourceJobFromStaleHead(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Stale check job issue")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-2"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-ci",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &started.Session.IssueID,
		ChangeID:       &started.Change.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"check_name": "unit",
			"change_id":  started.Change.ID,
			"head_sha":   "head-1",
		},
	})
	if err != nil {
		t.Fatalf("enqueue stale job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-ci",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim stale job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	sourceJobID := job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/unit", reportCheckRequest{
		Kind:        string(coordinator.CheckKindCI),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
		ExitCode:    intPointer(0),
	}, http.StatusForbidden, nil)
}

func TestWorkerCheckReportRejectsSourceJobMissingCheckMetadata(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Missing check metadata issue")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-1"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-ci-metadata",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &started.Session.IssueID,
		ChangeID:       &started.Change.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue job without metadata: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-ci-metadata",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	sourceJobID := job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/unit", reportCheckRequest{
		Kind:        string(coordinator.CheckKindCI),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
		ExitCode:    intPointer(0),
	}, http.StatusForbidden, nil)
}

func TestWorkerReviewerJobCanReportReviewerCheck(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Reviewer report issue")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-1"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	reviewer := startLiveCheckJobForIssue(t, fixture, "reviewer-token", "w-review-report", started.Session.IssueID, started.Change.ID, "head-1", "reviewer", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	sourceJobID := reviewer.Job.ID
	leaseID := reviewer.Lease.ID
	var response checkResponse
	doJSONRequestAs(t, fixture.Server, "reviewer-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/reviewer", reportCheckRequest{
		Kind:        string(coordinator.CheckKindReviewer),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
		ExitCode:    intPointer(0),
	}, http.StatusOK, &response)
	if response.Check.Kind != coordinator.CheckKindReviewer || response.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("reviewer report response = %+v", response)
	}
}

func TestReadyWithUnchangedHeadDoesNotResetBlockedChecks(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	// Synthetic head SHAs: model a project with no exchange-configured checks.
	repointFixtureExchange(t, fixture, "")
	started := startAuthorSessionForStatusTest(t, fixture, "Same head fix loop issue")
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: "head-1",
	}, http.StatusOK, nil)

	required := true
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/reviewer", reportCheckRequest{
		Kind:     string(coordinator.CheckKindReviewer),
		Required: &required,
		Verdict:  string(coordinator.CheckBlocked),
		Details:  "Needs a fix.",
	}, http.StatusOK, nil)
	fixJob, ok, err := fixture.Workers.LiveAuthorJobForIssue(ctx, started.Session.IssueID)
	if err != nil {
		t.Fatalf("live author job: %v", err)
	}
	if !ok {
		t.Fatal("expected fix author job")
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-same-head",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register fix worker: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, "w-same-head", fixJob.ID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark fix running: %v", err)
	}
	fixSession, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-same-head",
	})
	if err != nil {
		t.Fatalf("start fix session: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, fixSession.Token, http.MethodPost, "/v1/sessions/"+fixSession.Session.ID+"/ready", readySessionRequest{
		HeadSHA: "head-1",
	}, http.StatusOK, nil)
	check, err := fixture.Checks.GetCheck(ctx, started.Session.IssueID, "reviewer")
	if err != nil {
		t.Fatalf("get reviewer: %v", err)
	}
	if check.Verdict != coordinator.CheckBlocked {
		t.Fatalf("reviewer verdict = %q, want blocked for unchanged head", check.Verdict)
	}
}

func validHandoffContent(goal string) string {
	return handoff.RenderTemplate(handoff.TemplateInput{
		CurrentGoal:           goal,
		CompletedWork:         "Wrote the handler.",
		RemainingWork:         "Run the tests.",
		TestsRun:              "go test ./... passed.",
		FailedApproaches:      "None.",
		ImportantFiles:        "internal/api/server.go",
		NextRecommendedAction: "Review the change.",
	})
}

func TestPutHandoffSnapshotRecordsHandoffForOwningSession(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Eager handoff issue")

	// A valid handoff from the owning session is recorded as present and valid.
	content := validHandoffContent("Ship eager handoff sync.")
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPut,
		"/v1/changes/"+started.Change.ID+"/handoff",
		putHandoffRequest{Content: content, HeadSHA: "eager-head-1"},
		http.StatusOK, nil)

	snapshot, err := fixture.Reconciler.GetHandoffSnapshot(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get handoff snapshot: %v", err)
	}
	if !snapshot.Present || !snapshot.Valid {
		t.Fatalf("snapshot present/valid = %t/%t, want true/true: %+v", snapshot.Present, snapshot.Valid, snapshot)
	}
	if snapshot.Summary != "Ship eager handoff sync." {
		t.Fatalf("snapshot summary = %q, want %q", snapshot.Summary, "Ship eager handoff sync.")
	}
	if snapshot.HeadSHA != "eager-head-1" {
		t.Fatalf("snapshot head_sha = %q, want %q", snapshot.HeadSHA, "eager-head-1")
	}

	// An invalid handoff (missing required sections) is recorded, not rejected:
	// the snapshot mirrors reconcile semantics and reports valid=false.
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPut,
		"/v1/changes/"+started.Change.ID+"/handoff",
		putHandoffRequest{Content: "# Flow Handoff\n\nincomplete", HeadSHA: "eager-head-2"},
		http.StatusOK, nil)
	invalid, err := fixture.Reconciler.GetHandoffSnapshot(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get invalid handoff snapshot: %v", err)
	}
	if !invalid.Present || invalid.Valid {
		t.Fatalf("invalid snapshot present/valid = %t/%t, want true/false: %+v", invalid.Present, invalid.Valid, invalid)
	}
	if invalid.HeadSHA != "eager-head-2" {
		t.Fatalf("invalid snapshot head_sha = %q, want %q", invalid.HeadSHA, "eager-head-2")
	}
}

func TestPutHandoffSnapshotRejectsSessionForDifferentChange(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	owning := startAuthorSessionForStatusTest(t, fixture, "Handoff owner issue")
	other := startAuthorSessionForStatusTestWithWorker(t, fixture, "Handoff other issue", "w-other")

	// The owning session's token must not be able to write the other change's
	// handoff: the PUT is scoped to the change the session owns.
	doJSONRequestAs(t, fixture.Server, owning.Token, http.MethodPut,
		"/v1/changes/"+other.Change.ID+"/handoff",
		putHandoffRequest{Content: validHandoffContent("Cross-change write."), HeadSHA: "x"},
		http.StatusForbidden, nil)

	// The rejected request must have no side effect: the auth check runs before
	// the upsert, so the other change's handoff snapshot is never written.
	if _, err := fixture.Reconciler.GetHandoffSnapshot(ctx, other.Change.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected cross-change PUT wrote a snapshot: err = %v", err)
	}
}

func TestGetHandoffSnapshotReturnsBodyForOwningSessionAndOwner(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Get handoff issue")

	// No snapshot yet: GET is a 404 the session builder treats as "no prior
	// handoff" rather than an error.
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodGet,
		"/v1/changes/"+started.Change.ID+"/handoff", nil, http.StatusNotFound, nil)

	content := validHandoffContent("Resume the migration.")
	if err := fixture.Reconciler.UpsertHandoffSnapshot(ctx, started.Change.ID, "head-9", content); err != nil {
		t.Fatalf("seed handoff snapshot: %v", err)
	}

	// The owning session reads the full body for prompt injection.
	var got handoffResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodGet,
		"/v1/changes/"+started.Change.ID+"/handoff", nil, http.StatusOK, &got)
	if got.Content != content || !got.Present || !got.Valid || got.HeadSHA != "head-9" {
		t.Fatalf("session get handoff = %+v, want full content at head-9", got)
	}

	// Owners read any change's handoff.
	var ownerGot handoffResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet,
		"/v1/changes/"+started.Change.ID+"/handoff", nil, http.StatusOK, &ownerGot)
	if ownerGot.Content != content {
		t.Fatalf("owner get handoff content = %q, want %q", ownerGot.Content, content)
	}

	// A verifier worker reads the handoff for prompt injection by proving its
	// live lease on the change.
	claimed := startLiveCheckJobForIssue(t, fixture, "worker-token-vfy", "w-vfy",
		started.Session.IssueID, started.Change.ID, "head-9", "verifier-check",
		flowworker.RoleVerifier, flowworker.BucketEphemeral)
	var workerGot handoffResponse
	doJSONRequestAs(t, fixture.Server, "worker-token-vfy", http.MethodGet,
		"/v1/changes/"+started.Change.ID+"/handoff?lease_id="+claimed.Lease.ID, nil, http.StatusOK, &workerGot)
	if workerGot.Content != content {
		t.Fatalf("worker get handoff content = %q, want %q", workerGot.Content, content)
	}
	// Without the lease the worker is forbidden.
	doJSONRequestAs(t, fixture.Server, "worker-token-vfy", http.MethodGet,
		"/v1/changes/"+started.Change.ID+"/handoff", nil, http.StatusForbidden, nil)
}

func TestAutoMergeApprovedIssueMergesExchangeBase(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Auto merge issue")
	autoMerge := true
	requiresHuman := false
	if _, err := fixture.Issues.EditIssue(ctx, started.Session.IssueID, coordinator.EditIssueInput{
		AutoMerge:           &autoMerge,
		RequiresHumanReview: &requiresHuman,
	}); err != nil {
		t.Fatalf("enable auto merge: %v", err)
	}
	exchangePath, headSHA := createMergeExchange(t, started.Change.Branch)
	repointFixtureExchange(t, fixture, exchangePath)
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, http.StatusOK, nil)

	var response checkResponse
	_ = satisfyAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer)
	response = satisfyAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier)
	if response.ReviewState != coordinator.ReviewMerged {
		t.Fatalf("review state after auto merge = %q, want merged", response.ReviewState)
	}
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get merged change: %v", err)
	}
	if change.MergedAt == nil {
		t.Fatalf("change merged_at is nil: %+v", change)
	}
	issue, err := fixture.Issues.GetIssue(ctx, started.Session.IssueID)
	if err != nil {
		t.Fatalf("get merged issue: %v", err)
	}
	if issue.ScheduleState != coordinator.ScheduleClosed {
		t.Fatalf("issue schedule state = %q, want closed", issue.ScheduleState)
	}
	app, present, err := flowgit.ReadTextFileAtRef(ctx, exchangePath, "refs/heads/main", "app.go")
	if err != nil {
		t.Fatalf("read merged app: %v", err)
	}
	if !present || app != "package app\n\nconst Value = 1\n" {
		t.Fatalf("merged app present=%t content=%q", present, app)
	}
	if _, present, err := flowgit.ReadTextFileAtRef(ctx, exchangePath, "refs/heads/main", ".flow/session/state.json"); err != nil {
		t.Fatalf("read session state: %v", err)
	} else if present {
		t.Fatal(".flow/session file was included in auto-merged base")
	}
}

func TestAutoMergeConflictEnqueuesAuthorFixAndRerunsChecks(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Auto merge conflict issue")
	autoMerge := true
	requiresHuman := false
	if _, err := fixture.Issues.EditIssue(ctx, started.Session.IssueID, coordinator.EditIssueInput{
		AutoMerge:           &autoMerge,
		RequiresHumanReview: &requiresHuman,
	}); err != nil {
		t.Fatalf("enable auto merge: %v", err)
	}
	repoPath, exchangePath, headSHA := createConflictingMergeExchange(t, started.Change.Branch)
	repointFixtureExchange(t, fixture, exchangePath)
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, http.StatusOK, nil)

	_ = satisfyAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer)
	response := satisfyAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier)
	if response.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("review state after auto-merge conflict = %q, want changes_requested", response.ReviewState)
	}
	autoMergeCheck, err := fixture.Checks.GetCheck(ctx, started.Session.IssueID, coordinator.AutoMergeCheckName)
	if err != nil {
		t.Fatalf("get auto-merge check: %v", err)
	}
	if autoMergeCheck.Verdict != coordinator.CheckBlocked || !autoMergeCheck.Required || !strings.Contains(autoMergeCheck.Details, "CONFLICT") {
		t.Fatalf("auto-merge check = %+v", autoMergeCheck)
	}
	fixJobs := liveAuthorJobsForIssue(t, fixture, started.Session.IssueID)
	if len(fixJobs) != 1 {
		t.Fatalf("live author jobs = %+v, want one fix job", fixJobs)
	}
	if payloadString(fixJobs[0].Payload, "branch") != started.Change.Branch {
		t.Fatalf("fix job payload branch = %+v, want %s", fixJobs[0].Payload, started.Change.Branch)
	}

	claimed := claimSpecificJob(t, fixture, "w-local", fixJobs[0].ID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark fix running: %v", err)
	}
	fixSession, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start fix session: %v", err)
	}

	runAPIGit(t, repoPath, "checkout", started.Change.Branch)
	runAPIGit(t, repoPath, "merge", "-s", "ours", "main", "-m", "merge main")
	resolvedHead := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, started.Change.Branch+":"+started.Change.Branch)
	doJSONRequestAs(t, fixture.Server, fixSession.Token, http.MethodPost, "/v1/sessions/"+fixSession.Session.ID+"/ready", readySessionRequest{
		HeadSHA: resolvedHead,
	}, http.StatusOK, nil)

	autoMergeCheck, err = fixture.Checks.GetCheck(ctx, started.Session.IssueID, coordinator.AutoMergeCheckName)
	if err != nil {
		t.Fatalf("get reset auto-merge check: %v", err)
	}
	if autoMergeCheck.Required || autoMergeCheck.Verdict != coordinator.CheckSkipped {
		t.Fatalf("auto-merge check after author fix = %+v, want optional skipped", autoMergeCheck)
	}
	assertAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer, coordinator.CheckPending)
	assertAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier, coordinator.CheckPending)

	_ = satisfyAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer)
	response = satisfyAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier)
	if response.ReviewState != coordinator.ReviewMerged {
		t.Fatalf("review state after resolved auto-merge = %q, want merged", response.ReviewState)
	}
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get merged change: %v", err)
	}
	if change.MergedAt == nil {
		t.Fatalf("change merged_at is nil after resolved auto-merge: %+v", change)
	}
	merged, present, err := flowgit.ReadTextFileAtRef(ctx, exchangePath, "refs/heads/main", "conflict.txt")
	if err != nil {
		t.Fatalf("read merged conflict file: %v", err)
	}
	if !present || merged != "issue branch\n" {
		t.Fatalf("merged conflict file present=%t content=%q", present, merged)
	}
}

func TestAutoMergeFollowUpFailureAfterVerifierCheckDoesNotFailCheckReport(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Auto merge follow-up failure issue")
	autoMerge := true
	requiresHuman := false
	if _, err := fixture.Issues.EditIssue(ctx, started.Session.IssueID, coordinator.EditIssueInput{
		AutoMerge:           &autoMerge,
		RequiresHumanReview: &requiresHuman,
	}); err != nil {
		t.Fatalf("enable auto merge: %v", err)
	}
	exchangePath, headSHA := createMergeExchange(t, started.Change.Branch)
	repointFixtureExchange(t, fixture, exchangePath)
	// Reject pushes so the squash merge succeeds but the follow-up push fails
	// with a non-conflict error; conflicts take the author-fix recovery path.
	hookPath := filepath.Join(exchangePath, "hooks", "pre-receive")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho rejected by test hook >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, http.StatusOK, nil)

	_ = satisfyAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer)

	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   "verifier-merge-token",
		Scope:   coordinator.TokenScopeWorker,
		Subject: "w-verifier-merge",
	}); err != nil {
		t.Fatalf("store verifier token: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-verifier-merge",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true", "worker_id": "w-verifier-merge"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register verifier worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &started.Session.IssueID,
		ChangeID:       &started.Change.ID,
		Role:           flowworker.RoleVerifier,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Priority:       9,
		RunsOn:         map[string]string{"worker_id": "w-verifier-merge"},
		Payload: map[string]any{
			"change_id":  started.Change.ID,
			"check_name": "verifier",
			"head_sha":   headSHA,
		},
	})
	if err != nil {
		t.Fatalf("enqueue verifier job: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, "w-verifier-merge", job.ID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark verifier running: %v", err)
	}

	required := true
	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	var response checkResponse
	doJSONRequestAs(t, fixture.Server, "verifier-merge-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/verifier", reportCheckRequest{
		Kind:        string(coordinator.CheckKindVerifier),
		Required:    &required,
		Verdict:     string(coordinator.CheckSatisfied),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	}, http.StatusOK, &response)
	if response.Check.Verdict != coordinator.CheckSatisfied || response.ReviewState != coordinator.ReviewApproved {
		t.Fatalf("check response = %+v, want satisfied approved", response)
	}
	if len(response.FollowUpFailures) != 1 ||
		response.FollowUpFailures[0].EventKind != "auto_merge" ||
		!strings.Contains(response.FollowUpFailures[0].Details, "push merged base branch") {
		t.Fatalf("follow-up failures = %+v, want auto_merge push failure details", response.FollowUpFailures)
	}

	check, err := fixture.Checks.GetCheck(ctx, started.Session.IssueID, "verifier")
	if err != nil {
		t.Fatalf("get verifier check: %v", err)
	}
	if check.Verdict != coordinator.CheckSatisfied || check.SourceJobID == nil || *check.SourceJobID != job.ID {
		t.Fatalf("verifier check = %+v, want satisfied with source job", check)
	}

	var verifierTransitions int
	if err := fixture.DB.QueryRow(`
SELECT COUNT(*)
FROM transitions
WHERE issue_id = ?
	AND event_kind = 'check_reported'
	AND payload_json LIKE '%"name":"verifier"%'`, started.Session.IssueID).Scan(&verifierTransitions); err != nil {
		t.Fatalf("count verifier transitions: %v", err)
	}
	if verifierTransitions != 1 {
		t.Fatalf("verifier check transitions = %d, want 1", verifierTransitions)
	}
	var mergeGuard string
	var mergePhase string
	if err := fixture.DB.QueryRow(`
SELECT guard_result, to_phase
FROM transitions
WHERE issue_id = ?
	AND event_kind = 'auto_merge'
ORDER BY seq DESC
LIMIT 1`, started.Session.IssueID).Scan(&mergeGuard, &mergePhase); err != nil {
		t.Fatalf("load auto_merge transition: %v", err)
	}
	if !strings.Contains(mergeGuard, "failed:") || !strings.Contains(mergeGuard, "push merged base branch") || mergePhase != string(coordinator.PhaseApproved) {
		t.Fatalf("auto_merge transition guard=%q to_phase=%q, want failed approved", mergeGuard, mergePhase)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != started.Session.IssueID {
		t.Fatalf("needs_attention board after failed auto-merge = %+v", board.Board)
	}
	if board.LaneStates[started.Session.IssueID] != coordinator.LaneStateReadyToMerge {
		t.Fatalf("lane state = %q, want ready_to_merge", board.LaneStates[started.Session.IssueID])
	}
}

func TestManualMergeIssueAPIRequiresOwnerAndMerges(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started, exchangePath := readyApprovedMergeChange(t, fixture, "Manual issue merge")

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.NeedsAttention) != 1 || board.Board.NeedsAttention[0].ID != started.Session.IssueID {
		t.Fatalf("needs_attention board = %+v", board.Board.NeedsAttention)
	}
	if board.LaneStates[started.Session.IssueID] != coordinator.LaneStateReadyToMerge {
		t.Fatalf("lane state = %q, want ready_to_merge", board.LaneStates[started.Session.IssueID])
	}
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}
	card, ok := board.IssueCards[started.Session.IssueID]
	if !ok {
		t.Fatalf("ready_to_merge card missing for %s: %+v", started.Session.IssueID, board.IssueCards)
	}
	if card.DiffStats == nil || card.DiffStats.HeadSHA != change.HeadSHA || card.DiffStats.TotalFiles != 1 || card.DiffStats.Additions != 3 || card.DiffStats.Deletions != 0 || card.DiffUnavailableReason != "" {
		t.Fatalf("ready_to_merge diff card = %+v", card)
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/merge", map[string]string{}, http.StatusForbidden, nil)

	var response mergeResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/merge", map[string]string{}, http.StatusOK, &response)
	if response.Merge.Issue.ScheduleState != coordinator.ScheduleClosed || response.Merge.Change.MergedAt == nil || response.Merge.MergeSHA == "" {
		t.Fatalf("merge response = %+v", response.Merge)
	}
	if _, present, err := flowgit.ReadTextFileAtRef(ctx, exchangePath, "refs/heads/main", ".flow/session/state.json"); err != nil {
		t.Fatalf("read session state: %v", err)
	} else if present {
		t.Fatal(".flow/session file was included in manually merged base")
	}
}

func TestManualMergeChangeAPI(t *testing.T) {
	fixture := newTestFixture(t)
	started, _ := readyApprovedMergeChange(t, fixture, "Manual change merge")

	var response mergeResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/changes/"+started.Change.ID+"/merge", map[string]string{}, http.StatusOK, &response)
	if response.Merge.Change.ID != started.Change.ID || response.Merge.Issue.ScheduleState != coordinator.ScheduleClosed {
		t.Fatalf("merge response = %+v", response.Merge)
	}
}

func TestChangeDetailReadModelIncludesReviewState(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started, _ := readyApprovedMergeChange(t, fixture, "Change detail")
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/changes/"+change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: change.HeadSHA,
		FilePath:        "app.go",
		Line:            1,
		Context:         "const Value = 1",
		Body:            "Check this value.",
	}, http.StatusCreated, nil)

	var response changeResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/changes/"+change.ID, nil, http.StatusOK, &response)
	if response.Change.ID != change.ID || response.Issue.ID != started.Session.IssueID {
		t.Fatalf("change response identity = %+v", response)
	}
	if response.ReviewState != coordinator.ReviewApproved || !response.CanMerge || response.MergeBlockedReason != "" {
		t.Fatalf("merge state = review:%q canMerge:%t reason:%q", response.ReviewState, response.CanMerge, response.MergeBlockedReason)
	}
	if len(response.Checks) != 2 || response.RequiredChecks.Total != 2 || response.RequiredChecks.Satisfied != 2 {
		t.Fatalf("check summary = checks:%+v required:%+v", response.Checks, response.RequiredChecks)
	}
	if len(response.Threads) != 1 || response.Threads[0].FilePath != "app.go" || len(response.Threads[0].Comments) != 1 {
		t.Fatalf("threads = %+v", response.Threads)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/changes/"+change.ID, nil, http.StatusForbidden, nil)
}

func TestWebReadModelsSurviveCoordinatorRestart(t *testing.T) {
	dataDir := t.TempDir()
	registry, bundles, _, global := newTestRegistryInDir(t, dataDir, "api")
	fixture := fixtureFromRegistry(t, registry, bundles[0], dataDir, global, true)
	ctx := context.Background()
	started, _ := readyApprovedMergeChange(t, fixture, "Restarted web issue")
	feedback := startAuthorSessionForStatusTestWithWorker(t, fixture, "Restarted feedback issue", "w-restart-feedback")
	if _, err := fixture.Sessions.UpdateSessionState(ctx, feedback.Session.ID, coordinator.SessionWaiting); err != nil {
		t.Fatalf("mark feedback waiting: %v", err)
	}
	if _, err := fixture.Status.WriteSessionStatus(ctx, feedback.Session.ID, "Waiting after restart", "author", coordinator.StatusKindNote); err != nil {
		t.Fatalf("write feedback status: %v", err)
	}
	triage, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{
		Title:              "Restarted triage issue",
		CreatedBy:          coordinator.ActorAgent,
		CreatedBySessionID: &started.Session.ID,
		TriageState:        coordinator.TriagePending,
	})
	if err != nil {
		t.Fatalf("create triage issue: %v", err)
	}
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}
	tag, err := fixture.Issues.CreateTag(ctx, coordinator.CreateTagInput{Slug: "restart", Name: "Restart"})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := fixture.Issues.TagIssue(ctx, started.Session.IssueID, tag.ID, coordinator.ActorHuman); err != nil {
		t.Fatalf("tag issue: %v", err)
	}
	if _, err := fixture.Threads.CreateThread(ctx, coordinator.CreateThreadInput{
		ChangeID:        change.ID,
		AnchorCommitSHA: change.HeadSHA,
		FilePath:        "app.go",
		Line:            1,
		Context:         "const Value = 1",
		Body:            "Review note",
		Actor:           "reviewer",
	}); err != nil {
		t.Fatalf("create review thread: %v", err)
	}
	if err := fixture.Registry.Close(); err != nil {
		t.Fatalf("close original registry bundles: %v", err)
	}

	restarted := reopenTestFixture(t, dataDir)
	var board boardResponse
	doJSONRequestAs(t, restarted.Server, "owner-token", http.MethodGet, restarted.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.NeedsAttention) != 2 {
		t.Fatalf("needs_attention after restart = %+v", board.Board.NeedsAttention)
	}
	if board.LaneStates[started.Session.IssueID] != coordinator.LaneStateReadyToMerge {
		t.Fatalf("ready_to_merge lane state after restart = %q", board.LaneStates[started.Session.IssueID])
	}
	if board.LaneStates[feedback.Session.IssueID] != coordinator.LaneStateInProgress {
		t.Fatalf("feedback lane state after restart = %q, want in_progress", board.LaneStates[feedback.Session.IssueID])
	}
	if board.WaitReasons[feedback.Session.IssueID] != coordinator.WaitReasonQuestion {
		t.Fatalf("feedback wait reason after restart = %q, want question", board.WaitReasons[feedback.Session.IssueID])
	}
	if board.LaneStates[triage.ID] != coordinator.LaneStateTriage {
		t.Fatalf("triage lane state after restart = %q", board.LaneStates[triage.ID])
	}
	var triaged *coordinator.Issue
	for i := range board.Board.Backlog {
		if board.Board.Backlog[i].ID == triage.ID {
			triaged = &board.Board.Backlog[i]
		}
	}
	if triaged == nil {
		t.Fatalf("triage issue missing from backlog after restart = %+v", board.Board.Backlog)
	}
	if triaged.CreatedBySessionID == nil || *triaged.CreatedBySessionID != started.Session.ID {
		t.Fatalf("triage provenance after restart = %+v", triaged.CreatedBySessionID)
	}
	card, ok := board.IssueCards[started.Session.IssueID]
	if !ok {
		t.Fatalf("issue cards after restart = %+v", board.IssueCards)
	}
	if card.ReviewState != coordinator.ReviewApproved || card.RequiredChecks.Satisfied != 2 || len(card.Tags) != 1 || card.Tags[0].Slug != "restart" {
		t.Fatalf("card after restart = %+v", card)
	}
	feedbackCard, ok := board.IssueCards[feedback.Session.IssueID]
	if !ok {
		t.Fatalf("feedback card missing after restart = %+v", board.IssueCards)
	}
	if feedbackCard.ActiveSession == nil || feedbackCard.ActiveSession.ID != feedback.Session.ID || feedbackCard.ActiveSession.State != coordinator.SessionWaiting {
		t.Fatalf("feedback card active session after restart = %+v", feedbackCard.ActiveSession)
	}
	if feedbackCard.LatestStatus == nil || feedbackCard.LatestStatus.Message != "Waiting after restart" {
		t.Fatalf("feedback card status after restart = %+v", feedbackCard.LatestStatus)
	}
	triageCard, ok := board.IssueCards[triage.ID]
	if !ok {
		t.Fatalf("triage card missing after restart = %+v", board.IssueCards)
	}
	if triageCard.PrimaryAction != "triage" {
		t.Fatalf("triage card after restart = %+v", triageCard)
	}

	var issue issueResponse
	doJSONRequestAs(t, restarted.Server, "owner-token", http.MethodGet, "/v1/issues/"+started.Session.IssueID, nil, http.StatusOK, &issue)
	if issue.Detail == nil || issue.Detail.ReadyChange == nil || issue.Detail.ReadyChange.ID != change.ID {
		t.Fatalf("issue detail ready change after restart = %+v", issue.Detail)
	}
	if len(issue.Detail.Tags) != 1 || issue.Detail.Tags[0].Slug != "restart" || issue.Detail.RequiredChecks.Satisfied != 2 {
		t.Fatalf("issue detail after restart = %+v", issue.Detail)
	}

	var changeDetail changeResponse
	doJSONRequestAs(t, restarted.Server, "owner-token", http.MethodGet, "/v1/changes/"+change.ID, nil, http.StatusOK, &changeDetail)
	if changeDetail.Change.ID != change.ID || changeDetail.Issue.ID != started.Session.IssueID {
		t.Fatalf("change detail identity after restart = %+v", changeDetail)
	}
	if changeDetail.ReviewState != coordinator.ReviewApproved || !changeDetail.CanMerge || changeDetail.RequiredChecks.Satisfied != 2 {
		t.Fatalf("change detail review state after restart = %+v", changeDetail)
	}
	if len(changeDetail.Threads) != 1 || changeDetail.Threads[0].FilePath != "app.go" || len(changeDetail.Threads[0].Comments) != 1 {
		t.Fatalf("change threads after restart = %+v", changeDetail.Threads)
	}
}

func TestChangeDiffReadModelUsesExchangeRemote(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started, _ := readyApprovedMergeChange(t, fixture, "Change diff")
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}

	var response changeDiffResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/changes/"+change.ID+"/diff", nil, http.StatusOK, &response)
	if !response.Available || response.ChangeID != change.ID || response.HeadSHA != change.HeadSHA {
		t.Fatalf("diff response = %+v", response)
	}
	if response.TotalFiles != 1 || response.Additions != 3 || response.Deletions != 0 {
		t.Fatalf("diff stats = files:%d additions:%d deletions:%d", response.TotalFiles, response.Additions, response.Deletions)
	}
	var sawApp bool
	for _, file := range response.Files {
		if file.Path == "app.go" && file.Additions == 3 && file.Deletions == 0 && !file.Binary {
			sawApp = true
			if len(file.Hunks) != 1 || len(file.Hunks[0].Lines) != 3 {
				t.Fatalf("app.go hunks = %+v, want one three-line hunk", file.Hunks)
			}
			if file.Hunks[0].Lines[0].Kind != "add" || file.Hunks[0].Lines[0].Text != "package app" {
				t.Fatalf("app.go first hunk line = %+v", file.Hunks[0].Lines[0])
			}
		}
	}
	if !sawApp {
		t.Fatalf("diff files = %+v, missing app.go stats", response.Files)
	}
	for _, file := range response.Files {
		if strings.HasPrefix(file.Path, ".flow/session") {
			t.Fatalf("diff files included merge-excluded path: %+v", response.Files)
		}
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v1/changes/"+change.ID+"/diff", nil, http.StatusForbidden, nil)
}

// TestChangeDiffRemainsNonEmptyAfterMerge reproduces the empty-after-merge bug:
// a squash merge advances the base ref to a commit whose tree equals the branch
// content, so diffing refs/heads/<base>..head becomes empty. The diff must
// instead use the pre-merge base tip (previous_base_sha) recorded in the
// completed merge intent, keeping the change set visible after merge.
func TestChangeDiffRemainsNonEmptyAfterMerge(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started, _ := readyApprovedMergeChange(t, fixture, "Diff after merge")
	change, err := fixture.Sessions.GetChange(ctx, started.Change.ID)
	if err != nil {
		t.Fatalf("get ready change: %v", err)
	}

	var mergeResp mergeResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/merge", map[string]string{}, http.StatusOK, &mergeResp)
	if mergeResp.Merge.Change.MergedAt == nil || mergeResp.Merge.Issue.ScheduleState != coordinator.ScheduleClosed {
		t.Fatalf("merge response = %+v", mergeResp.Merge)
	}

	var response changeDiffResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/changes/"+change.ID+"/diff", nil, http.StatusOK, &response)
	if !response.Available {
		t.Fatalf("diff unavailable after merge: %+v", response)
	}
	if response.TotalFiles != 1 || response.Additions != 3 || response.Deletions != 0 {
		t.Fatalf("diff stats after merge = files:%d additions:%d deletions:%d, want the pre-merge change set", response.TotalFiles, response.Additions, response.Deletions)
	}
	var sawApp bool
	for _, file := range response.Files {
		if file.Path == "app.go" && file.Additions == 3 && file.Deletions == 0 {
			sawApp = true
		}
	}
	if !sawApp {
		t.Fatalf("diff files after merge = %+v, missing app.go", response.Files)
	}
}

func TestChangeDetailMergeEligibilityRequiresHeadAndProject(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	// Clear the project's exchange so merge eligibility reports the missing
	// exchange path (the no-local-exchange branch of the resolver).
	repointFixtureExchange(t, fixture, "")
	started := startAuthorSessionForStatusTest(t, fixture, "Change detail blocked merge")
	requiresHuman := false
	if _, err := fixture.Issues.EditIssue(ctx, started.Session.IssueID, coordinator.EditIssueInput{RequiresHumanReview: &requiresHuman}); err != nil {
		t.Fatalf("disable human review: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{}, http.StatusOK, nil)
	required := true
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+started.Session.IssueID+"/checks/unit", reportCheckRequest{
		Kind:     string(coordinator.CheckKindCI),
		Required: &required,
		Verdict:  string(coordinator.CheckSatisfied),
	}, http.StatusOK, nil)

	var missingHead changeResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/changes/"+started.Change.ID, nil, http.StatusOK, &missingHead)
	if missingHead.ReviewState != coordinator.ReviewApproved || missingHead.CanMerge || missingHead.MergeBlockedReason != "change head sha is required" {
		t.Fatalf("missing head eligibility = review:%q can:%t reason:%q", missingHead.ReviewState, missingHead.CanMerge, missingHead.MergeBlockedReason)
	}
	var missingHeadDiff changeDiffResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/changes/"+started.Change.ID+"/diff", nil, http.StatusOK, &missingHeadDiff)
	if missingHeadDiff.Available || missingHeadDiff.UnavailableReason != "change head sha is not recorded" {
		t.Fatalf("missing head diff = %+v", missingHeadDiff)
	}

	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, strings.Repeat("a", 40)); err != nil {
		t.Fatalf("set change head: %v", err)
	}
	var missingProject changeResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/changes/"+started.Change.ID, nil, http.StatusOK, &missingProject)
	if missingProject.CanMerge || missingProject.MergeBlockedReason != "project exchange path is not local" {
		t.Fatalf("missing project eligibility = can:%t reason:%q", missingProject.CanMerge, missingProject.MergeBlockedReason)
	}
}

func readyApprovedMergeChange(t *testing.T, fixture testFixture, title string) (coordinator.StartAuthorSessionResult, string) {
	t.Helper()

	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, title)
	requiresHuman := false
	if _, err := fixture.Issues.EditIssue(ctx, started.Session.IssueID, coordinator.EditIssueInput{RequiresHumanReview: &requiresHuman}); err != nil {
		t.Fatalf("disable human review: %v", err)
	}
	exchangePath, headSHA := createMergeExchange(t, started.Change.Branch)
	repointFixtureExchange(t, fixture, exchangePath)
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v1/sessions/"+started.Session.ID+"/ready", readySessionRequest{
		HeadSHA: headSHA,
	}, http.StatusOK, nil)
	_ = satisfyAPICheck(t, fixture, started.Session.IssueID, "reviewer", coordinator.CheckKindReviewer)
	_ = satisfyAPICheck(t, fixture, started.Session.IssueID, "verifier", coordinator.CheckKindVerifier)

	return started, exchangePath
}

func TestScheduleBlockedIssueDoesNotEnqueueAuthorJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	blocker, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Blocker"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Blocked"})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}
	if err := fixture.Issues.LinkIssues(ctx, blocker.ID, blocked.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}

	var scheduled issueResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+blocked.ID+"/schedule", scheduleIssueRequest{
		State: string(coordinator.ScheduleUpNext),
	}, http.StatusOK, &scheduled)
	if scheduled.Issue.ScheduleState != coordinator.ScheduleUpNext {
		t.Fatalf("scheduled issue = %+v", scheduled.Issue)
	}
	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none for blocked issue", jobs)
	}
}

func startAuthorSessionForStatusTest(t *testing.T, fixture testFixture, title string) coordinator.StartAuthorSessionResult {
	return startAuthorSessionForStatusTestWithWorker(t, fixture, title, "w-local")
}

func startAuthorSessionForStatusTestWithWorker(t *testing.T, fixture testFixture, title string, workerID string) coordinator.StartAuthorSessionResult {
	t.Helper()

	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: title})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, workerID, ensured.Job.ID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: workerID,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	return started
}

func liveAuthorJobsForIssue(t *testing.T, fixture testFixture, issueID string) []flowworker.Job {
	t.Helper()

	jobs, err := fixture.Workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var live []flowworker.Job
	for _, job := range jobs {
		if job.IssueID == nil || *job.IssueID != issueID || job.Role != flowworker.RoleAuthor {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			live = append(live, job)
		}
	}
	return live
}

func startLiveWorkerJobForIssue(t *testing.T, fixture testFixture, token string, workerID string, issueID string, changeID string, role flowworker.JobRole) flowworker.ClaimedJob {
	t.Helper()

	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   token,
		Scope:   coordinator.TokenScopeWorker,
		Subject: workerID,
	}); err != nil {
		t.Fatalf("store worker token: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true", "worker_id": workerID},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker %s: %v", workerID, err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issueID,
		ChangeID:       &changeID,
		Role:           role,
		CapacityBucket: flowworker.BucketPersistentAgent,
		RunsOn:         map[string]string{"worker_id": workerID},
	})
	if err != nil {
		t.Fatalf("enqueue %s job: %v", role, err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      workerID,
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim %s job: %v", role, err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	running, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("mark %s running: %v", role, err)
	}
	claimed.Job = running
	return claimed
}

func createCheckConfigExchange(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	runAPIGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "README.md", "initial\n")
	runAPIGit(t, repoPath, "add", "README.md")
	runAPIGit(t, repoPath, "commit", "-m", "initial")
	runAPIGit(t, "", "init", "--bare", exchangePath)
	runAPIGit(t, repoPath, "checkout", "-b", "issue/i-0001", "main")
	writeAPIFile(t, repoPath, ".flow/checks/unit.yaml", `
name: unit
kind: ci
entrypoint:
  argv: ["go", "test", "./..."]
`)
	writeAPIFile(t, repoPath, ".flow/checks/reviewer.yaml", `
name: reviewer
kind: reviewer
entrypoint:
  argv: ['codex exec -c "projects.$PWD.trust_level=trusted" "$(flow fetch-prompt)"']
  shell: true
requires: ["agent.harness.codex"]
`)
	writeAPIFile(t, repoPath, ".flow/checks/verifier.yaml", `
name: verifier
kind: verifier
entrypoint:
  argv: ['codex exec -c "projects.$PWD.trust_level=trusted" "$(flow fetch-prompt)"']
  shell: true
requires: ["agent.harness.codex"]
`)
	runAPIGit(t, repoPath, "add", ".flow/checks")
	runAPIGit(t, repoPath, "commit", "-m", "add checks")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, "issue/i-0001:issue/i-0001")

	return exchangePath, headSHA
}

func createInvalidCheckConfigExchange(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	runAPIGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "README.md", "initial\n")
	runAPIGit(t, repoPath, "add", "README.md")
	runAPIGit(t, repoPath, "commit", "-m", "initial")
	runAPIGit(t, "", "init", "--bare", exchangePath)
	runAPIGit(t, repoPath, "checkout", "-b", "issue/i-0001", "main")
	writeAPIFile(t, repoPath, ".flow/checks/bad.yaml", `
name: bad
kind: ci
`)
	runAPIGit(t, repoPath, "add", ".flow/checks")
	runAPIGit(t, repoPath, "commit", "-m", "add bad check")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, "issue/i-0001:issue/i-0001")

	return exchangePath, headSHA
}

func createMergeExchange(t *testing.T, branch string) (string, string) {
	t.Helper()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	runAPIGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "README.md", "initial\n")
	runAPIGit(t, repoPath, "add", "README.md")
	runAPIGit(t, repoPath, "commit", "-m", "initial")
	runAPIGit(t, "", "init", "--bare", exchangePath)
	runAPIGit(t, repoPath, "push", exchangePath, "main:main")
	runAPIGit(t, repoPath, "checkout", "-b", branch, "main")
	writeAPIFile(t, repoPath, "app.go", "package app\n\nconst Value = 1\n")
	writeAPIFile(t, repoPath, ".flow/session/state.json", "{}\n")
	runAPIGit(t, repoPath, "add", "app.go", ".flow/session/state.json")
	runAPIGit(t, repoPath, "commit", "-m", "add app")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, branch+":"+branch)

	return exchangePath, headSHA
}

func createConflictingMergeExchange(t *testing.T, branch string) (string, string, string) {
	t.Helper()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	runAPIGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "conflict.txt", "initial\n")
	runAPIGit(t, repoPath, "add", "conflict.txt")
	runAPIGit(t, repoPath, "commit", "-m", "initial")
	runAPIGit(t, "", "init", "--bare", exchangePath)
	runAPIGit(t, repoPath, "push", exchangePath, "main:main")

	runAPIGit(t, repoPath, "checkout", "-b", branch, "main")
	writeAPIFile(t, repoPath, "conflict.txt", "issue branch\n")
	runAPIGit(t, repoPath, "add", "conflict.txt")
	runAPIGit(t, repoPath, "commit", "-m", "change issue branch")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, branch+":"+branch)

	runAPIGit(t, repoPath, "checkout", "main")
	writeAPIFile(t, repoPath, "conflict.txt", "advanced base\n")
	runAPIGit(t, repoPath, "add", "conflict.txt")
	runAPIGit(t, repoPath, "commit", "-m", "advance base")
	runAPIGit(t, repoPath, "push", exchangePath, "main:main")

	return repoPath, exchangePath, headSHA
}

// repointFixtureExchange rebinds the fixture's single project to a
// test-created exchange. Exchange-dependent services (merges, check configs,
// and the lifecycle engine that wraps them) now read the exchange path off
// the project value they were constructed with, so they are rebuilt in place
// on the bundle the server resolves per request.
func repointFixtureExchange(t *testing.T, fixture testFixture, exchangePath string) {
	t.Helper()

	bundle := fixture.Bundle
	project := bundle.Project
	project.ExchangeURL = exchangePath
	project.ExchangePath = exchangePath
	bundle.Project = project

	// Keep the global registry row in sync so the by-exchange drain route can
	// resolve this project from its exchange path.
	if _, err := fixture.GlobalDB.ExecContext(context.Background(), `
UPDATE projects
SET exchange_url = ?, exchange_path = ?
WHERE id = ?`, exchangePath, exchangePath, project.ID); err != nil {
		t.Fatalf("repoint project exchange row: %v", err)
	}

	db := bundle.Store.DB()
	bundle.Merges = coordinator.NewMergeService(db, bundle.Issues, bundle.Sessions, project)
	bundle.CheckConfigs = coordinator.NewCheckConfigServiceWithOptions(db, bundle.Checks, bundle.Queue, bundle.Threads, project, coordinator.CheckConfigServiceOptions{})
	bundle.GitEventConsumer = coordinator.NewGitEventConsumer(db, project)
	bundle.Engine = lifecycle.NewEngine(db, lifecycle.NewEffects(
		bundle.Issues,
		bundle.Checks,
		bundle.CheckConfigs,
		bundle.Sessions,
		bundle.Merges,
		bundle.Threads,
		bundle.Status,
	))
}

func startLiveCheckJobForIssue(t *testing.T, fixture testFixture, token string, workerID string, issueID string, changeID string, headSHA string, checkName string, role flowworker.JobRole, bucket flowworker.CapacityBucket) flowworker.ClaimedJob {
	t.Helper()

	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   token,
		Scope:   coordinator.TokenScopeWorker,
		Subject: workerID,
	}); err != nil {
		t.Fatalf("store worker token: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true", "worker_id": workerID},
		CapacityPersistentAgent: 1,
		CapacityEphemeral:       1,
	}); err != nil {
		t.Fatalf("register worker %s: %v", workerID, err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issueID,
		ChangeID:       &changeID,
		Role:           role,
		CapacityBucket: bucket,
		RunsOn:         map[string]string{"worker_id": workerID},
		Payload: map[string]any{
			"check_name": checkName,
			"change_id":  changeID,
			"head_sha":   headSHA,
		},
	})
	if err != nil {
		t.Fatalf("enqueue %s check job: %v", role, err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      workerID,
		Buckets:       []flowworker.CapacityBucket{bucket},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim %s check job: %v", role, err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	running, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("mark %s check running: %v", role, err)
	}
	claimed.Job = running

	return claimed
}

func claimSpecificJob(t *testing.T, fixture testFixture, workerID string, jobID string, buckets []flowworker.CapacityBucket) flowworker.ClaimedJob {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 20; attempt++ {
		claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
			WorkerID:      workerID,
			Buckets:       buckets,
			LeaseDuration: time.Minute,
		})
		if err != nil {
			t.Fatalf("claim job %s: %v", jobID, err)
		}
		if !ok {
			t.Fatalf("claim job %s: no matching job after %d attempts", jobID, attempt+1)
		}
		if claimed.Job.ID == jobID {
			return claimed
		}
		if _, err := fixture.Workers.ReleaseLease(ctx, claimed.Lease.ID, flowworker.JobCanceled); err != nil {
			t.Fatalf("cancel unrelated claimed job %s while looking for %s: %v", claimed.Job.ID, jobID, err)
		}
	}
	t.Fatalf("job %s was not claimable", jobID)
	return flowworker.ClaimedJob{}
}

func intPointer(value int) *int {
	return &value
}

func satisfyAPICheck(t *testing.T, fixture testFixture, issueID string, name string, kind coordinator.CheckKind) checkResponse {
	t.Helper()
	required := true
	var response checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issueID+"/checks/"+name, reportCheckRequest{
		Kind:     string(kind),
		Required: &required,
		Verdict:  string(coordinator.CheckSatisfied),
	}, http.StatusOK, &response)
	return response
}

func assertAPICheck(t *testing.T, fixture testFixture, issueID string, name string, kind coordinator.CheckKind, verdict coordinator.CheckVerdict) {
	t.Helper()
	check, err := fixture.Checks.GetCheck(context.Background(), issueID, name)
	if err != nil {
		t.Fatalf("get check %s: %v", name, err)
	}
	if check.Kind != kind || check.Verdict != verdict {
		t.Fatalf("check %s = %+v", name, check)
	}
}

func assertAPILiveJobs(t *testing.T, fixture testFixture, issueID string, want map[flowworker.JobRole]int) {
	t.Helper()
	jobs, err := fixture.Workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	counts := map[flowworker.JobRole]int{}
	for _, job := range jobs {
		if job.IssueID == nil || *job.IssueID != issueID {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			counts[job.Role]++
		}
	}
	for role, expected := range want {
		if counts[role] != expected {
			t.Fatalf("live %s jobs = %d, want %d; all counts=%+v", role, counts[role], expected, counts)
		}
	}
}

func writeAPIFile(t *testing.T, repoPath string, relativePath string, contents string) {
	t.Helper()
	path := filepath.Join(repoPath, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relativePath, err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", relativePath, err)
	}
}

func runAPIGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = apiGitOutput(t, dir, args...)
}

func apiGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}

	return strings.TrimSpace(string(output))
}

func TestWorkerEndpointsRequireWorkerScopeAndLeaseOwnership(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   "other-worker-token",
		Scope:   coordinator.TokenScopeWorker,
		Subject: "w-other",
	}); err != nil {
		t.Fatalf("store other worker token: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/workers/register", registerWorkerRequest{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/register", registerWorkerRequest{
		ID:                      "w-other",
		CapacityPersistentAgent: 1,
	}, http.StatusForbidden, nil)

	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Lease ownership issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if _, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok {
		t.Fatal("claim ok=false")
	}

	doJSONRequestAs(t, fixture.Server, "other-worker-token", http.MethodPost, "/v1/workers/renew", renewLeaseRequest{
		LeaseID:              claimed.Lease.ID,
		LeaseDurationSeconds: 60,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "other-worker-token", http.MethodPost, "/v1/workers/release", releaseLeaseRequest{
		LeaseID:    claimed.Lease.ID,
		FinalState: string(flowworker.JobFinished),
	}, http.StatusForbidden, nil)
}

func TestJobEnqueueIdempotencyReplaysCreatedJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Idempotent job issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	request := enqueueJobRequest{
		IssueID:        &issue.ID,
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketEphemeral),
		Priority:       3,
	}

	var first jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs", request, http.StatusCreated, &first, idempotencyHeader, "job-key")
	var second jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs", request, http.StatusCreated, &second, idempotencyHeader, "job-key")
	if first.Job.ID == "" || second.Job.ID != first.Job.ID {
		t.Fatalf("idempotent enqueue returned jobs %q then %q", first.Job.ID, second.Job.ID)
	}

	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("job count = %d, want 1: %+v", len(jobs), jobs)
	}
}

func TestAuthorJobEnqueueUsesSessionOrchestration(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Author enqueue issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs", enqueueJobRequest{
		IssueID:        &issue.ID,
		Role:           string(flowworker.RoleAuthor),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
	}, http.StatusBadRequest, nil)

	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	var response jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/jobs", enqueueJobRequest{
		IssueID:        &issue.ID,
		Role:           string(flowworker.RoleAuthor),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
		Priority:       9,
	}, http.StatusCreated, &response)
	if response.Job.Role != flowworker.RoleAuthor || response.Change == nil || response.Job.ChangeID == nil || *response.Job.ChangeID != response.Change.ID {
		t.Fatalf("author enqueue response = %+v", response)
	}
	if payloadString(response.Job.Payload, "branch") != "issue/"+issue.ID {
		t.Fatalf("author job payload = %+v", response.Job.Payload)
	}
}

func TestCheckReportingDerivesReviewStateAndBoardLane(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Check API issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, ensured.Change.ID, "head-1"); err != nil {
		t.Fatalf("record change head: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
		ChangeID:       &ensured.Change.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"check_name": "fake-ci",
			"change_id":  ensured.Change.ID,
			"head_sha":   "head-1",
		},
	})
	if err != nil {
		t.Fatalf("enqueue check job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim check job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed check job = %+v, ok=%t; want %s", claimed.Job, ok, job.ID)
	}

	exitFailure := 1
	var blocked checkResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/issues/"+issue.ID+"/checks/fake-ci", reportCheckRequest{
		ExitCode: &exitFailure,
		Details:  "exit status 1",
	}, http.StatusForbidden, nil)

	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/issues/"+issue.ID+"/checks/fake-ci", reportCheckRequest{
		ExitCode:    &exitFailure,
		Details:     "exit status 1",
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	}, http.StatusOK, &blocked)
	if blocked.Check.Verdict != coordinator.CheckBlocked || blocked.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("blocked check response = %+v", blocked)
	}
	if blocked.Check.Reporter != "w-local" {
		t.Fatalf("Reporter = %q, want worker subject", blocked.Check.Reporter)
	}

	var checks checksResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v1/issues/"+issue.ID+"/checks", nil, http.StatusOK, &checks)
	if len(checks.Checks) != 1 || checks.Checks[0].Name != "fake-ci" || checks.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("checks response = %+v", checks)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.InProgress) != 1 || board.Board.InProgress[0].ID != issue.ID {
		t.Fatalf("in_progress board = %+v", board.Board.InProgress)
	}
	if board.LaneStates[issue.ID] != coordinator.LaneStateChangesRequested {
		t.Fatalf("lane state = %q, want changes_requested", board.LaneStates[issue.ID])
	}
	if len(board.Board.UpNext) != 0 {
		t.Fatalf("up_next board = %+v, want empty while check is blocked", board.Board.UpNext)
	}

	exitZero := 0
	var satisfied checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/changes/"+issue.ID+"/checks/fake-ci", reportCheckRequest{
		ExitCode: &exitZero,
	}, http.StatusOK, &satisfied)
	if satisfied.Check.Verdict != coordinator.CheckSatisfied || satisfied.ReviewState != coordinator.ReviewApproved {
		t.Fatalf("satisfied check response = %+v", satisfied)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.InProgress) != 0 {
		t.Fatalf("in_progress board after satisfy = %+v, want empty", board.Board.InProgress)
	}
	if len(board.Board.UpNext) != 1 || board.Board.UpNext[0].ID != issue.ID {
		t.Fatalf("up_next board after satisfy = %+v", board.Board.UpNext)
	}
}

func TestBlockedCheckOnNonReviewableIssueDoesNotFailOrEnqueueAuthorJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Backlog check issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	required := true
	var response checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/issues/"+issue.ID+"/checks/reviewer", reportCheckRequest{
		Kind:     string(coordinator.CheckKindReviewer),
		Required: &required,
		Verdict:  string(coordinator.CheckBlocked),
		Details:  "Blocked before scheduling.",
	}, http.StatusOK, &response)
	if response.Check.Verdict != coordinator.CheckBlocked || response.ReviewState != coordinator.ReviewChangesRequested {
		t.Fatalf("check response = %+v", response)
	}
	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none for non-reviewable issue", jobs)
	}
}

func TestWorkerCheckReportingRejectsExpiredLease(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Expired check lease issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue check job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim check job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed check job = %+v, ok=%t; want %s", claimed.Job, ok, job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark job running: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claimed.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	exitFailure := 1
	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/issues/"+issue.ID+"/checks/fake-ci", reportCheckRequest{
		ExitCode:    &exitFailure,
		Details:     "exit status 1",
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	}, http.StatusForbidden, nil)

	swept, err := fixture.Workers.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get swept job: %v", err)
	}
	if swept.State != flowworker.JobCrashed {
		t.Fatalf("expired job state = %q, want crashed", swept.State)
	}
}

func TestWorkerCheckReportingRejectsNonCISourceJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Non-CI check lease issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
	})
	if err != nil {
		t.Fatalf("enqueue author job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed author job = %+v, ok=%t; want %s", claimed.Job, ok, job.ID)
	}

	exitZero := 0
	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/issues/"+issue.ID+"/checks/fake-ci", reportCheckRequest{
		ExitCode:    &exitZero,
		Details:     "exit status 0",
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	}, http.StatusForbidden, nil)
}

func TestSessionCheckReportingIsBoundToSourceIssue(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	source, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Session-owned issue"})
	if err != nil {
		t.Fatalf("create source issue: %v", err)
	}
	other, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Other issue"})
	if err != nil {
		t.Fatalf("create other issue: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:         "session-token",
		Scope:         coordinator.TokenScopeSession,
		Subject:       "s-1",
		ProjectID:     &fixture.Project.ID,
		SourceIssueID: &source.ID,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	exitZero := 0
	required := true
	optional := false
	var response checkResponse
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v1/issues/"+source.ID+"/checks/session-verdict", reportCheckRequest{
		Kind:     string(coordinator.CheckKindCI),
		Required: &optional,
		Verdict:  string(coordinator.CheckSatisfied),
		ExitCode: &exitZero,
	}, http.StatusOK, &response)
	if response.Check.Reporter != "s-1" {
		t.Fatalf("Reporter = %q, want session subject", response.Check.Reporter)
	}

	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v1/issues/"+source.ID+"/checks/required-session-verdict", reportCheckRequest{
		Kind:     string(coordinator.CheckKindCI),
		Required: &required,
		Verdict:  string(coordinator.CheckSatisfied),
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v1/issues/"+source.ID+"/checks/human-review", reportCheckRequest{
		Kind:     string(coordinator.CheckKindHuman),
		Required: &optional,
		Verdict:  string(coordinator.CheckSatisfied),
	}, http.StatusForbidden, nil)

	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v1/issues/"+other.ID+"/checks/session-verdict", reportCheckRequest{
		Required: &optional,
		Verdict:  string(coordinator.CheckSatisfied),
	}, http.StatusForbidden, nil)
}

func TestWorkerClaimCanWaitForJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	enqueued := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
			Role:           flowworker.RoleCI,
			CapacityBucket: flowworker.BucketEphemeral,
		})
		enqueued <- err
	}()

	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDurationSeconds: 60,
		WaitSeconds:          1,
	}, http.StatusOK, &claim)
	if err := <-enqueued; err != nil {
		t.Fatalf("enqueue delayed job: %v", err)
	}
	if !claim.Claimed || claim.Job == nil || claim.Job.Role != flowworker.RoleCI || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
}

func TestWorkerClaimSweepsExpiredLeases(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	expiredJob, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       100,
	})
	if err != nil {
		t.Fatalf("enqueue expired job: %v", err)
	}
	now := time.Now().UTC()
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE id = ?`, string(flowworker.JobClaimed), expiredJob.ID); err != nil {
		t.Fatalf("mark expired job claimed: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO leases (
	id,
	job_id,
	worker_id,
	capacity_bucket,
	leased_at,
	expires_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		"l-expired",
		expiredJob.ID,
		"w-local",
		string(flowworker.BucketEphemeral),
		now.Add(-2*time.Minute).Format(time.RFC3339Nano),
		now.Add(-time.Minute).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert expired lease: %v", err)
	}
	queuedJob, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       1,
	})
	if err != nil {
		t.Fatalf("enqueue queued job: %v", err)
	}

	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != queuedJob.ID {
		t.Fatalf("claim response = %+v, want queued job %s", claim, queuedJob.ID)
	}

	swept, err := fixture.Workers.GetJob(ctx, expiredJob.ID)
	if err != nil {
		t.Fatalf("get swept job: %v", err)
	}
	if swept.State != flowworker.JobCrashed {
		t.Fatalf("expired job state = %q, want crashed", swept.State)
	}
}

func TestWorkerClaimReconcilesCrashedAuthorSession(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "API crash resume issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claimed.Lease.ID); err != nil {
		t.Fatalf("expire author lease: %v", err)
	}

	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
	if claim.Job.ID == claimed.Job.ID {
		t.Fatalf("claimed crashed job again: %+v", claim.Job)
	}
	if claim.Job.ChangeID == nil || *claim.Job.ChangeID != started.Change.ID {
		t.Fatalf("resume claim ChangeID = %v, want %s", claim.Job.ChangeID, started.Change.ID)
	}
	if branch, _ := claim.Job.Payload["branch"].(string); branch != started.Change.Branch {
		t.Fatalf("resume claim payload = %+v, want branch %s", claim.Job.Payload, started.Change.Branch)
	}
	crashed, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get crashed session: %v", err)
	}
	if crashed.RuntimeState != coordinator.SessionCrashed {
		t.Fatalf("session state = %q, want crashed", crashed.RuntimeState)
	}
}

func newTestServer(t *testing.T) *Server {
	return newTestFixture(t).Server
}

// newTestRegistryInDir builds a registry rooted at dataDir so callers (e.g. the
// coordinator-restart test) can reopen the same data directory. It also returns
// the dataDir and the global store handle (tokens and web sessions live there).
func newTestRegistryInDir(t *testing.T, dataDir string, projectNames ...string) (*Registry, []*ProjectBundle, string, *flowdb.Store) {
	t.Helper()
	ctx := context.Background()

	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })

	registry, err := NewRegistry(RegistryOptions{DataDir: dataDir, Global: global})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	bundles := make([]*ProjectBundle, 0, len(projectNames))
	for _, name := range projectNames {
		project, err := registry.CreateProject(ctx, coordinator.Project{Name: name, BaseBranch: "main"})
		if err != nil {
			t.Fatalf("create project %q: %v", name, err)
		}
		bundle, ok := registry.Bundle(project.ID)
		if !ok {
			t.Fatalf("bundle for project %q not found after create", name)
		}
		bundles = append(bundles, bundle)
	}

	return registry, bundles, dataDir, global
}

// reopenTestRegistry reopens an existing data directory, simulating a
// coordinator restart: it opens every persisted project bundle from disk.
func reopenTestRegistry(t *testing.T, dataDir string) (*Registry, []*ProjectBundle, *flowdb.Store) {
	t.Helper()
	ctx := context.Background()

	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("reopen global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })

	registry, err := NewRegistry(RegistryOptions{DataDir: dataDir, Global: global})
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	if err := registry.OpenAll(ctx); err != nil {
		t.Fatalf("open all projects: %v", err)
	}

	return registry, registry.All(), global
}

// testWorkers wraps a project's queue service with the worker-directory and
// cross-project claim operations the registry now owns, so existing tests can
// keep calling RegisterWorker/ClaimNextJob on the fixture's "Workers".
type testWorkers struct {
	*flowworker.Service
	registry *Registry
}

func (w testWorkers) RegisterWorker(ctx context.Context, input flowworker.RegisterWorkerInput) (flowworker.Worker, error) {
	return w.registry.Directory().RegisterWorker(ctx, input)
}

func (w testWorkers) ClaimNextJob(ctx context.Context, input flowworker.ClaimInput) (flowworker.ClaimedJob, bool, error) {
	claim, ok, err := w.registry.Claim(ctx, input)
	if err != nil || !ok {
		return flowworker.ClaimedJob{}, ok, err
	}
	return flowworker.ClaimedJob{Job: claim.Job, Lease: claim.Lease}, true, nil
}

type testFixture struct {
	Registry    *Registry
	Bundle      *ProjectBundle
	Project     coordinator.Project
	DataDir     string
	Store       *flowdb.Store
	GlobalStore *flowdb.Store
	Server      *Server
	DB          *sql.DB
	GlobalDB    *sql.DB
	Issues      *coordinator.IssueService
	Checks      *coordinator.CheckService
	Credentials *coordinator.CredentialService
	GitEvents   *coordinator.GitEventService
	Workers     testWorkers
	Sessions    *coordinator.SessionService
	Status      *coordinator.StatusService
	Reconciler  *coordinator.ReconcileService
	Threads     *coordinator.ThreadService
	CheckConfig *coordinator.CheckConfigService
	Merges      *coordinator.MergeService
	WebSessions *coordinator.WebSessionService
}

func newTestFixture(t *testing.T) testFixture {
	t.Helper()
	registry, bundles, dataDir, global := newTestRegistryInDir(t, t.TempDir(), "api")
	return fixtureFromRegistry(t, registry, bundles[0], dataDir, global, true)
}

// boardPath returns the single-project board route, which keeps the legacy
// boardResponse shape (the unscoped /v1/board now returns an aggregate).
func (f testFixture) boardPath() string {
	return "/v1/projects/" + f.Project.ID + "/board"
}

// gitEventsPath returns the project-scoped hook git-events route.
func (f testFixture) gitEventsPath() string {
	return "/v1/projects/" + f.Project.ID + "/git/events"
}

// reopenTestFixture rebuilds a fixture from an existing data directory without
// re-minting tokens (they persist in the global database).
func reopenTestFixture(t *testing.T, dataDir string) testFixture {
	t.Helper()
	registry, bundles, global := reopenTestRegistry(t, dataDir)
	if len(bundles) == 0 {
		t.Fatalf("reopened registry has no project bundles")
	}
	return fixtureFromRegistry(t, registry, bundles[0], dataDir, global, false)
}

// fixtureFromRegistry wires a testFixture from a registry and its primary
// bundle. When mintTokens is set it seeds the standard owner/hook/worker
// tokens; reopened fixtures reuse the persisted ones.
func fixtureFromRegistry(t *testing.T, registry *Registry, bundle *ProjectBundle, dataDir string, global *flowdb.Store, mintTokens bool) testFixture {
	t.Helper()

	credentials := registry.Credentials()
	ctx := context.Background()
	if mintTokens {
		if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
			Token: "owner-token",
			Scope: coordinator.TokenScopeOwner,
		}); err != nil {
			t.Fatalf("store owner token: %v", err)
		}
		if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
			Token: "hook-token",
			Scope: coordinator.TokenScopeHook,
		}); err != nil {
			t.Fatalf("store hook token: %v", err)
		}
		if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
			Token:   "worker-token",
			Scope:   coordinator.TokenScopeWorker,
			Subject: "w-local",
		}); err != nil {
			t.Fatalf("store worker token: %v", err)
		}
	}

	server, err := NewServer(ServerOptions{
		Registry:   registry,
		OwnerToken: "owner-token",
		HookToken:  "hook-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	return testFixture{
		Registry:    registry,
		Bundle:      bundle,
		Project:     bundle.Project,
		DataDir:     dataDir,
		Store:       bundle.Store,
		GlobalStore: global,
		Server:      server,
		DB:          bundle.Store.DB(),
		GlobalDB:    global.DB(),
		Issues:      bundle.Issues,
		Checks:      bundle.Checks,
		Credentials: credentials,
		GitEvents:   bundle.GitEvents,
		Workers:     testWorkers{Service: bundle.Queue, registry: registry},
		Sessions:    bundle.Sessions,
		Status:      bundle.Status,
		Reconciler:  bundle.Reconciler,
		Threads:     bundle.Threads,
		CheckConfig: bundle.CheckConfigs,
		Merges:      bundle.Merges,
		WebSessions: registry.WebSessions(),
	}
}

func authorizedRequest(method string, path string, body any) *http.Request {
	var requestBody bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&requestBody).Encode(body)
	}
	request := httptest.NewRequest(method, path, &requestBody)
	request.Header.Set("Authorization", "Bearer owner-token")
	request.Header.Set(protocolHeader, "1")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	return request
}

func doJSONRequest(t *testing.T, server *Server, method string, path string, body any, wantStatus int, target any) {
	t.Helper()
	doJSONRequestAs(t, server, "owner-token", method, path, body, wantStatus, target)
}

func doJSONRequestAs(t *testing.T, server *Server, token string, method string, path string, body any, wantStatus int, target any, extraHeaders ...string) {
	t.Helper()

	response := httptest.NewRecorder()
	request := authorizedRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(extraHeaders); i += 2 {
		request.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}
	server.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, path, response.Code, wantStatus, response.Body.String())
	}
	if target != nil {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
}

func harnessOptionNames(options []contract.HarnessOption) map[string]bool {
	names := map[string]bool{}
	for _, option := range options {
		names[option.Name] = true
	}
	return names
}

// newMultiProjectServer builds a registry with the named projects, mints an
// owner token, and returns the server alongside the project bundles in the
// order the names were given.
func newMultiProjectServer(t *testing.T, projectNames ...string) (*Server, []*ProjectBundle) {
	t.Helper()

	registry, bundles, _, _ := newTestRegistryInDir(t, t.TempDir(), projectNames...)
	if err := registry.Credentials().EnsureToken(context.Background(), coordinator.CredentialInput{
		Token: "owner-token",
		Scope: coordinator.TokenScopeOwner,
	}); err != nil {
		t.Fatalf("store owner token: %v", err)
	}
	server, err := NewServer(ServerOptions{Registry: registry, OwnerToken: "owner-token"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	return server, bundles
}

func TestListProjectsEndpoint(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")

	var response projectsResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v1/projects", nil, http.StatusOK, &response)
	if len(response.Projects) != 2 {
		t.Fatalf("projects = %+v, want 2", response.Projects)
	}

	byID := map[string]uiProject{}
	for _, project := range response.Projects {
		byID[project.ID] = project
	}
	for _, bundle := range bundles {
		got, ok := byID[bundle.Project.ID]
		if !ok {
			t.Fatalf("project %s missing from listing %+v", bundle.Project.ID, response.Projects)
		}
		if got.Name != bundle.Project.Name {
			t.Fatalf("project %s name = %q, want %q", bundle.Project.ID, got.Name, bundle.Project.Name)
		}
	}
}

func TestAggregateBoardMergesProjects(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	ctx := context.Background()

	for _, bundle := range bundles {
		issue, err := bundle.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Work in " + bundle.Project.Name})
		if err != nil {
			t.Fatalf("create issue in %s: %v", bundle.Project.Name, err)
		}
		if issue.ID != "i-0001" {
			t.Fatalf("first issue id in %s = %q, want i-0001", bundle.Project.Name, issue.ID)
		}
		if _, err := bundle.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
			t.Fatalf("schedule issue in %s: %v", bundle.Project.Name, err)
		}
	}

	var response aggregateBoardResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v1/board", nil, http.StatusOK, &response)
	if len(response.Boards) != 2 {
		t.Fatalf("boards = %+v, want 2", response.Boards)
	}

	seenProjects := map[string]bool{}
	for _, board := range response.Boards {
		if seenProjects[board.ProjectID] {
			t.Fatalf("duplicate project id %s in aggregate board", board.ProjectID)
		}
		seenProjects[board.ProjectID] = true
		if len(board.Board.UpNext) != 1 || board.Board.UpNext[0].ID != "i-0001" {
			t.Fatalf("board for %s up_next = %+v, want [i-0001]", board.ProjectName, board.Board.UpNext)
		}
	}
	for _, bundle := range bundles {
		if !seenProjects[bundle.Project.ID] {
			t.Fatalf("aggregate board missing project %s; boards = %+v", bundle.Project.ID, response.Boards)
		}
	}
}

func TestProjectScopedIssueRouteIsolation(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	projectA, projectB := bundles[0], bundles[1]

	issue, err := projectA.Issues.CreateIssue(context.Background(), coordinator.CreateIssueInput{Title: "Only in alpha"})
	if err != nil {
		t.Fatalf("create issue in alpha: %v", err)
	}
	if issue.ID != "i-0001" {
		t.Fatalf("issue id = %q, want i-0001", issue.ID)
	}

	doJSONRequestAs(t, server, "owner-token", http.MethodGet,
		"/v1/projects/"+projectB.Project.ID+"/issues/i-0001", nil, http.StatusNotFound, nil)

	var found issueResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet,
		"/v1/projects/"+projectA.Project.ID+"/issues/i-0001", nil, http.StatusOK, &found)
	if found.Issue.ID != "i-0001" || found.Issue.Title != "Only in alpha" {
		t.Fatalf("alpha issue = %+v", found.Issue)
	}
}

func TestUnscopedIssueRouteRejectedWithMultipleProjects(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")

	if _, err := bundles[0].Issues.CreateIssue(context.Background(), coordinator.CreateIssueInput{Title: "Ambiguous"}); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, "/v1/issues/i-0001", nil)
	server.ServeHTTP(response, request)
	if response.Code < 400 || response.Code >= 500 {
		t.Fatalf("unscoped issue status = %d, want 4xx; body: %s", response.Code, response.Body.String())
	}
	var body errorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(body.Error.Message, "/v1/projects/") {
		t.Fatalf("error message = %q, want guidance to use the project-scoped route", body.Error.Message)
	}
}

func TestSessionProcessExitCrashesAuthorSessionAndReconciles(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	issue, err := fixture.Issues.CreateIssue(ctx, coordinator.CreateIssueInput{Title: "Process exit issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if _, err := fixture.Issues.ScheduleIssue(ctx, issue.ID, coordinator.ScheduleUpNext); err != nil {
		t.Fatalf("schedule issue: %v", err)
	}
	ensured, err := fixture.Sessions.EnsureAuthorJob(ctx, coordinator.EnsureAuthorJobInput{IssueID: issue.ID})
	if err != nil {
		t.Fatalf("ensure author job: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != ensured.Job.ID {
		t.Fatalf("claim = %+v ok=%t, want %s", claimed.Job, ok, ensured.Job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: "w-local",
	})
	if err != nil {
		t.Fatalf("start author session: %v", err)
	}

	// A worker reports an un-finalized process exit (exit code 0 is still a crash
	// because the interactive agent never finalized the session itself).
	var exited sessionResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/sessions/"+started.Session.ID+"/process-exit", sessionProcessExitRequest{
		LeaseID:  claimed.Lease.ID,
		ExitCode: 0,
	}, http.StatusOK, &exited)
	if exited.Session.RuntimeState != coordinator.SessionCrashed {
		t.Fatalf("process-exit session state = %q, want crashed", exited.Session.RuntimeState)
	}

	crashed, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get crashed session: %v", err)
	}
	if crashed.RuntimeState != coordinator.SessionCrashed || crashed.FinishedAt == nil {
		t.Fatalf("crashed session = %+v, want crashed with finished_at", crashed)
	}
	if _, err := fixture.Credentials.Authenticate(ctx, started.Token); !errors.Is(err, coordinator.ErrInvalidCredential) {
		t.Fatalf("authenticate crashed token err = %v, want ErrInvalidCredential", err)
	}
	// handleSessionProcessExit reconciles the crashed author session, so a fresh
	// author job is live for the issue.
	resumeJob, ok, err := fixture.Workers.LiveAuthorJobForIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("live author job after process-exit: %v", err)
	}
	if !ok || resumeJob.ChangeID == nil || *resumeJob.ChangeID != started.Change.ID {
		t.Fatalf("resume job = %+v ok=%t, want change %s", resumeJob, ok, started.Change.ID)
	}
	if resumeJob.ID == claimed.Job.ID {
		t.Fatal("resume job reused crashed job id")
	}
}

func TestSessionProcessExitRejectsConsoleSession(t *testing.T) {
	fixture := newTestFixture(t)

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v1/console", consoleRequest{
		Harness: "codex",
	}, http.StatusCreated, &startedConsole)
	if startedConsole.Job == nil {
		t.Fatalf("started console response = %+v", startedConsole)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Codex): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Role != flowworker.RoleConsole {
		t.Fatalf("running console = %+v", running)
	}

	// A console session must be released through /v1/console, never the generic
	// persistent-session process-exit path.
	var body errorResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v1/sessions/"+running.Session.ID+"/process-exit", sessionProcessExitRequest{
		LeaseID:  claim.Lease.ID,
		ExitCode: 0,
	}, http.StatusBadRequest, &body)
	if !strings.Contains(body.Error.Message, "console sessions are released through console release") {
		t.Fatalf("console process-exit error = %q, want console release rejection", body.Error.Message)
	}
	// The console session and its lease are untouched by the rejected call.
	session, err := fixture.Sessions.GetSession(context.Background(), running.Session.ID)
	if err != nil {
		t.Fatalf("get console session: %v", err)
	}
	if session.RuntimeState == coordinator.SessionCrashed {
		t.Fatalf("console session = %+v, want unchanged (not crashed)", session)
	}
}
