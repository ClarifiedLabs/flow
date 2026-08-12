// Package mcp serves flow's agent-facing read surface over the Model Context
// Protocol. The catalog is deliberately read-only: writes and admin operations
// (create/done/reset/merge, feature/epic/flows/agent-def admin, workers/jobs)
// are never registered, so an MCP client cannot mutate coordinator state.
package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// maxToolResults caps every list-returning tool.
const maxToolResults = 100

// toolResult is the common structured payload: the bound project plus the
// per-tool data. Every result carries the project so an agent connected to a
// multi-project server can disambiguate.
type toolResult struct {
	ProjectID string `json:"project_id"`
	Data      any    `json:"data"`
}

// TaskSummary is the compact per-task shape (bodies omitted in list/ready).
type TaskSummary struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	State      string `json:"state,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// NewServer builds the MCP server over a resolved, project-scoped client.
// Only the read toolset is registered.
func NewServer(client *flowclient.Client) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "flow",
		Title:   "Flow coordinator",
		Version: "1",
	}, &sdk.ServerOptions{
		Instructions: "Flow read tools: list/show/search tasks, readiness, and the project event log. No writes are exposed.",
	})
	project := client.ProjectRef()
	wrap := func(data any, err error) (*sdk.CallToolResult, toolResult, error) {
		if err != nil {
			return nil, toolResult{}, err
		}
		return nil, toolResult{ProjectID: project, Data: data}, nil
	}
	_ = wrap

	sdk.AddTool(server, &sdk.Tool{
		Name:        "flow.task_list",
		Description: "List tasks in the bound project (compact; bodies omitted). Optional state/tag/ready filters. Capped at 100.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, in taskListInput) (*sdk.CallToolResult, toolResult, error) {
		filter := flowclient.TaskFilter{TagSlugs: in.Tags, Ready: in.Ready}
		if in.State != "" {
			filter.LifecycleStates = []string{in.State}
		}
		tasks, err := client.ListTasks(filter)
		if err != nil {
			return nil, toolResult{}, err
		}
		return nil, toolResult{ProjectID: project, Data: summarizeTasks(tasks, true)}, nil
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "flow.task_show",
		Description: "Show one task's detail, including completion resolution/message/evidence.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, in taskShowInput) (*sdk.CallToolResult, toolResult, error) {
		if in.ID == "" {
			return nil, toolResult{}, fmt.Errorf("id is required")
		}
		task, err := client.GetTask(in.ID)
		if err != nil {
			return nil, toolResult{}, err
		}
		return nil, toolResult{ProjectID: project, Data: taskDetail(task)}, nil
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "flow.ready",
		Description: "List ready (unblocked, unscheduled) tasks, ordered by priority then creation time.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, in struct{}) (*sdk.CallToolResult, toolResult, error) {
		tasks, err := client.ListTasks(flowclient.TaskFilter{Ready: true})
		if err != nil {
			return nil, toolResult{}, err
		}
		return nil, toolResult{ProjectID: project, Data: summarizeTasks(tasks, true)}, nil
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "flow.search",
		Description: "Full-text (or substring) task search by title/body. Optional limit.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, in searchInput) (*sdk.CallToolResult, toolResult, error) {
		if in.Query == "" {
			return nil, toolResult{}, fmt.Errorf("query is required")
		}
		limit := in.Limit
		if limit <= 0 || limit > maxToolResults {
			limit = maxToolResults
		}
		tasks, err := client.SearchTasks(in.Query, limit)
		if err != nil {
			return nil, toolResult{}, err
		}
		return nil, toolResult{ProjectID: project, Data: summarizeTasks(tasks, true)}, nil
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "flow.events",
		Description: "Read one page of the project event log with seq > since, plus the resumable next_since cursor. Optional kind/task filters.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, in eventsInput) (*sdk.CallToolResult, toolResult, error) {
		limit := in.Limit
		if limit <= 0 || limit > maxToolResults {
			limit = maxToolResults
		}
		events, next, err := client.ListEvents(in.Since, limit, coordinator.EventFilter{Kind: in.Kind, TaskID: in.Task})
		if err != nil {
			return nil, toolResult{}, err
		}
		return nil, toolResult{ProjectID: project, Data: map[string]any{"events": events, "next_since": next}}, nil
	})

	return server
}

type taskListInput struct {
	State string   `json:"state,omitempty" jsonschema:"lifecycle state filter: unscheduled|scheduled|in_progress|done"`
	Tags  []string `json:"tags,omitempty" jsonschema:"tag slug filters"`
	Ready bool     `json:"ready,omitempty" jsonschema:"restrict to unblocked unscheduled tasks"`
}

type taskShowInput struct {
	ID string `json:"id" jsonschema:"task id"`
}

type searchInput struct {
	Query string `json:"query" jsonschema:"search text"`
	Limit int    `json:"limit,omitempty" jsonschema:"max hits (default 100)"`
}

type eventsInput struct {
	Since int64  `json:"since,omitempty" jsonschema:"only events with seq greater than this"`
	Limit int    `json:"limit,omitempty" jsonschema:"max events (default 100)"`
	Kind  string `json:"kind,omitempty" jsonschema:"event kind filter"`
	Task  string `json:"task,omitempty" jsonschema:"task id filter"`
}

// summarizeTasks compacts tasks for list/ready/search (bodies omitted), capped.
func summarizeTasks(tasks []coordinator.Task, compact bool) []TaskSummary {
	if len(tasks) > maxToolResults {
		tasks = tasks[:maxToolResults]
	}
	out := make([]TaskSummary, 0, len(tasks))
	for _, task := range tasks {
		summary := TaskSummary{ID: task.ID, Title: task.Title}
		if task.State != nil {
			summary.State = string(*task.State)
		}
		if task.DoneResolution != nil {
			summary.Resolution = string(*task.DoneResolution)
		}
		out = append(out, summary)
	}
	return out
}

// taskDetail returns the full task including completion detail.
func taskDetail(task coordinator.Task) map[string]any {
	detail := map[string]any{
		"id":     task.ID,
		"title":  task.Title,
		"body":   task.Body,
		"status": "unscheduled",
	}
	if task.State != nil {
		detail["status"] = string(*task.State)
	}
	if task.DoneResolution != nil {
		detail["done_resolution"] = string(*task.DoneResolution)
	}
	if task.DoneMessage != "" {
		detail["done_message"] = task.DoneMessage
	}
	if len(task.DoneEvidence) > 0 {
		detail["done_evidence"] = task.DoneEvidence
	}
	return detail
}
