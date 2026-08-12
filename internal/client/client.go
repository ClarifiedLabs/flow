package client

import (
	"bytes"
	"bufio"
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
	"github.com/ClarifiedLabs/flow/internal/checkverdict"
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

// CreateTask creates a task and reports whether the server replayed a stored
// idempotent response (reused=true) instead of executing a fresh create.
func (c *Client) CreateTask(input CreateTaskInput) (coordinator.Task, bool, error) {
	var response taskResponse
	headers := http.Header{}
	if key := strings.TrimSpace(input.IdempotencyKey); key != "" {
		headers.Set(contract.IdempotencyHeader, key)
	}
	responseHeaders, err := c.doRequest(context.Background(), http.MethodPost, c.tasksPath(""), input, nil, headers, &response)
	if err != nil {
		return coordinator.Task{}, false, err
	}
	reused := strings.TrimSpace(responseHeaders.Get(contract.IdempotentReplayHeader)) != ""
	return response.Task, reused, nil
}

func (c *Client) ListTasks(filter TaskFilter) ([]coordinator.Task, error) {
	query := url.Values{}
	for _, state := range filter.LifecycleStates {
		query.Add("state", state)
	}
	for _, tag := range filter.TagSlugs {
		query.Add("tag", tag)
	}
	if filter.Ready {
		query.Set("ready", "1")
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

// TaskView is a task together with the server-derived flags the CLI polls on.
type TaskView struct {
	Task    coordinator.Task
	Blocked bool
}

// GetTaskView reads a task with its derived blocked flag.
func (c *Client) GetTaskView(id string) (TaskView, error) {
	var response taskResponse
	if err := c.do(http.MethodGet, c.tasksPath("/"+url.PathEscape(id)), nil, nil, &response); err != nil {
		return TaskView{}, err
	}

	return TaskView{Task: response.Task, Blocked: response.Blocked}, nil
}

// CreateFeature posts a new feature: a project-child task group with its own
// long-lived branch in the exchange.
func (c *Client) CreateFeature(input CreateFeatureInput) (contract.FeatureResponse, error) {
	var response featureResponse
	if err := c.do(http.MethodPost, c.projectPath("/features"), input, nil, &response); err != nil {
		return contract.FeatureResponse{}, err
	}

	return response, nil
}

// ListFeatures returns the project's features. The status filter selects the
// lifecycle bucket (open, landed, archived); an empty filter lists open
// features and "all" lists everything.
func (c *Client) ListFeatures(status string) ([]contract.FeatureResponse, error) {
	query := url.Values{}
	if status != "" {
		query.Set("status", status)
	}

	var response featuresResponse
	if err := c.do(http.MethodGet, c.projectPath("/features"), nil, query, &response); err != nil {
		return nil, err
	}

	return response.Features, nil
}

func (c *Client) GetFeature(id string) (contract.FeatureResponse, error) {
	var response featureResponse
	if err := c.do(http.MethodGet, c.projectPath("/features/"+url.PathEscape(id)), nil, nil, &response); err != nil {
		return contract.FeatureResponse{}, err
	}

	return response, nil
}

// UpdateFeature edits feature metadata; nil fields are left unchanged.
func (c *Client) UpdateFeature(id string, input UpdateFeatureInput) (contract.FeatureResponse, error) {
	var response featureResponse
	if err := c.do(http.MethodPatch, c.projectPath("/features/"+url.PathEscape(id)), input, nil, &response); err != nil {
		return contract.FeatureResponse{}, err
	}

	return response, nil
}

// RebaseFeature rebases the feature branch onto the project's base branch.
// The response reports whether the branch was rebased instantly, was already
// up to date, or a rebase task was created to resolve conflicts.
func (c *Client) RebaseFeature(id string) (contract.RebaseFeatureResponse, error) {
	var response rebaseFeatureResponse
	if err := c.do(http.MethodPost, c.projectPath("/features/"+url.PathEscape(id)+"/rebase"), map[string]any{}, nil, &response); err != nil {
		return contract.RebaseFeatureResponse{}, err
	}

	return response, nil
}

// LandFeature squash-merges the feature branch into the project's base
// branch and marks the feature landed.
func (c *Client) LandFeature(id string) (contract.FeatureResponse, error) {
	var response featureResponse
	if err := c.do(http.MethodPost, c.projectPath("/features/"+url.PathEscape(id)+"/land"), map[string]any{}, nil, &response); err != nil {
		return contract.FeatureResponse{}, err
	}

	return response, nil
}

// ArchiveFeature archives the feature; the branch is retained for audit.
func (c *Client) ArchiveFeature(id string) (contract.FeatureResponse, error) {
	var response featureResponse
	if err := c.do(http.MethodPost, c.projectPath("/features/"+url.PathEscape(id)+"/archive"), map[string]any{}, nil, &response); err != nil {
		return contract.FeatureResponse{}, err
	}

	return response, nil
}

func (c *Client) StartFeature(id string) (coordinator.ContainerStartResult, error) {
	var response coordinator.ContainerStartResult
	if err := c.do(http.MethodPost, c.projectPath("/features/"+url.PathEscape(id)+"/start"), map[string]any{}, nil, &response); err != nil {
		return coordinator.ContainerStartResult{}, err
	}
	return response, nil
}

func (c *Client) CreateEpic(input contract.CreateEpicRequest) (contract.EpicResponse, error) {
	var response contract.EpicResponse
	if err := c.do(http.MethodPost, c.projectPath("/epics"), input, nil, &response); err != nil {
		return contract.EpicResponse{}, err
	}
	return response, nil
}

func (c *Client) ListEpics(status string) ([]contract.EpicResponse, error) {
	query := url.Values{}
	if strings.TrimSpace(status) != "" {
		query.Set("status", status)
	}
	var response contract.EpicsResponse
	if err := c.do(http.MethodGet, c.projectPath("/epics"), nil, query, &response); err != nil {
		return nil, err
	}
	return response.Epics, nil
}

func (c *Client) GetEpic(id string) (contract.EpicResponse, error) {
	var response contract.EpicResponse
	if err := c.do(http.MethodGet, c.projectPath("/epics/"+url.PathEscape(id)), nil, nil, &response); err != nil {
		return contract.EpicResponse{}, err
	}
	return response, nil
}

func (c *Client) UpdateEpic(id string, input contract.EditEpicRequest) (contract.EpicResponse, error) {
	var response contract.EpicResponse
	if err := c.do(http.MethodPatch, c.projectPath("/epics/"+url.PathEscape(id)), input, nil, &response); err != nil {
		return contract.EpicResponse{}, err
	}
	return response, nil
}

func (c *Client) epicAction(id, action string) (contract.EpicResponse, error) {
	var response contract.EpicResponse
	if err := c.do(http.MethodPost, c.projectPath("/epics/"+url.PathEscape(id)+"/"+action), map[string]any{}, nil, &response); err != nil {
		return contract.EpicResponse{}, err
	}
	return response, nil
}

func (c *Client) StartEpic(id string) (coordinator.ContainerStartResult, error) {
	var response coordinator.ContainerStartResult
	if err := c.do(http.MethodPost, c.projectPath("/epics/"+url.PathEscape(id)+"/start"), map[string]any{}, nil, &response); err != nil {
		return coordinator.ContainerStartResult{}, err
	}
	return response, nil
}

func (c *Client) CompleteEpic(id string) (contract.EpicResponse, error) {
	return c.epicAction(id, "complete")
}

func (c *Client) ReopenEpic(id string) (contract.EpicResponse, error) {
	return c.epicAction(id, "reopen")
}

func (c *Client) ArchiveEpic(id string) (contract.EpicResponse, error) {
	return c.epicAction(id, "archive")
}

func (c *Client) ListWorkItems(query url.Values) ([]coordinator.WorkItemSummary, error) {
	var response contract.WorkItemsResponse
	if err := c.do(http.MethodGet, c.projectPath("/work-items"), nil, query, &response); err != nil {
		return nil, err
	}
	return response.Items, nil
}

func (c *Client) DoctorWorkItems() (coordinator.WorkItemConsistencyReport, error) {
	var report coordinator.WorkItemConsistencyReport
	err := c.do(http.MethodGet, c.projectPath("/work-items/doctor"), nil, nil, &report)
	return report, err
}

func (c *Client) GetWorkItem(id string, tree bool) (contract.WorkItemResponse, error) {
	path := c.projectPath("/work-items/" + url.PathEscape(id))
	if tree {
		path += "/tree"
	}
	var response contract.WorkItemResponse
	if err := c.do(http.MethodGet, path, nil, nil, &response); err != nil {
		return contract.WorkItemResponse{}, err
	}
	return response, nil
}

func (c *Client) GetWorkItemRelations(id string) ([]coordinator.WorkItemRelation, error) {
	var response struct {
		Relations []coordinator.WorkItemRelation `json:"relations"`
	}
	if err := c.do(http.MethodGet, c.projectPath("/work-items/"+url.PathEscape(id)+"/relations"), nil, nil, &response); err != nil {
		return nil, err
	}
	return response.Relations, nil
}

func (c *Client) LinkWorkItems(sourceID string, kind coordinator.RelationKind, targetID string) error {
	request := contract.WorkItemRelationRequest{SourceItemID: sourceID, TargetItemID: targetID, Kind: string(kind)}
	return c.do(http.MethodPost, c.projectPath("/work-items/"+url.PathEscape(sourceID)+"/relations"), request, nil, nil)
}

func (c *Client) UnlinkWorkItems(sourceID string, kind coordinator.RelationKind, targetID string) error {
	request := contract.WorkItemRelationRequest{SourceItemID: sourceID, TargetItemID: targetID, Kind: string(kind)}
	return c.do(http.MethodDelete, c.projectPath("/work-items/"+url.PathEscape(sourceID)+"/relations"), request, nil, nil)
}

func (c *Client) MoveWorkItem(id, parentID string) (contract.WorkItemResponse, error) {
	var response contract.WorkItemResponse
	request := contract.MoveWorkItemRequest{ParentItemID: parentID}
	if err := c.do(http.MethodPatch, c.projectPath("/work-items/"+url.PathEscape(id)+"/parent"), request, nil, &response); err != nil {
		return contract.WorkItemResponse{}, err
	}
	return response, nil
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
	OwnerRulings     []coordinator.OwnerRuling            `json:"owner_rulings,omitempty"`
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

func (c *Client) RecordOwnerRuling(taskID, body, supersedesID, idempotencyKey string) (coordinator.RecordOwnerRulingResult, error) {
	var response coordinator.RecordOwnerRulingResult
	request := struct {
		Body         string `json:"body"`
		SupersedesID string `json:"supersedes_id,omitempty"`
	}{Body: body, SupersedesID: supersedesID}
	headers := http.Header{contract.IdempotencyHeader: []string{strings.TrimSpace(idempotencyKey)}}
	if err := c.doContextWithHeaders(context.Background(), http.MethodPost,
		c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/rulings", request, nil, headers, &response); err != nil {
		return coordinator.RecordOwnerRulingResult{}, err
	}
	return response, nil
}

func (c *Client) RequestReviewScopeDecision(ctx context.Context, taskID, checkName, leaseID, sourceJobID string, report checkverdict.VerdictReport) (coordinator.RequestReviewScopeDecisionResult, error) {
	var response coordinator.RequestReviewScopeDecisionResult
	request := struct {
		LeaseID     string                     `json:"lease_id"`
		SourceJobID string                     `json:"source_job_id"`
		CheckName   string                     `json:"check_name"`
		Report      checkverdict.VerdictReport `json:"report"`
	}{LeaseID: leaseID, SourceJobID: sourceJobID, CheckName: checkName, Report: report}
	if err := c.doContext(ctx, http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/workflow/review-scope-decisions", request, nil, &response); err != nil {
		return coordinator.RequestReviewScopeDecisionResult{}, err
	}
	return response, nil
}

func (c *Client) ResolveReviewScopeDecision(taskID, waitID string, choice coordinator.ReviewScopeDecisionChoice, guidance, idempotencyKey string) (coordinator.ResolveReviewScopeDecisionResult, error) {
	var response coordinator.ResolveReviewScopeDecisionResult
	request := struct {
		Choice   coordinator.ReviewScopeDecisionChoice `json:"choice"`
		Guidance string                                `json:"guidance,omitempty"`
	}{Choice: choice, Guidance: guidance}
	headers := http.Header{contract.IdempotencyHeader: []string{strings.TrimSpace(idempotencyKey)}}
	path := c.tasksPath("/"+url.PathEscape(taskID)) + "/workflow/review-scope-decisions/" + url.PathEscape(waitID) + "/resolve"
	if err := c.doContextWithHeaders(context.Background(), http.MethodPost, path, request, nil, headers, &response); err != nil {
		return coordinator.ResolveReviewScopeDecisionResult{}, err
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

// RespondWorkflow answers the exact human-gate wait observed by the caller.
// reviewWaitID is mandatory; nodeRunID is retained as an additional assertion.
func (c *Client) RespondWorkflow(taskID, nodeRunID, reviewWaitID, outcome, feedback string) (coordinator.CompleteWorkflowNodeResult, error) {
	reviewWaitID = strings.TrimSpace(reviewWaitID)
	if reviewWaitID == "" {
		return coordinator.CompleteWorkflowNodeResult{}, errors.New("review wait id is required")
	}
	var response coordinator.CompleteWorkflowNodeResult
	request := map[string]string{"node_run_id": nodeRunID, "review_wait_id": reviewWaitID, "outcome": outcome, "feedback": feedback}
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

// ListEvents returns one page of the project event log with seq > since,
// plus the cursor to pass for the following page.
func (c *Client) ListEvents(since int64, limit int) ([]coordinator.Event, int64, error) {
	var response struct {
		Events    []coordinator.Event `json:"events"`
		NextSince int64               `json:"next_since"`
	}
	query := url.Values{}
	query.Set("since", strconv.FormatInt(since, 10))
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if err := c.do(http.MethodGet, c.projectPath("/events"), nil, query, &response); err != nil {
		return nil, 0, err
	}
	return response.Events, response.NextSince, nil
}

// StreamEvents tails the project event log over server-sent events, invoking
// onEvent for each event with seq > since. It returns when ctx is canceled,
// when onEvent fails, or when the server closes the stream.
func (c *Client) StreamEvents(ctx context.Context, since int64, onEvent func(coordinator.Event) error) error {
	endpoint := c.baseURL + c.projectPath("/events/stream") + "?since=" + strconv.FormatInt(since, 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set("Accept", "text/event-stream")
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
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event coordinator.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			return fmt.Errorf("decode event stream frame: %w", err)
		}
		if err := onEvent(event); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
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
	return c.ReportCheckContext(context.Background(), taskID, name, input)
}

func (c *Client) ReportCheckContext(ctx context.Context, taskID string, name string, input ReportCheckInput) (CheckResult, error) {
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
	if err := c.doContext(ctx, http.MethodPost, c.tasksPath("/"+url.PathEscape(taskID))+"/checks/"+url.PathEscape(name), request, nil, &response); err != nil {
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

func (c *Client) ReserveProvisionerAssignment(ctx context.Context, input ReserveProvisionerAssignmentInput) (ReserveProvisionerAssignmentResult, error) {
	request := contract.ReserveProvisionerAssignmentRequest{
		ProviderID: input.ProviderID, ProviderRequestID: input.ProviderRequestID,
		ProfileName: input.ProfileName, ProviderType: input.ProviderType, ProviderOptions: input.ProviderOptions, MaxConcurrency: input.MaxConcurrency,
		AllowedRoles: input.AllowedRoles, AllowedBuckets: input.AllowedBuckets,
		Labels: input.Labels, Taints: input.Taints, HarnessModels: input.HarnessModels,
		RequiredSelector:      input.RequiredSelector,
		StartupTimeoutSeconds: durationSeconds(input.StartupTimeout),
		WaitSeconds:           durationSeconds(input.Wait),
	}
	var response contract.ReserveProvisionerAssignmentResponse
	if err := c.doContext(ctx, http.MethodPost, "/v2/provisioner/assignments/reserve", request, nil, &response); err != nil {
		return ReserveProvisionerAssignmentResult{}, err
	}
	return ReserveProvisionerAssignmentResult(response), nil
}

func (c *Client) ListProvisionerAssignments(ctx context.Context, filter ProvisionerAssignmentFilter) ([]ProvisionerAssignment, error) {
	query := url.Values{}
	query.Set("project_id", strings.TrimSpace(filter.ProjectID))
	query.Set("provider_id", strings.TrimSpace(filter.ProviderID))
	query.Set("profile_name", strings.TrimSpace(filter.ProfileName))
	query.Set("provider_request_id", strings.TrimSpace(filter.ProviderRequestID))
	query.Set("worker_id", strings.TrimSpace(filter.WorkerID))
	query.Set("job_id", strings.TrimSpace(filter.JobID))
	for _, state := range filter.States {
		query.Add("state", string(state))
	}
	if filter.OpenOnly {
		query.Set("open_only", "true")
	}
	if filter.NeedsCleanup {
		query.Set("needs_cleanup", "true")
	}
	var response contract.ProvisionerAssignmentsResponse
	if err := c.doContext(ctx, http.MethodGet, "/v2/provisioner/assignments", nil, query, &response); err != nil {
		return nil, err
	}
	return response.Assignments, nil
}

func (c *Client) RecordProvisionerAssignmentAttempt(ctx context.Context, assignmentID string, input RecordProvisionerAssignmentAttemptInput) (ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	request := contract.RecordProvisionerAssignmentAttemptRequest{ProviderError: input.ProviderError, NextRetryAt: input.NextRetryAt}
	path := "/v2/provisioner/assignments/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/attempt"
	if err := c.doContext(ctx, http.MethodPost, path, request, nil, &response); err != nil {
		return ProvisionerAssignment{}, err
	}
	return response.Assignment, nil
}

func (c *Client) AbandonProvisionerAssignment(ctx context.Context, assignmentID, providerError string) (ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	path := "/v2/provisioner/assignments/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/abandon"
	if err := c.doContext(ctx, http.MethodPost, path, contract.AbandonProvisionerAssignmentRequest{ProviderError: providerError}, nil, &response); err != nil {
		return ProvisionerAssignment{}, err
	}
	return response.Assignment, nil
}

func (c *Client) RevokeProvisionerAssignmentCredentials(ctx context.Context, assignmentID string) (ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	path := "/v2/provisioner/assignments/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/revoked"
	if err := c.doContext(ctx, http.MethodPost, path, nil, nil, &response); err != nil {
		return ProvisionerAssignment{}, err
	}
	return response.Assignment, nil
}

func (c *Client) MarkProvisionerAssignmentCleaned(ctx context.Context, assignmentID string) (ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	path := "/v2/provisioner/assignments/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/cleaned"
	if err := c.doContext(ctx, http.MethodPost, path, nil, nil, &response); err != nil {
		return ProvisionerAssignment{}, err
	}
	return response.Assignment, nil
}

func (c *Client) RegisterWorker(input RegisterWorkerInput) (flowworker.Worker, error) {
	return c.RegisterWorkerContext(context.Background(), input)
}

func (c *Client) RegisterWorkerContext(ctx context.Context, input RegisterWorkerInput) (flowworker.Worker, error) {
	var response workerResponse
	request := registerWorkerRequest{
		ID:                  input.ID,
		Labels:              input.Labels,
		Taints:              input.Taints,
		HarnessModels:       input.HarnessModels,
		HeartbeatTTLSeconds: durationSeconds(input.HeartbeatTTL),
	}
	if err := c.doContext(ctx, http.MethodPost, "/v2/workers/register", request, nil, &response); err != nil {
		return flowworker.Worker{}, err
	}

	return response.Worker, nil
}

func (c *Client) HeartbeatWorker(input HeartbeatWorkerInput) (flowworker.Worker, error) {
	return c.HeartbeatWorkerContext(context.Background(), input)
}

func (c *Client) HeartbeatWorkerContext(ctx context.Context, input HeartbeatWorkerInput) (flowworker.Worker, error) {
	var response workerResponse
	request := heartbeatWorkerRequest{
		WorkerID:            input.WorkerID,
		HeartbeatTTLSeconds: durationSeconds(input.HeartbeatTTL),
	}
	if err := c.doContext(ctx, http.MethodPost, "/v2/workers/heartbeat", request, nil, &response); err != nil {
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
	return c.ClaimJobContext(context.Background(), input)
}

func (c *Client) ClaimJobContext(ctx context.Context, input ClaimJobInput) (ClaimJobResult, error) {
	var response claimJobResponse
	request := claimJobRequest{
		WorkerID:             input.WorkerID,
		LeaseDurationSeconds: durationSeconds(input.LeaseDuration),
		WaitSeconds:          durationSeconds(input.Wait),
		CapabilitiesReported: input.CapabilitiesReported,
		Labels:               input.Labels,
		Taints:               input.Taints,
		HarnessModels:        input.HarnessModels,
		HeartbeatTTLSeconds:  durationSeconds(input.HeartbeatTTL),
	}
	if err := c.doContext(ctx, http.MethodPost, "/v2/workers/claim", request, nil, &response); err != nil {
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
	return c.ReleaseLeaseContext(context.Background(), input)
}

func (c *Client) ReleaseLeaseContext(ctx context.Context, input ReleaseLeaseInput) (flowworker.Job, error) {
	var response jobResponse
	request := releaseLeaseRequest{
		LeaseID:    input.LeaseID,
		FinalState: string(input.FinalState),
	}
	if err := c.doContext(ctx, http.MethodPost, "/v2/workers/release", request, nil, &response); err != nil {
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
		Harness:        input.Harness,
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

// ProjectRef returns the configured project id or name used for project-scoped routes.
func (c *Client) ProjectRef() string { return c.projectID }

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

type HistoryCaptureFilter struct {
	TaskIDs    []string
	JobIDs     []string
	SessionIDs []string
	CaptureIDs []string
	States     []string
	Since      *time.Time
	Until      *time.Time
	Resumable  *bool
	Limit      int
	Cursor     string
}

func (c *Client) ListHistoryCaptures(ctx context.Context, filter HistoryCaptureFilter) (contract.HistoryCapturesResponse, error) {
	query := url.Values{}
	for _, value := range filter.TaskIDs {
		query.Add("task_id", value)
	}
	for _, value := range filter.JobIDs {
		query.Add("job_id", value)
	}
	for _, value := range filter.SessionIDs {
		query.Add("session_id", value)
	}
	for _, value := range filter.CaptureIDs {
		query.Add("capture_id", value)
	}
	for _, value := range filter.States {
		query.Add("state", value)
	}
	if filter.Since != nil {
		query.Set("since", filter.Since.UTC().Format(time.RFC3339Nano))
	}
	if filter.Until != nil {
		query.Set("until", filter.Until.UTC().Format(time.RFC3339Nano))
	}
	if filter.Resumable != nil {
		query.Set("resumable", strconv.FormatBool(*filter.Resumable))
	}
	if filter.Limit != 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Cursor != "" {
		query.Set("cursor", filter.Cursor)
	}
	var response contract.HistoryCapturesResponse
	if err := c.doContext(ctx, http.MethodGet, c.projectPath("/history/captures"), nil, query, &response); err != nil {
		return contract.HistoryCapturesResponse{}, err
	}
	return response, nil
}

func (c *Client) GetHistoryCapture(ctx context.Context, captureID string) (contract.HistoryCaptureResponse, error) {
	var response contract.HistoryCaptureResponse
	path := c.projectPath("/history/captures/" + url.PathEscape(captureID))
	if err := c.doContext(ctx, http.MethodGet, path, nil, nil, &response); err != nil {
		return contract.HistoryCaptureResponse{}, err
	}
	return response, nil
}

func (c *Client) ListHistoryCaptureEvents(ctx context.Context, captureID string) (contract.HistoryCaptureEventsResponse, error) {
	var response contract.HistoryCaptureEventsResponse
	path := c.projectPath("/history/captures/" + url.PathEscape(captureID) + "/events")
	if err := c.doContext(ctx, http.MethodGet, path, nil, nil, &response); err != nil {
		return contract.HistoryCaptureEventsResponse{}, err
	}
	return response, nil
}

func (c *Client) WaiveHistoryCapture(ctx context.Context, captureID string, input contract.WaiveHistoryCaptureRequest) (contract.HistoryCapture, error) {
	var response contract.HistoryCapture
	path := c.projectPath("/history/captures/" + url.PathEscape(captureID) + "/waive")
	if err := c.doContext(ctx, http.MethodPost, path, input, nil, &response); err != nil {
		return contract.HistoryCapture{}, err
	}
	return response, nil
}

func (c *Client) RevokeHistoryUploadGrant(ctx context.Context, captureID string, input contract.RevokeHistoryUploadGrantRequest) (contract.HistoryCapture, error) {
	var response contract.HistoryCapture
	path := c.projectPath("/history/captures/" + url.PathEscape(captureID) + "/upload-grant/revoke")
	if err := c.doContext(ctx, http.MethodPost, path, input, nil, &response); err != nil {
		return contract.HistoryCapture{}, err
	}
	return response, nil
}

func (c *Client) DownloadHistoryArtifact(ctx context.Context, captureID, artifactID string, dst io.Writer) error {
	path := c.projectPath("/history/captures/" + url.PathEscape(captureID) + "/artifacts/" + url.PathEscape(artifactID) + "/content")
	return c.downloadHistoryContent(ctx, path, dst)
}

func (c *Client) DownloadHistoryManifest(ctx context.Context, captureID string, dst io.Writer) error {
	path := c.projectPath("/history/captures/" + url.PathEscape(captureID) + "/manifest")
	return c.downloadHistoryContent(ctx, path, dst)
}

func (c *Client) ResumeHistoryCapture(ctx context.Context, captureID string, input contract.ResumeHistoryCaptureRequest) (contract.ResumeHistoryCaptureResponse, error) {
	var response contract.ResumeHistoryCaptureResponse
	path := c.projectPath("/history/captures/" + url.PathEscape(captureID) + "/resume")
	if err := c.doContext(ctx, http.MethodPost, path, input, nil, &response); err != nil {
		return contract.ResumeHistoryCaptureResponse{}, err
	}
	return response, nil
}

// DownloadHistoryResumeArtifact downloads one coordinator-selected source
// artifact while proving the caller still owns the active resume lease.
func (c *Client) DownloadHistoryResumeArtifact(ctx context.Context, captureID, artifactID, jobID, leaseID string, dst io.Writer) error {
	if dst == nil {
		return errors.New("history resume destination is required")
	}
	path := "/v2/history/captures/" + url.PathEscape(captureID) + "/artifacts/" + url.PathEscape(artifactID) + "/resume-content"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set("Flow-History-Resume-Job", strings.TrimSpace(jobID))
	request.Header.Set("Flow-History-Resume-Lease", strings.TrimSpace(leaseID))
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
	_, err = io.Copy(dst, response.Body)
	return err
}

func (c *Client) ReserveHistoryCapture(ctx context.Context, input contract.ReserveHistoryCaptureRequest) (contract.ReserveHistoryCaptureResponse, error) {
	var response contract.ReserveHistoryCaptureResponse
	if err := c.doContext(ctx, http.MethodPost, "/v2/history/captures", input, nil, &response); err != nil {
		return contract.ReserveHistoryCaptureResponse{}, err
	}
	return response, nil
}

func (c *Client) UploadHistoryArtifactBytes(ctx context.Context, captureID, grant string, source io.Reader) (contract.HistoryUploadResponse, error) {
	if source == nil {
		return contract.HistoryUploadResponse{}, errors.New("history upload source is required")
	}
	path := "/v2/history/captures/" + url.PathEscape(captureID) + "/uploads"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, source)
	if err != nil {
		return contract.HistoryUploadResponse{}, err
	}
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Flow-History-Upload-Grant", grant)
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return contract.HistoryUploadResponse{}, err
	}
	defer response.Body.Close()
	if err := statusError(response); err != nil {
		return contract.HistoryUploadResponse{}, err
	}
	var result contract.HistoryUploadResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return contract.HistoryUploadResponse{}, err
	}
	return result, nil
}

func (c *Client) AbandonHistoryArtifactUpload(ctx context.Context, captureID, grant, temporaryUploadID string) error {
	path := "/v2/history/captures/" + url.PathEscape(captureID) + "/uploads/" + url.PathEscape(temporaryUploadID)
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set("Flow-History-Upload-Grant", grant)
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return statusError(response)
}

func (c *Client) PublishHistoryArtifact(ctx context.Context, captureID, grant string, input contract.PublishHistoryArtifactRequest) (contract.HistoryArtifact, error) {
	var response contract.HistoryArtifact
	err := c.doHistoryWorkerJSON(ctx, captureID, "artifacts", grant, input, &response)
	return response, err
}

func (c *Client) RegisterHistoryTranscriptSegment(ctx context.Context, captureID, grant string, input contract.RegisterHistoryTranscriptSegmentRequest) error {
	return c.doHistoryWorkerJSON(ctx, captureID, "transcript-segments", grant, input, nil)
}

func (c *Client) SealHistoryTranscript(ctx context.Context, captureID, grant string, input contract.HistoryTranscriptSeal) error {
	return c.doHistoryWorkerJSON(ctx, captureID, "transcript-seal", grant, input, nil)
}

func (c *Client) DeclareHistoryExpectedSet(ctx context.Context, captureID, grant string, input contract.DeclareHistoryExpectedSetRequest) (contract.HistoryCapture, error) {
	var response contract.HistoryCapture
	err := c.doHistoryWorkerJSON(ctx, captureID, "expected-set", grant, input, &response)
	return response, err
}

func (c *Client) RegisterHistoryWorkspaceSummary(ctx context.Context, captureID, grant string, input contract.RegisterHistoryWorkspaceSummaryRequest) (contract.HistoryWorkspaceSummary, error) {
	var response contract.HistoryWorkspaceSummary
	err := c.doHistoryWorkerJSON(ctx, captureID, "workspace-summary", grant, input, &response)
	return response, err
}

func (c *Client) RegisterHistoryHarnessMembers(ctx context.Context, captureID, grant string, input contract.RegisterHistoryHarnessMembersRequest) error {
	return c.doHistoryWorkerJSON(ctx, captureID, "harness-members", grant, input, nil)
}

func (c *Client) RecordHistoryExecutionVerdict(ctx context.Context, captureID, grant string, input contract.RecordHistoryExecutionVerdictRequest) (contract.HistoryCapture, error) {
	var response contract.HistoryCapture
	err := c.doHistoryWorkerJSON(ctx, captureID, "verdict", grant, input, &response)
	return response, err
}

func (c *Client) TransitionHistoryCapture(ctx context.Context, captureID, grant string, input contract.TransitionHistoryCaptureRequest) (contract.HistoryCapture, error) {
	var response contract.HistoryCapture
	err := c.doHistoryWorkerJSON(ctx, captureID, "transition", grant, input, &response)
	return response, err
}

func (c *Client) GenerateHistoryManifest(ctx context.Context, captureID, grant string) (contract.HistoryArtifact, error) {
	var response contract.HistoryArtifact
	err := c.doHistoryWorkerJSON(ctx, captureID, "manifest", grant, map[string]any{}, &response)
	return response, err
}

func (c *Client) CompleteHistoryCapture(ctx context.Context, captureID, grant string, expectedVersion int64) (contract.HistoryCapture, error) {
	var response contract.HistoryCapture
	err := c.doHistoryWorkerJSON(ctx, captureID, "complete", grant, contract.CompleteHistoryCaptureRequest{ExpectedVersion: expectedVersion}, &response)
	return response, err
}

func (c *Client) doHistoryWorkerJSON(ctx context.Context, captureID, operation, grant string, body, target any) error {
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return err
		}
	}
	path := "/v2/history/captures/" + url.PathEscape(captureID) + "/" + operation
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, &encoded)
	if err != nil {
		return err
	}
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Flow-History-Upload-Grant", grant)
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
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (c *Client) downloadHistoryContent(ctx context.Context, path string, dst io.Writer) error {
	if dst == nil {
		return errors.New("history download destination is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
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
	_, err = io.Copy(dst, response.Body)
	return err
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
	return c.doContextWithHeaders(ctx, method, path, body, query, nil, target)
}

func (c *Client) doContextWithHeaders(ctx context.Context, method string, path string, body any, query url.Values, headers http.Header, target any) error {
	_, err := c.doRequest(ctx, method, path, body, query, headers, target)
	return err
}

// doRequest performs the request and returns the response headers so callers
// can observe server signals such as idempotent replay.
func (c *Client) doRequest(ctx context.Context, method string, path string, body any, query url.Values, headers http.Header, target any) (http.Header, error) {
	var requestBody io.Reader
	if body != nil {
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return nil, err
		}
		requestBody = &encoded
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return nil, err
	}
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", authScheme+c.token)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}

	started := time.Now()
	slog.Debug("flow api request", "method", method, "path", path)
	response, err := c.httpClient.Do(request)
	if err != nil {
		slog.Debug("flow api request failed", "method", method, "path", path, "duration", time.Since(started), "error", err)
		return nil, err
	}
	defer response.Body.Close()
	slog.Debug("flow api response", "method", method, "path", path, "status", response.StatusCode, "duration", time.Since(started))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError errorResponse
		if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
			slog.Debug("flow api error response decode failed", "method", method, "path", path, "status", response.StatusCode, "error", err)
			return response.Header, &HTTPStatusError{StatusCode: response.StatusCode}
		}
		slog.Debug("flow api error response", "method", method, "path", path, "status", response.StatusCode, "code", apiError.Error.Code)
		return response.Header, &HTTPStatusError{
			StatusCode: response.StatusCode,
			Code:       apiError.Error.Code,
			Message:    apiError.Error.Message,
		}
	}
	if target == nil {
		return response.Header, nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		slog.Debug("flow api response decode failed", "method", method, "path", path, "status", response.StatusCode, "error", err)
		return response.Header, err
	}

	return response.Header, nil
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
	Title        string `json:"title"`
	Body         string `json:"body"`
	Priority     int    `json:"priority"`
	FlowID       string `json:"flow_id,omitempty"`
	FeatureID    string `json:"feature_id,omitempty"`
	ParentItemID string `json:"parent_item_id,omitempty"`
	// IdempotencyKey rides the Idempotency-Key header rather than the body so
	// retries of the same create replay the stored response instead of
	// creating a second task.
	IdempotencyKey string `json:"-"`
}

type EditTaskInput struct {
	Title     *string `json:"title,omitempty"`
	Body      *string `json:"body,omitempty"`
	Priority  *int    `json:"priority,omitempty"`
	FlowID    *string `json:"flow_id,omitempty"`
	FeatureID *string `json:"feature_id,omitempty"`
}

type CreateFeatureInput struct {
	Title        string `json:"title"`
	Body         string `json:"body"`
	ParentItemID string `json:"parent_item_id,omitempty"`
}

type UpdateFeatureInput struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
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
	// Ready restricts the list to unscheduled tasks with no unresolved
	// effective blockers (the same rule the schedule-time dependency gate
	// applies before starting workflow nodes).
	Ready bool
}

type ReviewFollowUpFinding = contract.ReviewFollowUpFinding

type ReviewFollowUpTaskAction = contract.ReviewFollowUpTaskAction

type ApplyReviewFollowUpInput = contract.ApplyReviewFollowUpRequest

type ApplyReviewFollowUpResult struct {
	Task        coordinator.Task
	Disposition string
}

type ProvisionerAssignment = contract.ProvisionerAssignment

type ReserveProvisionerAssignmentInput struct {
	ProviderID        string
	ProviderRequestID string
	ProfileName       string
	ProviderType      string
	ProviderOptions   map[string]string
	MaxConcurrency    int
	AllowedRoles      []flowworker.JobRole
	AllowedBuckets    []flowworker.CapacityBucket
	Labels            map[string]string
	Taints            []scheduler.Taint
	HarnessModels     []flowharness.Model
	RequiredSelector  map[string]string
	StartupTimeout    time.Duration
	Wait              time.Duration
}

type ReserveProvisionerAssignmentResult contract.ReserveProvisionerAssignmentResponse

type ProvisionerAssignmentFilter struct {
	ProjectID         string
	ProviderID        string
	ProfileName       string
	ProviderRequestID string
	WorkerID          string
	JobID             string
	States            []flowworker.AssignmentState
	OpenOnly          bool
	NeedsCleanup      bool
}

type RecordProvisionerAssignmentAttemptInput struct {
	ProviderError string
	NextRetryAt   *time.Time
}

type RegisterWorkerInput struct {
	ID            string
	Labels        map[string]string
	Taints        []scheduler.Taint
	HarnessModels []flowharness.Model
	HeartbeatTTL  time.Duration
}

type HeartbeatWorkerInput struct {
	WorkerID     string
	HeartbeatTTL time.Duration
}

type ClaimJobInput struct {
	WorkerID             string
	LeaseDuration        time.Duration
	Wait                 time.Duration
	CapabilitiesReported bool
	Labels               map[string]string
	Taints               []scheduler.Taint
	HarnessModels        []flowharness.Model
	HeartbeatTTL         time.Duration
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
	Harness        flowworker.HarnessRequirement
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
type featureResponse = contract.FeatureResponse
type featuresResponse = contract.FeaturesResponse
type rebaseFeatureResponse = contract.RebaseFeatureResponse
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
