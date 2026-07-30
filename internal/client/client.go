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
	baseURL    string
	token      string
	projectID  string
	httpClient *http.Client
}

func New(cfg config.ClientConfig) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("server URL is required")
	}
	return &Client{
		baseURL:    baseURL,
		token:      cfg.Token,
		httpClient: http.DefaultClient,
	}, nil
}

// WithProject returns a client whose task and board calls target the given
// project's scoped routes. Without a project the unscoped routes apply: the
// coordinator resolves session tokens to their bound project and treats a
// single-project deployment as implicit.
func (c *Client) WithProject(projectID string) *Client {
	clone := *c
	clone.projectID = strings.TrimSpace(projectID)
	return &clone
}

// tasksPath scopes an task route to the client's project when one is set.
func (c *Client) tasksPath(suffix string) string {
	if c.projectID != "" {
		return "/v2/projects/" + url.PathEscape(c.projectID) + "/tasks" + suffix
	}

	return "/v2/tasks" + suffix
}

// projectPath scopes an arbitrary project-owned route ("/flows",
// "/agent-defs/...") to the client's project when one is set.
func (c *Client) projectPath(suffix string) string {
	if c.projectID != "" {
		return "/v2/projects/" + url.PathEscape(c.projectID) + suffix
	}

	return "/v2" + suffix
}

func (c *Client) consolePath() string {
	if c.projectID != "" {
		return "/v2/projects/" + url.PathEscape(c.projectID) + "/console"
	}

	return "/v2/console"
}

func (c *Client) CreateTask(input CreateTaskInput) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPost, c.tasksPath(""), input, nil, &response); err != nil {
		return coordinator.Task{}, err
	}

	return response.Task, nil
}

func (c *Client) ListTasks(filter TaskFilter) ([]coordinator.Task, error) {
	query := url.Values{}
	for _, state := range filter.LifecycleStates {
		query.Add("state", state)
	}
	for _, tag := range filter.TagSlugs {
		query.Add("tag", tag)
	}

	var response tasksResponse
	if err := c.do(http.MethodGet, c.tasksPath(""), nil, query, &response); err != nil {
		return nil, err
	}

	return response.Tasks, nil
}

func (c *Client) GetTask(id string) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(id)), nil, nil, &response); err != nil {
		return coordinator.Task{}, err
	}

	return response.Task, nil
}

// ListAgentDefs returns the project's effective agent definition catalog,
// including inherited global definitions not overridden by name.
func (c *Client) ListAgentDefs() ([]coordinator.AgentDef, error) {
	var response struct {
		AgentDefs []coordinator.AgentDef `json:"agent_defs"`
	}
	if err := c.do(http.MethodGet, c.projectPath("/agent-defs"), nil, nil, &response); err != nil {
		return nil, err
	}
	return response.AgentDefs, nil
}

func (c *Client) CreateAgentDef(input coordinator.AgentDefInput) (coordinator.AgentDef, error) {
	var response struct {
		AgentDef coordinator.AgentDef `json:"agent_def"`
	}
	if err := c.do(http.MethodPost, c.projectPath("/agent-defs"), input, nil, &response); err != nil {
		return coordinator.AgentDef{}, err
	}
	return response.AgentDef, nil
}

func (c *Client) UpdateAgentDef(id string, input coordinator.AgentDefInput) (coordinator.AgentDef, error) {
	var response struct {
		AgentDef coordinator.AgentDef `json:"agent_def"`
	}
	if err := c.do(http.MethodPatch, c.projectPath("/agent-defs/"+url.PathEscape(id)), input, nil, &response); err != nil {
		return coordinator.AgentDef{}, err
	}
	return response.AgentDef, nil
}

func (c *Client) DeleteAgentDef(id string) error {
	return c.do(http.MethodDelete, c.projectPath("/agent-defs/"+url.PathEscape(id)), nil, nil, nil)
}

func (c *Client) ListGlobalAgentDefs() ([]coordinator.AgentDef, error) {
	var response struct {
		AgentDefs []coordinator.AgentDef `json:"agent_defs"`
	}
	if err := c.do(http.MethodGet, "/v2/global/agent-defs", nil, nil, &response); err != nil {
		return nil, err
	}
	return response.AgentDefs, nil
}

func (c *Client) CreateGlobalAgentDef(input coordinator.AgentDefInput) (coordinator.AgentDef, error) {
	var response struct {
		AgentDef coordinator.AgentDef `json:"agent_def"`
	}
	if err := c.do(http.MethodPost, "/v2/global/agent-defs", input, nil, &response); err != nil {
		return coordinator.AgentDef{}, err
	}
	return response.AgentDef, nil
}

func (c *Client) UpdateGlobalAgentDef(id string, input coordinator.AgentDefInput) (coordinator.AgentDef, error) {
	var response struct {
		AgentDef coordinator.AgentDef `json:"agent_def"`
	}
	if err := c.do(http.MethodPatch, "/v2/global/agent-defs/"+url.PathEscape(id), input, nil, &response); err != nil {
		return coordinator.AgentDef{}, err
	}
	return response.AgentDef, nil
}

func (c *Client) DeleteGlobalAgentDef(id string) error {
	return c.do(http.MethodDelete, "/v2/global/agent-defs/"+url.PathEscape(id), nil, nil, nil)
}

// FlowsList is the flows collection response: the catalog plus the project
// default flow id.
type FlowsList struct {
	Flows         []coordinator.Flow `json:"flows"`
	DefaultFlowID string             `json:"default_flow_id,omitempty"`
}

func (c *Client) ListFlows() (FlowsList, error) {
	var response FlowsList
	if err := c.do(http.MethodGet, c.projectPath("/flows"), nil, nil, &response); err != nil {
		return FlowsList{}, err
	}
	return response, nil
}

func (c *Client) CreateFlow(input coordinator.FlowInput) (coordinator.Flow, error) {
	var response struct {
		Flow coordinator.Flow `json:"flow"`
	}
	if err := c.do(http.MethodPost, c.projectPath("/flows"), input, nil, &response); err != nil {
		return coordinator.Flow{}, err
	}
	return response.Flow, nil
}

func (c *Client) UpdateFlow(id string, input coordinator.FlowInput) (coordinator.Flow, error) {
	var response struct {
		Flow coordinator.Flow `json:"flow"`
	}
	if err := c.do(http.MethodPatch, c.projectPath("/flows/"+url.PathEscape(id)), input, nil, &response); err != nil {
		return coordinator.Flow{}, err
	}
	return response.Flow, nil
}

func (c *Client) DeleteFlow(id string) error {
	return c.do(http.MethodDelete, c.projectPath("/flows/"+url.PathEscape(id)), nil, nil, nil)
}

func (c *Client) SetDefaultFlow(id string) (coordinator.Flow, error) {
	var response struct {
		Flow coordinator.Flow `json:"flow"`
	}
	if err := c.do(http.MethodPost, c.projectPath("/flows/"+url.PathEscape(id)+"/default"), nil, nil, &response); err != nil {
		return coordinator.Flow{}, err
	}
	return response.Flow, nil
}

// ApproveWorkPhase approves an task's gate-paused work phase.
func (c *Client) ApproveWorkPhase(taskID string) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/phase/approve", map[string]string{}, nil, &response); err != nil {
		return coordinator.Task{}, err
	}
	return response.Task, nil
}

// RequestWorkPhaseChanges sends a gate-paused work phase back to rework.
func (c *Client) RequestWorkPhaseChanges(taskID string, feedback string) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/phase/request-changes", map[string]string{"feedback": feedback}, nil, &response); err != nil {
		return coordinator.Task{}, err
	}
	return response.Task, nil
}

// PromptContext is the coordinator-resolved per-phase prompt material: the
// current work phase's role instructions, human gate feedback, and completed
// prior-phase handoffs.
type PromptContext struct {
	RoleInstructions string                               `json:"role_instructions,omitempty"`
	PhaseName        string                               `json:"phase_name,omitempty"`
	WorkspaceMode    coordinator.WorkspaceMode            `json:"workspace_mode,omitempty"`
	ArtifactKind     coordinator.ArtifactKind             `json:"artifact_kind,omitempty"`
	PhaseIndex       int                                  `json:"phase_index"`
	FinalPhase       bool                                 `json:"final_phase"`
	GateFeedback     string                               `json:"gate_feedback,omitempty"`
	PriorHandoffs    []PromptPhaseHandoff                 `json:"prior_handoffs,omitempty"`
	TaskSetWorkflow  *coordinator.TaskSetWorkflowContract `json:"task_set_workflow,omitempty"`
}

type PromptPhaseHandoff struct {
	PhaseName string `json:"phase_name"`
	Content   string `json:"content"`
}

// GetPromptContext resolves the prompt material for the task's current work
// phase; a non-empty checkName resolves the named review check's agent prompt
// instead.
func (c *Client) GetPromptContext(taskID string, checkName string) (PromptContext, error) {
	var query url.Values
	if strings.TrimSpace(checkName) != "" {
		query = url.Values{"check": []string{strings.TrimSpace(checkName)}}
	}
	var response PromptContext
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(taskID))+"/prompt-context", nil, query, &response); err != nil {
		return PromptContext{}, err
	}
	return response, nil
}

func (c *Client) GetTaskWithStatus(id string) (coordinator.Task, []coordinator.StatusLogEntry, error) {
	var response taskResponse
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(id)), nil, nil, &response); err != nil {
		return coordinator.Task{}, nil, err
	}

	return response.Task, response.StatusLog, nil
}

func (c *Client) EditTask(id string, input EditTaskInput) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPatch, c.tasksPath("/"+url.PathEscape(id)), input, nil, &response); err != nil {
		return coordinator.Task{}, err
	}

	return response.Task, nil
}

func (c *Client) ScheduleTask(id string, state coordinator.ScheduleState) (coordinator.Task, error) {
	if _, err := c.ScheduleWorkflow(id); err != nil {
		return coordinator.Task{}, err
	}
	task, _, err := c.GetTaskWithStatus(id)
	return task, err
}

type WorkflowDetail struct {
	Detail    coordinator.WorkflowRunDetail  `json:"detail"`
	Artifacts []coordinator.WorkflowArtifact `json:"artifacts"`
}

func (c *Client) ScheduleWorkflow(id string) (coordinator.WorkflowRun, error) {
	var response struct {
		Run coordinator.WorkflowRun `json:"run"`
	}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/schedule", map[string]string{}, nil, &response); err != nil {
		return coordinator.WorkflowRun{}, err
	}
	return response.Run, nil
}

func (c *Client) GetWorkflow(id string) (WorkflowDetail, error) {
	var response WorkflowDetail
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(id))+"/workflow", nil, nil, &response); err != nil {
		return WorkflowDetail{}, err
	}
	return response, nil
}

func (c *Client) CreateWorkflowArtifact(taskID string, input coordinator.CreateWorkflowArtifactInput) (coordinator.WorkflowArtifact, bool, error) {
	var response struct {
		Artifact coordinator.WorkflowArtifact `json:"artifact"`
		Replayed bool                         `json:"replayed"`
	}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/artifacts", input, nil, &response); err != nil {
		return coordinator.WorkflowArtifact{}, false, err
	}
	return response.Artifact, response.Replayed, nil
}

func (c *Client) CompleteWorkflowAgentNode(taskID, nodeRunID, artifactID string) (coordinator.CompleteWorkflowNodeResult, error) {
	var response coordinator.CompleteWorkflowNodeResult
	request := map[string]string{"node_run_id": nodeRunID, "artifact_id": artifactID}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/complete", request, nil, &response); err != nil {
		return coordinator.CompleteWorkflowNodeResult{}, err
	}
	return response, nil
}

// SubmitForReview parks the active agent node on a human review of the
// artifact instead of completing it: the session stays alive while the human
// decides, and GetReviewStatus reports the verdict.
func (c *Client) SubmitForReview(taskID, nodeRunID, artifactID string) error {
	var response struct {
		Wait coordinator.WorkflowWait `json:"wait"`
	}
	request := map[string]string{"node_run_id": nodeRunID, "artifact_id": artifactID}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/submit-review", request, nil, &response); err != nil {
		return err
	}
	return nil
}

func (c *Client) GetReviewStatus(taskID, nodeRunID string) (coordinator.ReviewStatusResult, error) {
	var response coordinator.ReviewStatusResult
	path := c.tasksPath("/"+url.PathEscape(taskID)) + "/workflow/review?node_run_id=" + url.QueryEscape(nodeRunID)
	if err := c.do(http.MethodGet, path, nil, nil, &response); err != nil {
		return coordinator.ReviewStatusResult{}, err
	}
	return response, nil
}

func (c *Client) RespondWorkflow(taskID, nodeRunID, outcome, feedback string) (coordinator.CompleteWorkflowNodeResult, error) {
	var response coordinator.CompleteWorkflowNodeResult
	request := map[string]string{"node_run_id": nodeRunID, "outcome": outcome, "feedback": feedback}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/respond", request, nil, &response); err != nil {
		return coordinator.CompleteWorkflowNodeResult{}, err
	}
	return response, nil
}

func (c *Client) ExtendWorkflowBudget(taskID string, additional int) (coordinator.WorkflowRun, error) {
	var response struct {
		Run coordinator.WorkflowRun `json:"run"`
	}
	request := map[string]int{"additional": additional}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/budget", request, nil, &response); err != nil {
		return coordinator.WorkflowRun{}, err
	}
	return response.Run, nil
}

func (c *Client) RetryWorkflow(taskID string, refreshAgentRuntime bool) (coordinator.WorkflowRun, error) {
	var response struct {
		Run coordinator.WorkflowRun `json:"run"`
	}
	request := struct {
		RefreshAgentRuntime bool `json:"refresh_agent_runtime,omitempty"`
	}{RefreshAgentRuntime: refreshAgentRuntime}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/retry", request, nil, &response); err != nil {
		return coordinator.WorkflowRun{}, err
	}
	return response.Run, nil
}

func (c *Client) SetTaskState(id string, state coordinator.TaskState) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/state", taskStateRequest{State: string(state)}, nil, &response); err != nil {
		return coordinator.Task{}, err
	}

	return response.Task, nil
}

func (c *Client) ResetTask(id string) (coordinator.Task, error) {
	if _, err := c.ResetWorkflow(id); err != nil {
		return coordinator.Task{}, err
	}
	task, _, err := c.GetTaskWithStatus(id)
	return task, err
}

func (c *Client) ResetWorkflow(id string) (coordinator.WorkflowRun, error) {
	var response struct {
		Run coordinator.WorkflowRun `json:"run"`
	}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/reset", map[string]string{}, nil, &response); err != nil {
		return coordinator.WorkflowRun{}, err
	}
	return response.Run, nil
}

func (c *Client) ForceDone(id string, resolution coordinator.DoneResolution, note string) (coordinator.Task, error) {
	var response taskResponse
	request := map[string]string{"resolution": string(resolution), "note": note}
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/done", request, nil, &response); err != nil {
		return coordinator.Task{}, err
	}
	return response.Task, nil
}

func (c *Client) ReopenTask(id string) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/reopen", map[string]string{}, nil, &response); err != nil {
		return coordinator.Task{}, err
	}
	return response.Task, nil
}

func (c *Client) CloseTask(id string) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/close", map[string]string{}, nil, &response); err != nil {
		return coordinator.Task{}, err
	}

	return response.Task, nil
}

func (c *Client) TriageTask(id string, state coordinator.TriageState) (coordinator.Task, error) {
	var response taskResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/triage", triageTaskRequest{State: string(state)}, nil, &response); err != nil {
		return coordinator.Task{}, err
	}

	return response.Task, nil
}

func (c *Client) LinkTasks(sourceTaskID string, kind coordinator.RelationKind, targetTaskID string) error {
	request := taskRelationRequest{
		TargetTaskID: targetTaskID,
		Kind:         string(kind),
	}
	return c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(sourceTaskID))+"/relations", request, nil, nil)
}

func (c *Client) UnlinkTasks(sourceTaskID string, kind coordinator.RelationKind, targetTaskID string) error {
	request := taskRelationRequest{
		TargetTaskID: targetTaskID,
		Kind:         string(kind),
	}
	return c.do(http.MethodDelete, c.tasksPath("/"+url.PathEscape(sourceTaskID))+"/relations", request, nil, nil)
}

func (c *Client) GetTaskRelations(id string) ([]coordinator.TaskRelation, error) {
	var response taskRelationsResponse
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(id))+"/relations", nil, nil, &response); err != nil {
		return nil, err
	}
	return response.Relations, nil
}

func (c *Client) ApplyReviewFollowUp(sourceTaskID string, input ApplyReviewFollowUpInput) (ApplyReviewFollowUpResult, error) {
	var response contract.ApplyReviewFollowUpResponse
	if err := c.do(
		http.MethodPost,
		c.tasksPath("/"+url.PathEscape(sourceTaskID))+"/review-follow-ups",
		contract.ApplyReviewFollowUpRequest(input),
		nil,
		&response,
	); err != nil {
		return ApplyReviewFollowUpResult{}, err
	}
	return ApplyReviewFollowUpResult{
		Task:        response.Task,
		Disposition: response.Disposition,
	}, nil
}

func (c *Client) MergeTask(id string) (coordinator.MergeResult, error) {
	var response mergeResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(id))+"/merge", map[string]string{}, nil, &response); err != nil {
		return coordinator.MergeResult{}, err
	}

	return response.Merge, nil
}

func (c *Client) UploadTaskAttachment(taskID string, input UploadTaskAttachmentInput) (coordinator.TaskAttachment, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if input.Stage != "" {
		if err := writer.WriteField("stage", string(input.Stage)); err != nil {
			return coordinator.TaskAttachment{}, err
		}
	}
	filename := strings.TrimSpace(input.Filename)
	if filename == "" {
		return coordinator.TaskAttachment{}, errors.New("attachment filename is required")
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
		return coordinator.TaskAttachment{}, err
	}
	if input.Reader == nil {
		return coordinator.TaskAttachment{}, errors.New("attachment reader is required")
	}
	if _, err := io.Copy(part, input.Reader); err != nil {
		return coordinator.TaskAttachment{}, err
	}
	if err := writer.Close(); err != nil {
		return coordinator.TaskAttachment{}, err
	}

	query := url.Values{}
	if strings.TrimSpace(input.LeaseID) != "" {
		query.Set("lease_id", strings.TrimSpace(input.LeaseID))
	}
	var response taskAttachmentResponse
	if err := c.doMultipart(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/attachments", query, writer.FormDataContentType(), &body, &response); err != nil {
		return coordinator.TaskAttachment{}, err
	}

	return response.Attachment, nil
}

// ListTaskAttachments lists the attachments recorded for an task. It is the
// client counterpart to the task attachments read API and authenticates with
// the client's token (the worker uses its worker token).
func (c *Client) ListTaskAttachments(ctx context.Context, taskID string) ([]coordinator.TaskAttachment, error) {
	var response taskAttachmentsResponse
	if err := c.doContext(ctx, http.MethodGet, c.tasksPath("/"+url.PathEscape(taskID))+"/attachments", nil, nil, &response); err != nil {
		return nil, err
	}
	return response.Attachments, nil
}

// DownloadTaskAttachment downloads an task attachment's bytes into dst. It
// authenticates with the client's token (the worker uses its worker token).
func (c *Client) DownloadTaskAttachment(ctx context.Context, taskID, attachmentID string, dst io.Writer) error {
	endpoint := c.baseURL + c.tasksPath("/"+url.PathEscape(taskID)+"/attachments/"+url.PathEscape(attachmentID))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
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
	if err := c.do(http.MethodPost, "/v2/changes/"+url.PathEscape(id)+"/merge", map[string]string{}, nil, &response); err != nil {
		return coordinator.MergeResult{}, err
	}

	return response.Merge, nil
}

func (c *Client) CreateWebBootstrap() (WebBootstrapResult, error) {
	var response webBootstrapResponse
	if err := c.do(http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, nil, &response); err != nil {
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
		if err := c.do(http.MethodGet, "/v2/projects/"+url.PathEscape(c.projectID)+"/board", nil, nil, &response); err != nil {
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
	if err := c.do(http.MethodGet, "/v2/board", nil, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/projects", input, nil, &response); err != nil {
		return Project{}, false, err
	}

	return response.Project, response.Created, nil
}

func (c *Client) ListProjects() ([]Project, error) {
	var response projectsResponse
	if err := c.do(http.MethodGet, "/v2/projects", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Projects, nil
}

func (c *Client) ListHarnesses() (HarnessesResponse, error) {
	var response harnessesResponse
	if err := c.do(http.MethodGet, "/v2/harnesses", nil, nil, &response); err != nil {
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
	if err := c.do(http.MethodGet, "/v2/projects", nil, query, &response); err != nil {
		return nil, err
	}
	if len(response.Projects) == 0 {
		return nil, nil
	}

	return &response.Projects[0], nil
}

func (c *Client) ListChecks(taskID string) (CheckListResult, error) {
	var response checksResponse
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(taskID))+"/checks", nil, nil, &response); err != nil {
		return CheckListResult{}, err
	}

	return CheckListResult{
		Checks:      response.Checks,
		ReviewState: response.ReviewState,
	}, nil
}

func (c *Client) ListTransitions(taskID string) ([]coordinator.WorkflowTransition, error) {
	var response transitionsResponse
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(taskID))+"/transitions", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Transitions, nil
}

func (c *Client) RunReview(taskID string) (ReviewRunResult, error) {
	var response reviewRunResponse
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/review/run", map[string]string{}, nil, &response); err != nil {
		return ReviewRunResult{}, err
	}

	return ReviewRunResult{
		Change:      response.Change,
		Scheduled:   response.Scheduled,
		Checks:      response.Checks,
		ReviewState: response.ReviewState,
	}, nil
}

func (c *Client) GetCheck(taskID string, name string) (CheckResult, error) {
	var response checkResponse
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(taskID))+"/checks/"+url.PathEscape(name), nil, nil, &response); err != nil {
		return CheckResult{}, err
	}

	return CheckResult{
		Check:            response.Check,
		ReviewState:      response.ReviewState,
		FollowUpFailures: response.FollowUpFailures,
		Workflow:         response.Workflow,
	}, nil
}

func (c *Client) ReportCheck(taskID string, name string, input ReportCheckInput) (CheckResult, error) {
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
	if err := c.do(http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/checks/"+url.PathEscape(name), request, nil, &response); err != nil {
		return CheckResult{}, err
	}

	return CheckResult{
		Check:            response.Check,
		ReviewState:      response.ReviewState,
		FollowUpFailures: response.FollowUpFailures,
		Workflow:         response.Workflow,
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
	if err := c.do(http.MethodPost, "/v2/changes/"+url.PathEscape(changeID)+"/comments", request, nil, &response); err != nil {
		return coordinator.ReviewThread{}, err
	}

	return response.Thread, nil
}

func (c *Client) ListThreads(changeID string, leaseID string) ([]coordinator.ReviewThread, error) {
	var response threadsResponse
	path := "/v2/changes/" + url.PathEscape(changeID) + "/threads"
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
	if err := c.do(http.MethodPost, "/v2/threads/"+url.PathEscape(threadID)+"/comments", threadCommentRequest{Body: body, LeaseID: leaseID}, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/threads/"+url.PathEscape(threadID)+"/claims", request, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/threads/"+url.PathEscape(threadID)+"/"+action, threadCommentRequest{Body: body, LeaseID: leaseID}, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/workers/register", request, nil, &response); err != nil {
		return flowworker.Worker{}, err
	}

	return response.Worker, nil
}

func (c *Client) JoinWorker(input JoinWorkerInput) (JoinWorkerResult, error) {
	var response joinWorkerResponse
	request := joinWorkerRequest{WorkerID: input.WorkerID}
	if err := c.do(http.MethodPost, "/v2/workers/join", request, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/workers/heartbeat", request, nil, &response); err != nil {
		return flowworker.Worker{}, err
	}

	return response.Worker, nil
}

func (c *Client) ListWorkerReapJobs() ([]flowworker.Job, error) {
	var response jobsResponse
	if err := c.do(http.MethodGet, "/v2/workers/reap-jobs", nil, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/workers/claim", request, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/workers/renew", request, nil, &response); err != nil {
		return flowworker.Lease{}, err
	}

	return response.Lease, nil
}

func (c *Client) WorkerJobStatus(ctx context.Context, input WorkerJobStatusInput) (WorkerJobStatusResult, error) {
	var response workerJobStatusResponse
	request := workerJobStatusRequest{LeaseID: input.LeaseID}
	if err := c.doContext(ctx, http.MethodPost, "/v2/workers/status", request, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/workers/running", request, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/workers/release", request, nil, &response); err != nil {
		return flowworker.Job{}, err
	}

	return response.Job, nil
}

func (c *Client) ReleaseConsole(ctx context.Context) error {
	var response consoleResponse
	return c.doContext(ctx, http.MethodDelete, c.consolePath(), nil, nil, &response)
}

func (c *Client) RegisterJobTerminal(ctx context.Context, jobID string, leaseID string, tmuxSocketPath string) (coordinator.JobTerminal, error) {
	var response jobTerminalResponse
	request := jobTerminalRequest{
		LeaseID:        leaseID,
		TmuxSocketPath: tmuxSocketPath,
	}
	if err := c.doContext(ctx, http.MethodPost, "/v2/jobs/"+url.PathEscape(jobID)+"/terminal", request, nil, &response); err != nil {
		return coordinator.JobTerminal{}, err
	}

	return response.Terminal, nil
}

func (c *Client) ListWorkers() ([]flowworker.Worker, error) {
	var response workersResponse
	if err := c.do(http.MethodGet, "/v2/workers", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Workers, nil
}

func (c *Client) ListJobs() ([]flowworker.Job, error) {
	var response jobsResponse
	if err := c.do(http.MethodGet, "/v2/jobs", nil, nil, &response); err != nil {
		return nil, err
	}

	return response.Jobs, nil
}

// QueueStats is the aggregate job queue depth across every project database,
// as served by GET /v2/queue/stats (owner or orchestrator scope).
type QueueStats struct {
	Queued           int            `json:"queued"`
	ClaimedOrRunning int            `json:"claimed_or_running"`
	ByBucket         map[string]int `json:"by_bucket"`
}

// GetQueueStats fetches the queue depth the flow-orchestrator scales on.
func (c *Client) GetQueueStats() (QueueStats, error) {
	var response QueueStats
	if err := c.do(http.MethodGet, "/v2/queue/stats", nil, nil, &response); err != nil {
		return QueueStats{}, err
	}

	return response, nil
}

func (c *Client) EnqueueJob(input EnqueueJobInput) (flowworker.Job, error) {
	var response jobResponse
	request := enqueueJobRequest{
		TaskID:         input.TaskID,
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
	if err := c.do(http.MethodPost, "/v2/jobs", request, nil, &response); err != nil {
		return flowworker.Job{}, err
	}

	return response.Job, nil
}

func (c *Client) JobAttach(jobID string) (terminal.AttachInfo, error) {
	var response attachResponse
	if err := c.do(http.MethodGet, "/v2/jobs/"+url.PathEscape(jobID)+"/attach", nil, nil, &response); err != nil {
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
	if err := c.doContext(ctx, http.MethodPost, "/v2/sessions/"+url.PathEscape(sessionID)+"/event", request, nil, &response); err != nil {
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
	if err := c.doContext(ctx, http.MethodPost, "/v2/sessions/"+url.PathEscape(sessionID)+"/signal", request, nil, &response); err != nil {
		return coordinator.Session{}, err
	}

	return response.Session, nil
}

func (c *Client) ReadySession(sessionID string) (coordinator.Session, error) {
	return c.ReadySessionWithInput(sessionID, ReadySessionInput{})
}

func (c *Client) ReadySessionWithInput(sessionID string, input ReadySessionInput) (coordinator.Session, error) {
	var response sessionResponse
	if err := c.do(http.MethodPost, "/v2/sessions/"+url.PathEscape(sessionID)+"/ready", readySessionRequest{HeadSHA: input.HeadSHA}, nil, &response); err != nil {
		return coordinator.Session{}, err
	}

	return response.Session, nil
}

func (c *Client) SessionAttach(sessionID string) (terminal.AttachInfo, error) {
	var response attachResponse
	if err := c.do(http.MethodGet, "/v2/sessions/"+url.PathEscape(sessionID)+"/attach", nil, nil, &response); err != nil {
		return terminal.AttachInfo{}, err
	}

	return response.Attach, nil
}

func (c *Client) RegisterSessionTerminal(ctx context.Context, sessionID string, tmuxSocketPath string) (coordinator.SessionTerminal, error) {
	var response sessionTerminalResponse
	request := sessionTerminalRequest{TmuxSocketPath: tmuxSocketPath}
	if err := c.doContext(ctx, http.MethodPost, "/v2/sessions/"+url.PathEscape(sessionID)+"/terminal", request, nil, &response); err != nil {
		return coordinator.SessionTerminal{}, err
	}

	return response.Terminal, nil
}

func (c *Client) CreateSessionTerminalAccess(sessionID string) (coordinator.SessionTerminalAccess, error) {
	var response sessionTerminalAccessResponse
	if err := c.do(http.MethodPost, "/v2/sessions/"+url.PathEscape(sessionID)+"/terminal-token", map[string]string{}, nil, &response); err != nil {
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
	if err := c.do(http.MethodPost, "/v2/sessions/"+url.PathEscape(sessionID)+"/status", request, nil, &response); err != nil {
		return coordinator.StatusLogEntry{}, err
	}

	return response.Status, nil
}

func (c *Client) ReportSessionProcessExit(ctx context.Context, input ReportSessionProcessExitInput) (coordinator.Session, error) {
	var response sessionResponse
	request := sessionProcessExitRequest{LeaseID: input.LeaseID, ExitCode: input.ExitCode}
	if err := c.doContext(ctx, http.MethodPost, "/v2/sessions/"+url.PathEscape(input.SessionID)+"/process-exit", request, nil, &response); err != nil {
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
	path := "/v2/sessions/" + url.PathEscape(input.SessionID) + "/messages"
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
	path := "/v2/sessions/" + url.PathEscape(input.SessionID) + "/messages/" + url.PathEscape(input.MessageID) + "/delivered"
	if err := c.doContext(ctx, http.MethodPost, path, request, nil, &response); err != nil {
		return coordinator.SessionMessage{}, err
	}

	return response.Message, nil
}

func (c *Client) ReplyToTask(taskID string, input ReplyToTaskInput) (coordinator.SessionMessage, bool, error) {
	var response sessionMessageResponse
	request := attentionReplyRequest{Message: input.Message, StatusLogID: input.StatusLogID}
	if err := c.do(http.MethodPost, "/v2/tasks/"+url.PathEscape(taskID)+"/attention/reply", request, nil, &response); err != nil {
		return coordinator.SessionMessage{}, false, err
	}

	return response.Message, response.Queued, nil
}

func (c *Client) Reconcile() (coordinator.ReconcileResult, error) {
	var response reconcileResponse
	if err := c.do(http.MethodPost, "/v2/reconcile", map[string]string{}, nil, &response); err != nil {
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
	if err := c.do(http.MethodPut, "/v2/changes/"+url.PathEscape(changeID)+"/handoff", request, nil, &response); err != nil {
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
	err := c.do(http.MethodGet, "/v2/changes/"+url.PathEscape(changeID)+"/handoff", nil, query, &response)
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
	return c.doRaw(ctx, http.MethodPut, "/v2/sessions/"+url.PathEscape(sessionID)+"/transcript", nil, r)
}

// UploadJobTranscript PUTs raw transcript bytes for a check job, proving a live
// lease via the lease_id query parameter (mirroring the worker check-report
// scope).
func (c *Client) UploadJobTranscript(ctx context.Context, jobID string, leaseID string, r io.Reader) error {
	query := url.Values{}
	query.Set("lease_id", leaseID)
	return c.doRaw(ctx, http.MethodPut, "/v2/jobs/"+url.PathEscape(jobID)+"/transcript", query, r)
}

// SessionTranscript GETs an author session's terminal-rendered plain-text
// transcript (owner scope).
func (c *Client) SessionTranscript(sessionID string) (string, error) {
	return c.getText("/v2/sessions/" + url.PathEscape(sessionID) + "/transcript")
}

// JobTranscript GETs a job's terminal-rendered plain-text transcript (owner
// scope).
func (c *Client) JobTranscript(jobID string) (string, error) {
	return c.getText("/v2/jobs/" + url.PathEscape(jobID) + "/transcript")
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
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
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
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
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
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
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
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
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

type CreateTaskInput struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Priority int    `json:"priority"`
	FlowID   string `json:"flow_id,omitempty"`
}

type EditTaskInput struct {
	Title    *string `json:"title,omitempty"`
	Body     *string `json:"body,omitempty"`
	Priority *int    `json:"priority,omitempty"`
	FlowID   *string `json:"flow_id,omitempty"`
}

type UploadTaskAttachmentInput struct {
	Stage       coordinator.TaskAttachmentStage
	Filename    string
	ContentType string
	Reader      io.Reader
	LeaseID     string
}

type TaskFilter struct {
	LifecycleStates []string
	TagSlugs        []string
}

type ReviewFollowUpFinding = contract.ReviewFollowUpFinding

type ReviewFollowUpTaskAction = contract.ReviewFollowUpTaskAction

type ApplyReviewFollowUpInput = contract.ApplyReviewFollowUpRequest

type ApplyReviewFollowUpResult struct {
	Task        coordinator.Task
	Disposition string
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
	TaskID         *string
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
	Workflow         *coordinator.WorkflowRun
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

type ReplyToTaskInput struct {
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

type scheduleTaskRequest = contract.ScheduleTaskRequest
type taskStateRequest = contract.TaskStateRequest
type triageTaskRequest = contract.TriageTaskRequest
type taskRelationRequest = contract.TaskRelationRequest
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
type attentionReplyRequest = contract.AttentionReplyRequest
type sessionTerminalRequest = contract.SessionTerminalRequest
type jobTerminalRequest = contract.JobTerminalRequest
type createThreadRequest = contract.CreateThreadRequest
type putHandoffRequest = contract.PutHandoffRequest
type threadCommentRequest = contract.ThreadCommentRequest
type threadClaimRequest = contract.ThreadClaimRequest
type taskResponse = contract.TaskResponse
type tasksResponse = contract.TasksResponse
type taskAttachmentResponse = contract.TaskAttachmentResponse
type taskAttachmentsResponse = contract.TaskAttachmentsResponse
type taskRelationsResponse = contract.TaskRelationsResponse
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
