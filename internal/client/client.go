package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

const (
	protocolHeader = contract.ProtocolHeader
	authScheme     = contract.AuthScheme
)

type Client struct {
	baseURL         string
	token           string
	protocolVersion string
	projectID       string
	httpClient      *http.Client
}

func New(cfg config.ClientConfig) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("server URL is required")
	}
	protocolVersion := cfg.ProtocolVersion
	if protocolVersion == "" {
		protocolVersion = config.DefaultProtocolVersion
	}

	return &Client{
		baseURL:         baseURL,
		token:           cfg.Token,
		protocolVersion: protocolVersion,
		httpClient:      http.DefaultClient,
	}, nil
}

// WithProject returns a client whose issue and board calls target the given
// project's scoped routes. Without a project the unscoped routes apply: the
// coordinator resolves session tokens to their bound project and treats a
// single-project deployment as implicit.
func (c *Client) WithProject(projectID string) *Client {
	clone := *c
	clone.projectID = strings.TrimSpace(projectID)
	return &clone
}

// issuesPath scopes an issue route to the client's project when one is set.
func (c *Client) issuesPath(suffix string) string {
	if c.projectID != "" {
		return "/v1/projects/" + url.PathEscape(c.projectID) + "/issues" + suffix
	}

	return "/v1/issues" + suffix
}

func (c *Client) consolePath() string {
	if c.projectID != "" {
		return "/v1/projects/" + url.PathEscape(c.projectID) + "/console"
	}

	return "/v1/console"
}

func (c *Client) CreateIssue(input CreateIssueInput) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPost, c.issuesPath(""), input, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) ListIssues(filter IssueFilter) ([]coordinator.Issue, error) {
	query := url.Values{}
	for _, state := range filter.ScheduleStates {
		query.Add("schedule_state", string(state))
	}
	for _, state := range filter.TriageStates {
		query.Add("triage_state", string(state))
	}
	for _, tag := range filter.TagSlugs {
		query.Add("tag", tag)
	}

	var response issuesResponse
	if err := c.do(http.MethodGet, c.issuesPath(""), nil, query, &response); err != nil {
		return nil, err
	}

	return response.Issues, nil
}

func (c *Client) GetIssue(id string) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodGet, c.issuesPath("/"+url.PathEscape(id)), nil, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) GetIssueWithStatus(id string) (coordinator.Issue, []coordinator.StatusLogEntry, error) {
	var response issueResponse
	if err := c.do(http.MethodGet, c.issuesPath("/"+url.PathEscape(id)), nil, nil, &response); err != nil {
		return coordinator.Issue{}, nil, err
	}

	return response.Issue, response.StatusLog, nil
}

func (c *Client) EditIssue(id string, input EditIssueInput) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPatch, c.issuesPath("/"+url.PathEscape(id)), input, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) ScheduleIssue(id string, state coordinator.ScheduleState) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(id))+"/schedule", scheduleIssueRequest{State: string(state)}, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) SetIssueState(id string, state coordinator.IssueState) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(id))+"/state", issueStateRequest{State: string(state)}, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) ResetIssue(id string) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(id))+"/reset", map[string]string{}, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) CloseIssue(id string) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(id))+"/close", map[string]string{}, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) TriageIssue(id string, state coordinator.TriageState) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(id))+"/triage", triageIssueRequest{State: string(state)}, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) LinkIssues(sourceIssueID string, kind coordinator.RelationKind, targetIssueID string) error {
	request := issueRelationRequest{
		TargetIssueID: targetIssueID,
		Kind:          string(kind),
	}
	return c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(sourceIssueID))+"/relations", request, nil, nil)
}

func (c *Client) UnlinkIssues(sourceIssueID string, kind coordinator.RelationKind, targetIssueID string) error {
	request := issueRelationRequest{
		TargetIssueID: targetIssueID,
		Kind:          string(kind),
	}
	return c.do(http.MethodDelete, c.issuesPath("/"+url.PathEscape(sourceIssueID))+"/relations", request, nil, nil)
}

func (c *Client) MergeIssue(id string) (coordinator.MergeResult, error) {
	var response mergeResponse
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(id))+"/merge", map[string]string{}, nil, &response); err != nil {
		return coordinator.MergeResult{}, err
	}

	return response.Merge, nil
}

func (c *Client) UploadIssueAttachment(issueID string, input UploadIssueAttachmentInput) (coordinator.IssueAttachment, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if input.Stage != "" {
		if err := writer.WriteField("stage", string(input.Stage)); err != nil {
			return coordinator.IssueAttachment{}, err
		}
	}
	filename := strings.TrimSpace(input.Filename)
	if filename == "" {
		return coordinator.IssueAttachment{}, errors.New("attachment filename is required")
	}
	contentType := strings.TrimSpace(input.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name":     "file",
		"filename": filename,
	}))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return coordinator.IssueAttachment{}, err
	}
	if input.Reader == nil {
		return coordinator.IssueAttachment{}, errors.New("attachment reader is required")
	}
	if _, err := io.Copy(part, input.Reader); err != nil {
		return coordinator.IssueAttachment{}, err
	}
	if err := writer.Close(); err != nil {
		return coordinator.IssueAttachment{}, err
	}

	query := url.Values{}
	if strings.TrimSpace(input.LeaseID) != "" {
		query.Set("lease_id", strings.TrimSpace(input.LeaseID))
	}
	var response issueAttachmentResponse
	if err := c.doMultipart(http.MethodPost, c.issuesPath("/"+url.PathEscape(issueID))+"/attachments", query, writer.FormDataContentType(), &body, &response); err != nil {
		return coordinator.IssueAttachment{}, err
	}

	return response.Attachment, nil
}

// ListIssueAttachments lists the attachments recorded for an issue. It is the
// client counterpart to the issue attachments read API and authenticates with
// the client's token (the worker uses its worker token).
func (c *Client) ListIssueAttachments(ctx context.Context, issueID string) ([]coordinator.IssueAttachment, error) {
	var response issueAttachmentsResponse
	if err := c.doContext(ctx, http.MethodGet, c.issuesPath("/"+url.PathEscape(issueID))+"/attachments", nil, nil, &response); err != nil {
		return nil, err
	}
	return response.Attachments, nil
}

// DownloadIssueAttachment downloads an issue attachment's bytes into dst. It
// authenticates with the client's token (the worker uses its worker token).
func (c *Client) DownloadIssueAttachment(ctx context.Context, issueID, attachmentID string, dst io.Writer) error {
	endpoint := c.baseURL + c.issuesPath("/"+url.PathEscape(issueID)+"/attachments/"+url.PathEscape(attachmentID))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, c.protocolVersion)
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := statusError(response); err != nil {
		return err
	}
	if _, err := io.Copy(dst, response.Body); err != nil {
		return err
	}
	return nil
}

func (c *Client) MergeChange(id string) (coordinator.MergeResult, error) {
	var response mergeResponse
	if err := c.do(http.MethodPost, "/v1/changes/"+url.PathEscape(id)+"/merge", map[string]string{}, nil, &response); err != nil {
		return coordinator.MergeResult{}, err
	}

	return response.Merge, nil
}

func (c *Client) CreateWebBootstrap() (WebBootstrapResult, error) {
	var response webBootstrapResponse
	if err := c.do(http.MethodPost, "/v1/ui/bootstrap", map[string]string{}, nil, &response); err != nil {
		return WebBootstrapResult{}, err
	}

	return WebBootstrapResult{
		LoginPath: response.LoginPath,
		ExpiresAt: response.ExpiresAt,
	}, nil
}

// Board returns one project's board; the client must be project-scoped (or
// the deployment single-project, in which case the coordinator resolves it).
func (c *Client) Board() (coordinator.BoardResult, error) {
	if c.projectID != "" {
		var response boardResponse
		if err := c.do(http.MethodGet, "/v1/projects/"+url.PathEscape(c.projectID)+"/board", nil, nil, &response); err != nil {
			return coordinator.BoardResult{}, err
		}
		return coordinator.BoardResult{
			Board:       response.Board,
			LaneStates:  response.LaneStates,
			WaitReasons: response.WaitReasons,
			BlockedIDs:  response.BlockedIDs,
		}, nil
	}

	boards, err := c.BoardAll()
	if err != nil {
		return coordinator.BoardResult{}, err
	}
	if len(boards) == 1 {
		return boards[0].BoardResult, nil
	}
	merged := coordinator.BoardResult{LaneStates: map[string]coordinator.LaneState{}, WaitReasons: map[string]coordinator.WaitReason{}}
	for _, board := range boards {
		merged.Board.Backlog = append(merged.Board.Backlog, board.Board.Backlog...)
		merged.Board.UpNext = append(merged.Board.UpNext, board.Board.UpNext...)
		merged.Board.InProgress = append(merged.Board.InProgress, board.Board.InProgress...)
		merged.Board.NeedsAttention = append(merged.Board.NeedsAttention, board.Board.NeedsAttention...)
		for id, state := range board.LaneStates {
			merged.LaneStates[id] = state
		}
		for id, reason := range board.WaitReasons {
			merged.WaitReasons[id] = reason
		}
		merged.BlockedIDs = append(merged.BlockedIDs, board.BlockedIDs...)
	}
	return merged, nil
}

// ProjectBoard is one project's board within the aggregate response.
type ProjectBoard struct {
	ProjectID   string
	ProjectName string
	coordinator.BoardResult
}

// BoardAll returns every project's board.
func (c *Client) BoardAll() ([]ProjectBoard, error) {
	var response aggregateBoardResponse
	if err := c.do(http.MethodGet, "/v1/board", nil, nil, &response); err != nil {
		return nil, err
	}

	boards := make([]ProjectBoard, 0, len(response.Boards))
	for _, board := range response.Boards {
		boards = append(boards, ProjectBoard{
			ProjectID:   board.ProjectID,
			ProjectName: board.ProjectName,
			BoardResult: coordinator.BoardResult{
				Board:       board.Board,
				LaneStates:  board.LaneStates,
				WaitReasons: board.WaitReasons,
				BlockedIDs:  board.BlockedIDs,
			},
		})
	}

	return boards, nil
}

// Project mirrors the coordinator's project registry entry.
type Project = contract.Project
type HarnessOption = contract.HarnessOption
type HarnessesResponse = contract.HarnessesResponse

type CreateProjectInput struct {
	Name         string `json:"name,omitempty"`
	RepoPath     string `json:"repo_path,omitempty"`
	BaseBranch   string `json:"base_branch"`
	ExchangeName string `json:"exchange_name,omitempty"`
}

// CreateProject registers a project with the coordinator; re-registering the
// same repo path returns the existing project with created=false.
func (c *Client) CreateProject(input CreateProjectInput) (Project, bool, error) {
	var response projectResponse
	if err := c.do(http.MethodPost, "/v1/projects", input, nil, &response); err != nil {
		return Project{}, false, err
	}

	return response.Project, response.Created, nil
}

func (c *Client) ListProjects() ([]Project, error) {
	var response projectsResponse
	if err := c.do(http.MethodGet, "/v1/projects", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Projects, nil
}

func (c *Client) ListHarnesses() (HarnessesResponse, error) {
	var response harnessesResponse
	if err := c.do(http.MethodGet, "/v1/harnesses", nil, nil, &response); err != nil {
		return HarnessesResponse{}, err
	}

	return response, nil
}

// LookupProjectByRepoPath resolves the project registered for a repo root,
// or nil when none matches.
func (c *Client) LookupProjectByRepoPath(repoPath string) (*Project, error) {
	query := url.Values{}
	query.Set("repo_path", repoPath)

	var response projectsResponse
	if err := c.do(http.MethodGet, "/v1/projects", nil, query, &response); err != nil {
		return nil, err
	}
	if len(response.Projects) == 0 {
		return nil, nil
	}

	return &response.Projects[0], nil
}

func (c *Client) ListChecks(issueID string) (CheckListResult, error) {
	var response checksResponse
	if err := c.do(http.MethodGet, c.issuesPath("/"+url.PathEscape(issueID))+"/checks", nil, nil, &response); err != nil {
		return CheckListResult{}, err
	}

	return CheckListResult{
		Checks:      response.Checks,
		ReviewState: response.ReviewState,
	}, nil
}

func (c *Client) ListTransitions(issueID string) ([]coordinator.TransitionLogEntry, error) {
	var response transitionsResponse
	if err := c.do(http.MethodGet, c.issuesPath("/"+url.PathEscape(issueID))+"/transitions", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Transitions, nil
}

func (c *Client) RunReview(issueID string) (ReviewRunResult, error) {
	var response reviewRunResponse
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(issueID))+"/review/run", map[string]string{}, nil, &response); err != nil {
		return ReviewRunResult{}, err
	}

	return ReviewRunResult{
		Change:      response.Change,
		Scheduled:   response.Scheduled,
		Checks:      response.Checks,
		ReviewState: response.ReviewState,
	}, nil
}

func (c *Client) GetCheck(issueID string, name string) (CheckResult, error) {
	var response checkResponse
	if err := c.do(http.MethodGet, c.issuesPath("/"+url.PathEscape(issueID))+"/checks/"+url.PathEscape(name), nil, nil, &response); err != nil {
		return CheckResult{}, err
	}

	return CheckResult{
		Check:            response.Check,
		ReviewState:      response.ReviewState,
		FollowUpFailures: response.FollowUpFailures,
	}, nil
}

func (c *Client) ReportCheck(issueID string, name string, input ReportCheckInput) (CheckResult, error) {
	var response checkResponse
	request := reportCheckRequest{
		Kind:        string(input.Kind),
		Required:    input.Required,
		Verdict:     string(input.Verdict),
		ExitCode:    input.ExitCode,
		Details:     input.Details,
		SourceJobID: input.SourceJobID,
		LeaseID:     input.LeaseID,
		Reporter:    input.Reporter,
	}
	if err := c.do(http.MethodPost, c.issuesPath("/"+url.PathEscape(issueID))+"/checks/"+url.PathEscape(name), request, nil, &response); err != nil {
		return CheckResult{}, err
	}

	return CheckResult{
		Check:            response.Check,
		ReviewState:      response.ReviewState,
		FollowUpFailures: response.FollowUpFailures,
	}, nil
}

func (c *Client) CreateThread(changeID string, input CreateThreadInput) (coordinator.ReviewThread, error) {
	var response threadResponse
	request := createThreadRequest{
		AnchorCommitSHA: input.AnchorCommitSHA,
		FilePath:        input.FilePath,
		Line:            input.Line,
		Context:         input.Context,
		Body:            input.Body,
		LeaseID:         input.LeaseID,
	}
	if err := c.do(http.MethodPost, "/v1/changes/"+url.PathEscape(changeID)+"/comments", request, nil, &response); err != nil {
		return coordinator.ReviewThread{}, err
	}

	return response.Thread, nil
}

func (c *Client) ListThreads(changeID string, leaseID string) ([]coordinator.ReviewThread, error) {
	var response threadsResponse
	path := "/v1/changes/" + url.PathEscape(changeID) + "/threads"
	if strings.TrimSpace(leaseID) != "" {
		path += "?lease_id=" + url.QueryEscape(strings.TrimSpace(leaseID))
	}
	if err := c.do(http.MethodGet, path, nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Threads, nil
}

func (c *Client) ReplyThread(threadID string, body string, leaseID string) (coordinator.ReviewThread, error) {
	var response threadResponse
	if err := c.do(http.MethodPost, "/v1/threads/"+url.PathEscape(threadID)+"/comments", threadCommentRequest{Body: body, LeaseID: leaseID}, nil, &response); err != nil {
		return coordinator.ReviewThread{}, err
	}

	return response.Thread, nil
}

func (c *Client) ClaimThread(threadID string, input ClaimThreadInput) (coordinator.ReviewThread, error) {
	var response threadResponse
	request := threadClaimRequest{
		Kind:           string(input.Kind),
		Body:           input.Body,
		ClaimCommitSHA: input.ClaimCommitSHA,
		LeaseID:        input.LeaseID,
	}
	if err := c.do(http.MethodPost, "/v1/threads/"+url.PathEscape(threadID)+"/claims", request, nil, &response); err != nil {
		return coordinator.ReviewThread{}, err
	}

	return response.Thread, nil
}

func (c *Client) CertifyThread(threadID string, body string, leaseID string) (coordinator.ReviewThread, error) {
	return c.verifyThread(threadID, "certify", body, leaseID)
}

func (c *Client) ReopenThread(threadID string, body string, leaseID string) (coordinator.ReviewThread, error) {
	return c.verifyThread(threadID, "reopen", body, leaseID)
}

func (c *Client) verifyThread(threadID string, action string, body string, leaseID string) (coordinator.ReviewThread, error) {
	var response threadResponse
	if err := c.do(http.MethodPost, "/v1/threads/"+url.PathEscape(threadID)+"/"+action, threadCommentRequest{Body: body, LeaseID: leaseID}, nil, &response); err != nil {
		return coordinator.ReviewThread{}, err
	}

	return response.Thread, nil
}

func (c *Client) RegisterWorker(input RegisterWorkerInput) (flowworker.Worker, error) {
	var response workerResponse
	request := registerWorkerRequest{
		ID:                      input.ID,
		Labels:                  input.Labels,
		Taints:                  input.Taints,
		HarnessModels:           input.HarnessModels,
		CapacityPersistentAgent: input.CapacityPersistentAgent,
		CapacityEphemeral:       input.CapacityEphemeral,
		HeartbeatTTLSeconds:     durationSeconds(input.HeartbeatTTL),
	}
	if err := c.do(http.MethodPost, "/v1/workers/register", request, nil, &response); err != nil {
		return flowworker.Worker{}, err
	}

	return response.Worker, nil
}

func (c *Client) JoinWorker(input JoinWorkerInput) (JoinWorkerResult, error) {
	var response joinWorkerResponse
	request := joinWorkerRequest{WorkerID: input.WorkerID}
	if err := c.do(http.MethodPost, "/v1/workers/join", request, nil, &response); err != nil {
		return JoinWorkerResult{}, err
	}

	return JoinWorkerResult{
		WorkerID: response.WorkerID,
		Token:    response.Token,
	}, nil
}

func (c *Client) HeartbeatWorker(input HeartbeatWorkerInput) (flowworker.Worker, error) {
	var response workerResponse
	request := heartbeatWorkerRequest{
		WorkerID:            input.WorkerID,
		HeartbeatTTLSeconds: durationSeconds(input.HeartbeatTTL),
	}
	if err := c.do(http.MethodPost, "/v1/workers/heartbeat", request, nil, &response); err != nil {
		return flowworker.Worker{}, err
	}

	return response.Worker, nil
}

func (c *Client) ListWorkerReapJobs() ([]flowworker.Job, error) {
	var response jobsResponse
	if err := c.do(http.MethodGet, "/v1/workers/reap-jobs", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Jobs, nil
}

func (c *Client) ClaimJob(input ClaimJobInput) (ClaimJobResult, error) {
	var response claimJobResponse
	request := claimJobRequest{
		WorkerID:             input.WorkerID,
		Buckets:              input.Buckets,
		LeaseDurationSeconds: durationSeconds(input.LeaseDuration),
		WaitSeconds:          durationSeconds(input.Wait),
	}
	if err := c.do(http.MethodPost, "/v1/workers/claim", request, nil, &response); err != nil {
		return ClaimJobResult{}, err
	}

	return ClaimJobResult{
		Claimed:   response.Claimed,
		ProjectID: response.ProjectID,
		Job:       response.Job,
		Lease:     response.Lease,
	}, nil
}

func (c *Client) RenewLease(input RenewLeaseInput) (flowworker.Lease, error) {
	var response leaseResponse
	request := renewLeaseRequest{
		LeaseID:              input.LeaseID,
		LeaseDurationSeconds: durationSeconds(input.LeaseDuration),
	}
	if err := c.do(http.MethodPost, "/v1/workers/renew", request, nil, &response); err != nil {
		return flowworker.Lease{}, err
	}

	return response.Lease, nil
}

func (c *Client) WorkerJobStatus(ctx context.Context, input WorkerJobStatusInput) (WorkerJobStatusResult, error) {
	var response workerJobStatusResponse
	request := workerJobStatusRequest{LeaseID: input.LeaseID}
	if err := c.doContext(ctx, http.MethodPost, "/v1/workers/status", request, nil, &response); err != nil {
		return WorkerJobStatusResult{}, err
	}

	return WorkerJobStatusResult{
		Job:     response.Job,
		Lease:   response.Lease,
		Session: response.Session,
	}, nil
}

func (c *Client) MarkJobRunning(leaseID string) (MarkJobRunningResult, error) {
	var response jobResponse
	request := markJobRunningRequest{LeaseID: leaseID}
	if err := c.do(http.MethodPost, "/v1/workers/running", request, nil, &response); err != nil {
		return MarkJobRunningResult{}, err
	}

	return MarkJobRunningResult{
		Job:          response.Job,
		Change:       response.Change,
		Session:      response.Session,
		SessionToken: response.SessionToken,
	}, nil
}

func (c *Client) ReleaseLease(input ReleaseLeaseInput) (flowworker.Job, error) {
	var response jobResponse
	request := releaseLeaseRequest{
		LeaseID:    input.LeaseID,
		FinalState: string(input.FinalState),
	}
	if err := c.do(http.MethodPost, "/v1/workers/release", request, nil, &response); err != nil {
		return flowworker.Job{}, err
	}

	return response.Job, nil
}

func (c *Client) ReleaseConsole(ctx context.Context) error {
	var response consoleResponse
	return c.doContext(ctx, http.MethodDelete, c.consolePath(), nil, nil, &response)
}

func (c *Client) RegisterJobTerminal(ctx context.Context, jobID string, leaseID string, targetURL string, tmuxSocketPath string) (coordinator.JobTerminal, error) {
	var response jobTerminalResponse
	request := jobTerminalRequest{
		LeaseID:        leaseID,
		TargetURL:      targetURL,
		TmuxSocketPath: tmuxSocketPath,
	}
	if err := c.doContext(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(jobID)+"/terminal", request, nil, &response); err != nil {
		return coordinator.JobTerminal{}, err
	}

	return response.Terminal, nil
}

func (c *Client) ListWorkers() ([]flowworker.Worker, error) {
	var response workersResponse
	if err := c.do(http.MethodGet, "/v1/workers", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Workers, nil
}

func (c *Client) ListJobs() ([]flowworker.Job, error) {
	var response jobsResponse
	if err := c.do(http.MethodGet, "/v1/jobs", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Jobs, nil
}

func (c *Client) EnqueueJob(input EnqueueJobInput) (flowworker.Job, error) {
	var response jobResponse
	request := enqueueJobRequest{
		IssueID:        input.IssueID,
		ChangeID:       input.ChangeID,
		Role:           string(input.Role),
		CapacityBucket: string(input.CapacityBucket),
		Priority:       input.Priority,
		RunsOn:         input.RunsOn,
		Requires:       input.Requires,
		Size:           input.Size,
		Tolerations:    input.Tolerations,
		Payload:        input.Payload,
	}
	if err := c.do(http.MethodPost, "/v1/jobs", request, nil, &response); err != nil {
		return flowworker.Job{}, err
	}

	return response.Job, nil
}

func (c *Client) JobAttach(jobID string) (terminal.AttachInfo, error) {
	var response attachResponse
	if err := c.do(http.MethodGet, "/v1/jobs/"+url.PathEscape(jobID)+"/attach", nil, nil, &response); err != nil {
		return terminal.AttachInfo{}, err
	}

	return response.Attach, nil
}

func (c *Client) UpdateSessionState(sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error) {
	return c.UpdateSessionStateContext(context.Background(), sessionID, state)
}

func (c *Client) UpdateSessionStateContext(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState) (coordinator.Session, error) {
	return c.UpdateSessionStateWithSourceContext(ctx, sessionID, state, "")
}

func (c *Client) UpdateSessionStateWithSourceContext(ctx context.Context, sessionID string, state coordinator.SessionRuntimeState, source string) (coordinator.Session, error) {
	var response sessionResponse
	request := sessionEventRequest{State: string(state), Source: strings.TrimSpace(source)}
	if err := c.doContext(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/event", request, nil, &response); err != nil {
		return coordinator.Session{}, err
	}

	return response.Session, nil
}

func (c *Client) ReportSessionSignal(ctx context.Context, sessionID string, input SessionSignalInput) (coordinator.Session, error) {
	var response sessionResponse
	request := sessionSignalRequest{
		Signal:        string(input.Signal),
		Source:        strings.TrimSpace(input.Source),
		Harness:       strings.TrimSpace(input.Harness),
		HookEventName: strings.TrimSpace(input.HookEventName),
		Details:       strings.TrimSpace(input.Details),
	}
	if err := c.doContext(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/signal", request, nil, &response); err != nil {
		return coordinator.Session{}, err
	}

	return response.Session, nil
}

func (c *Client) ReadySession(sessionID string) (coordinator.Session, error) {
	return c.ReadySessionWithInput(sessionID, ReadySessionInput{})
}

func (c *Client) ReadySessionWithInput(sessionID string, input ReadySessionInput) (coordinator.Session, error) {
	var response sessionResponse
	if err := c.do(http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/ready", readySessionRequest{HeadSHA: input.HeadSHA}, nil, &response); err != nil {
		return coordinator.Session{}, err
	}

	return response.Session, nil
}

func (c *Client) SessionAttach(sessionID string) (terminal.AttachInfo, error) {
	var response attachResponse
	if err := c.do(http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/attach", nil, nil, &response); err != nil {
		return terminal.AttachInfo{}, err
	}

	return response.Attach, nil
}

func (c *Client) RegisterSessionTerminal(ctx context.Context, sessionID string, targetURL string, tmuxSocketPath string) (coordinator.SessionTerminal, error) {
	var response sessionTerminalResponse
	request := sessionTerminalRequest{TargetURL: targetURL, TmuxSocketPath: tmuxSocketPath}
	if err := c.doContext(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/terminal", request, nil, &response); err != nil {
		return coordinator.SessionTerminal{}, err
	}

	return response.Terminal, nil
}

func (c *Client) CreateSessionTerminalAccess(sessionID string) (coordinator.SessionTerminalAccess, error) {
	var response sessionTerminalAccessResponse
	if err := c.do(http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/terminal-token", map[string]string{}, nil, &response); err != nil {
		return coordinator.SessionTerminalAccess{}, err
	}

	return response.Access, nil
}

func (c *Client) URLForPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return c.baseURL
	}
	if strings.HasPrefix(path, "/") {
		return c.baseURL + path
	}

	return c.baseURL + "/" + path
}

func (c *Client) WriteSessionStatus(sessionID string, message string, kind string) (coordinator.StatusLogEntry, error) {
	var response statusResponse
	request := sessionStatusRequest{Message: message, Kind: kind}
	if err := c.do(http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/status", request, nil, &response); err != nil {
		return coordinator.StatusLogEntry{}, err
	}

	return response.Status, nil
}

func (c *Client) ReportSessionProcessExit(ctx context.Context, input ReportSessionProcessExitInput) (coordinator.Session, error) {
	var response sessionResponse
	request := sessionProcessExitRequest{LeaseID: input.LeaseID, ExitCode: input.ExitCode}
	if err := c.doContext(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(input.SessionID)+"/process-exit", request, nil, &response); err != nil {
		return coordinator.Session{}, err
	}

	return response.Session, nil
}

func (c *Client) ListPendingSessionMessages(ctx context.Context, input ListPendingSessionMessagesInput) ([]coordinator.SessionMessage, error) {
	var response sessionMessagesResponse
	query := url.Values{}
	query.Set("lease_id", input.LeaseID)
	if input.Limit > 0 {
		query.Set("limit", strconv.Itoa(input.Limit))
	}
	path := "/v1/sessions/" + url.PathEscape(input.SessionID) + "/messages"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	if err := c.doContext(ctx, http.MethodGet, path, nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Messages, nil
}

func (c *Client) MarkSessionMessageDelivered(ctx context.Context, input MarkSessionMessageDeliveredInput) (coordinator.SessionMessage, error) {
	var response sessionMessageResponse
	request := sessionMessageDeliveredRequest{LeaseID: input.LeaseID}
	path := "/v1/sessions/" + url.PathEscape(input.SessionID) + "/messages/" + url.PathEscape(input.MessageID) + "/delivered"
	if err := c.doContext(ctx, http.MethodPost, path, request, nil, &response); err != nil {
		return coordinator.SessionMessage{}, err
	}

	return response.Message, nil
}

func (c *Client) ApprovePlan(issueID string) (coordinator.Issue, error) {
	var response issueResponse
	if err := c.do(http.MethodPost, "/v1/issues/"+url.PathEscape(issueID)+"/plan/approve", map[string]string{}, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) RejectPlan(issueID string, input RejectPlanInput) (coordinator.Issue, error) {
	var response issueResponse
	request := planRejectRequest{Comments: input.Comments}
	if err := c.do(http.MethodPost, "/v1/issues/"+url.PathEscape(issueID)+"/plan/reject", request, nil, &response); err != nil {
		return coordinator.Issue{}, err
	}

	return response.Issue, nil
}

func (c *Client) ReplyToIssue(issueID string, input ReplyToIssueInput) (coordinator.SessionMessage, bool, error) {
	var response sessionMessageResponse
	request := attentionReplyRequest{Message: input.Message, StatusLogID: input.StatusLogID}
	if err := c.do(http.MethodPost, "/v1/issues/"+url.PathEscape(issueID)+"/attention/reply", request, nil, &response); err != nil {
		return coordinator.SessionMessage{}, false, err
	}

	return response.Message, response.Queued, nil
}

func (c *Client) Reconcile() (coordinator.ReconcileResult, error) {
	var response reconcileResponse
	if err := c.do(http.MethodPost, "/v1/reconcile", map[string]string{}, nil, &response); err != nil {
		return coordinator.ReconcileResult{}, err
	}

	return response.Result, nil
}

// PutHandoff eagerly syncs the handoff content for a change to the coordinator,
// which records it as a snapshot. Git remains the durable source of truth: a
// later reconcile pass still overwrites the snapshot from the branch ref.
func (c *Client) PutHandoff(changeID string, input PutHandoffInput) (PutHandoffResult, error) {
	var response handoffResponse
	request := putHandoffRequest{Content: input.Content, HeadSHA: input.HeadSHA}
	if err := c.do(http.MethodPut, "/v1/changes/"+url.PathEscape(changeID)+"/handoff", request, nil, &response); err != nil {
		return PutHandoffResult{}, err
	}

	return PutHandoffResult{
		ChangeID: response.ChangeID,
		HeadSHA:  response.HeadSHA,
		Present:  response.Present,
		Valid:    response.Valid,
		Summary:  response.Summary,
	}, nil
}

// GetHandoff fetches the coordinator's current handoff snapshot for a change,
// including the full body. The session builder uses it to inject the prior
// handoff into the next author (fix round) and verifier prompt. leaseID proves
// the caller's live lease for worker (reviewer/verifier) tokens; it is ignored
// for owner/session tokens and may be empty. found is false when no handoff
// snapshot exists yet (a fresh change), which callers treat as a normal empty
// case rather than an error.
func (c *Client) GetHandoff(changeID string, leaseID string) (PutHandoffResult, string, bool, error) {
	var query url.Values
	if leaseID = strings.TrimSpace(leaseID); leaseID != "" {
		query = url.Values{"lease_id": {leaseID}}
	}
	var response handoffResponse
	err := c.do(http.MethodGet, "/v1/changes/"+url.PathEscape(changeID)+"/handoff", nil, query, &response)
	if err != nil {
		var statusErr *HTTPStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return PutHandoffResult{}, "", false, nil
		}
		return PutHandoffResult{}, "", false, err
	}

	return PutHandoffResult{
		ChangeID: response.ChangeID,
		HeadSHA:  response.HeadSHA,
		Present:  response.Present,
		Valid:    response.Valid,
		Summary:  response.Summary,
	}, response.Content, true, nil
}

// UploadSessionTranscript PUTs raw transcript bytes for an author session. The
// caller (the worker) supplies the trailing bytes of the tmux transcript log.
func (c *Client) UploadSessionTranscript(ctx context.Context, sessionID string, r io.Reader) error {
	return c.doRaw(ctx, http.MethodPut, "/v1/sessions/"+url.PathEscape(sessionID)+"/transcript", nil, r)
}

// UploadJobTranscript PUTs raw transcript bytes for a check job, proving a live
// lease via the lease_id query parameter (mirroring the worker check-report
// scope).
func (c *Client) UploadJobTranscript(ctx context.Context, jobID string, leaseID string, r io.Reader) error {
	query := url.Values{}
	query.Set("lease_id", leaseID)
	return c.doRaw(ctx, http.MethodPut, "/v1/jobs/"+url.PathEscape(jobID)+"/transcript", query, r)
}

// SessionTranscript GETs an author session's stored transcript (owner scope).
func (c *Client) SessionTranscript(sessionID string) (string, error) {
	return c.getText("/v1/sessions/" + url.PathEscape(sessionID) + "/transcript")
}

// JobTranscript GETs a job's stored transcript (owner scope).
func (c *Client) JobTranscript(jobID string) (string, error) {
	return c.getText("/v1/jobs/" + url.PathEscape(jobID) + "/transcript")
}

func (c *Client) do(method string, path string, body any, query url.Values, target any) error {
	return c.doContext(context.Background(), method, path, body, query, target)
}

// doRaw sends a non-JSON request body (used for transcript uploads). It applies
// the same auth and protocol headers as do but streams the body verbatim and
// expects an empty response.
// statusError returns an error describing a non-2xx response, decoding the JSON
// error envelope when present, or nil for a 2xx response.
func statusError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	var apiError errorResponse
	if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
		return &HTTPStatusError{StatusCode: response.StatusCode}
	}
	return &HTTPStatusError{
		StatusCode: response.StatusCode,
		Code:       apiError.Error.Code,
		Message:    apiError.Error.Message,
	}
}

func (c *Client) doRaw(ctx context.Context, method string, path string, query url.Values, body io.Reader) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, c.protocolVersion)
	request.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if err := statusError(response); err != nil {
		return err
	}

	return nil
}

func (c *Client) doMultipart(method string, path string, query url.Values, contentType string, body io.Reader, target any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, c.protocolVersion)
	request.Header.Set("Content-Type", contentType)
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if err := statusError(response); err != nil {
		return err
	}
	if target == nil {
		return nil
	}

	return json.NewDecoder(response.Body).Decode(target)
}

// getText GETs a text/plain resource and returns its body.
func (c *Client) getText(path string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set(protocolHeader, c.protocolVersion)
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if err := statusError(response); err != nil {
		return "", err
	}

	data, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (c *Client) doContext(ctx context.Context, method string, path string, body any, query url.Values, target any) error {
	var requestBody io.Reader
	if body != nil {
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return err
		}
		requestBody = &encoded
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, c.protocolVersion)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}

	started := time.Now()
	slog.Debug("flow api request", "method", method, "path", path)
	response, err := c.httpClient.Do(request)
	if err != nil {
		slog.Debug("flow api request failed", "method", method, "path", path, "duration", time.Since(started), "error", err)
		return err
	}
	defer response.Body.Close()
	slog.Debug("flow api response", "method", method, "path", path, "status", response.StatusCode, "duration", time.Since(started))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError errorResponse
		if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
			slog.Debug("flow api error response decode failed", "method", method, "path", path, "status", response.StatusCode, "error", err)
			return &HTTPStatusError{StatusCode: response.StatusCode}
		}
		slog.Debug("flow api error response", "method", method, "path", path, "status", response.StatusCode, "code", apiError.Error.Code)
		return &HTTPStatusError{
			StatusCode: response.StatusCode,
			Code:       apiError.Error.Code,
			Message:    apiError.Error.Message,
		}
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		slog.Debug("flow api response decode failed", "method", method, "path", path, "status", response.StatusCode, "error", err)
		return err
	}

	return nil
}

type HTTPStatusError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if strings.TrimSpace(e.Code) == "" {
		return fmt.Sprintf("request failed with status %d", e.StatusCode)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func IsRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}

	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusRequestTimeout ||
			statusErr.StatusCode == http.StatusTooManyRequests ||
			statusErr.StatusCode >= http.StatusInternalServerError
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}

func durationSeconds(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}

	return int(duration / time.Second)
}

type CreateIssueInput struct {
	Title               string           `json:"title"`
	Body                string           `json:"body"`
	AcceptanceCriteria  string           `json:"acceptance_criteria"`
	Priority            int              `json:"priority"`
	RequiresHumanReview *bool            `json:"requires_human_review,omitempty"`
	AutoMerge           *bool            `json:"auto_merge,omitempty"`
	PlanMode            bool             `json:"plan_mode,omitempty"`
	AgentHarness        string           `json:"agent_harness,omitempty"`
	HarnessArgs         flowharness.Args `json:"harness_args,omitempty"`
}

type EditIssueInput struct {
	Title               *string                `json:"title,omitempty"`
	Body                *string                `json:"body,omitempty"`
	AcceptanceCriteria  *string                `json:"acceptance_criteria,omitempty"`
	Priority            *int                   `json:"priority,omitempty"`
	RequiresHumanReview *bool                  `json:"requires_human_review,omitempty"`
	AutoMerge           *bool                  `json:"auto_merge,omitempty"`
	PlanMode            *bool                  `json:"plan_mode,omitempty"`
	AgentHarness        *string                `json:"agent_harness,omitempty"`
	HarnessArgs         *flowharness.ArgsPatch `json:"harness_args,omitempty"`
}

type UploadIssueAttachmentInput struct {
	Stage       coordinator.IssueAttachmentStage
	Filename    string
	ContentType string
	Reader      io.Reader
	LeaseID     string
}

type IssueFilter struct {
	ScheduleStates []coordinator.ScheduleState
	TriageStates   []coordinator.TriageState
	TagSlugs       []string
}

type RegisterWorkerInput struct {
	ID                      string
	Labels                  map[string]string
	Taints                  []scheduler.Taint
	HarnessModels           []flowharness.Model
	CapacityPersistentAgent int
	CapacityEphemeral       int
	HeartbeatTTL            time.Duration
}

type JoinWorkerInput struct {
	WorkerID string
}

type JoinWorkerResult struct {
	WorkerID string
	Token    string
}

type HeartbeatWorkerInput struct {
	WorkerID     string
	HeartbeatTTL time.Duration
}

type ClaimJobInput struct {
	WorkerID      string
	Buckets       []flowworker.CapacityBucket
	LeaseDuration time.Duration
	Wait          time.Duration
}

type ClaimJobResult struct {
	Claimed   bool
	ProjectID string
	Job       *flowworker.Job
	Lease     *flowworker.Lease
}

type MarkJobRunningResult struct {
	Job          flowworker.Job
	Change       *coordinator.Change
	Session      *coordinator.Session
	SessionToken string
}

type RenewLeaseInput struct {
	LeaseID       string
	LeaseDuration time.Duration
}

type WorkerJobStatusInput struct {
	LeaseID string
}

type WorkerJobStatusResult struct {
	Job     flowworker.Job
	Lease   flowworker.Lease
	Session *coordinator.Session
}

type ReleaseLeaseInput struct {
	LeaseID    string
	FinalState flowworker.JobState
}

type EnqueueJobInput struct {
	IssueID        *string
	ChangeID       *string
	Role           flowworker.JobRole
	CapacityBucket flowworker.CapacityBucket
	Priority       int
	RunsOn         map[string]string
	Requires       []string
	Size           string
	Tolerations    []scheduler.Toleration
	Payload        map[string]any
}

type WebBootstrapResult struct {
	LoginPath string
	ExpiresAt time.Time
}

type ReportCheckInput struct {
	Kind        coordinator.CheckKind
	Required    *bool
	Verdict     coordinator.CheckVerdict
	ExitCode    *int
	Details     string
	SourceJobID *string
	LeaseID     *string
	Reporter    string
}

type CheckResult struct {
	Check            coordinator.Check
	ReviewState      coordinator.ReviewState
	FollowUpFailures []CheckFollowUpFailure
}

type CheckFollowUpFailure = contract.CheckFollowUpFailure

type CheckListResult struct {
	Checks      []coordinator.Check
	ReviewState coordinator.ReviewState
}

type ReviewRunResult struct {
	Change      coordinator.Change
	Scheduled   coordinator.ScheduleReviewRoundResult
	Checks      []coordinator.Check
	ReviewState coordinator.ReviewState
}

type CreateThreadInput struct {
	AnchorCommitSHA string
	FilePath        string
	Line            int
	Context         string
	Body            string
	LeaseID         string
}

type ClaimThreadInput struct {
	Kind           coordinator.ReviewClaimKind
	Body           string
	ClaimCommitSHA string
	LeaseID        string
}

type ReadySessionInput struct {
	HeadSHA string
}

type ReportSessionProcessExitInput struct {
	SessionID string
	LeaseID   string
	ExitCode  int
}

type ListPendingSessionMessagesInput struct {
	SessionID string
	LeaseID   string
	Limit     int
}

type MarkSessionMessageDeliveredInput struct {
	SessionID string
	MessageID string
	LeaseID   string
}

type RejectPlanInput struct {
	Comments string
}

type ReplyToIssueInput struct {
	Message     string
	StatusLogID *int64
}

type SessionSignalInput struct {
	Signal        coordinator.SessionSignalKind
	Source        string
	Harness       string
	HookEventName string
	Details       string
}

type PutHandoffInput struct {
	Content string
	HeadSHA string
}

type PutHandoffResult struct {
	ChangeID string
	HeadSHA  string
	Present  bool
	Valid    bool
	Summary  string
}

type scheduleIssueRequest = contract.ScheduleIssueRequest
type issueStateRequest = contract.IssueStateRequest
type triageIssueRequest = contract.TriageIssueRequest
type issueRelationRequest = contract.IssueRelationRequest
type registerWorkerRequest = contract.RegisterWorkerRequest
type joinWorkerRequest = contract.JoinWorkerRequest
type heartbeatWorkerRequest = contract.HeartbeatWorkerRequest
type claimJobRequest = contract.ClaimJobRequest
type renewLeaseRequest = contract.RenewLeaseRequest
type workerJobStatusRequest = contract.WorkerJobStatusRequest
type markJobRunningRequest = contract.MarkJobRunningRequest
type releaseLeaseRequest = contract.ReleaseLeaseRequest
type enqueueJobRequest = contract.EnqueueJobRequest
type reportCheckRequest = contract.ReportCheckRequest
type sessionEventRequest = contract.SessionEventRequest
type sessionSignalRequest = contract.SessionSignalRequest
type readySessionRequest = contract.ReadySessionRequest
type sessionStatusRequest = contract.SessionStatusRequest
type sessionProcessExitRequest = contract.SessionProcessExitRequest
type sessionMessageDeliveredRequest = contract.SessionMessageDeliveredRequest
type planRejectRequest = contract.PlanRejectRequest
type attentionReplyRequest = contract.AttentionReplyRequest
type sessionTerminalRequest = contract.SessionTerminalRequest
type jobTerminalRequest = contract.JobTerminalRequest
type createThreadRequest = contract.CreateThreadRequest
type putHandoffRequest = contract.PutHandoffRequest
type threadCommentRequest = contract.ThreadCommentRequest
type threadClaimRequest = contract.ThreadClaimRequest
type issueResponse = contract.IssueResponse
type issuesResponse = contract.IssuesResponse
type issueAttachmentResponse = contract.IssueAttachmentResponse
type issueAttachmentsResponse = contract.IssueAttachmentsResponse
type boardResponse = contract.BoardResponse
type aggregateBoardResponse = contract.AggregateBoardResponse
type consoleResponse = contract.ConsoleResponse
type projectResponse = contract.ProjectResponse
type projectsResponse = contract.ProjectsResponse
type harnessesResponse = contract.HarnessesResponse
type mergeResponse = contract.MergeResponse
type webBootstrapResponse = contract.WebBootstrapResponse
type checkResponse = contract.CheckResponse
type transitionsResponse = contract.TransitionsResponse
type checksResponse = contract.ChecksResponse
type reviewRunResponse = contract.ReviewRunResponse
type workerResponse = contract.WorkerResponse
type joinWorkerResponse = contract.JoinWorkerResponse
type workersResponse = contract.WorkersResponse
type jobResponse = contract.JobResponse
type jobsResponse = contract.JobsResponse
type leaseResponse = contract.LeaseResponse
type workerJobStatusResponse = contract.WorkerJobStatusResponse
type claimJobResponse = contract.ClaimJobResponse
type sessionResponse = contract.SessionResponse
type attachResponse = contract.AttachResponse
type sessionTerminalResponse = contract.SessionTerminalResponse
type sessionTerminalAccessResponse = contract.SessionTerminalAccessResponse
type jobTerminalResponse = contract.JobTerminalResponse
type threadResponse = contract.ThreadResponse
type handoffResponse = contract.HandoffResponse
type threadsResponse = contract.ThreadsResponse
type statusResponse = contract.StatusResponse
type sessionMessagesResponse = contract.SessionMessagesResponse
type sessionMessageResponse = contract.SessionMessageResponse
type reconcileResponse = contract.ReconcileResponse
type errorResponse = contract.ErrorResponse
