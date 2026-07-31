package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// An epic is a task whose flow produced other tasks. Nothing new is stored for
// it: materializing a task_set artifact already links each child to its planner
// with a parent_of relation and applies the manifest's dependencies as blocks
// relations. This is only the read.

type epicResponse struct {
	Epic         epicSummary  `json:"epic"`
	Members      []epicMember `json:"members"`
	CriticalPath []string     `json:"critical_path,omitempty"`
	TotalCount   int          `json:"total_count"`
	MergedCount  int          `json:"merged_count"`
}

type epicSummary struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type epicMember struct {
	ID         string                     `json:"id"`
	Title      string                     `json:"title"`
	State      string                     `json:"state"`
	LaneState  coordinator.LaneState      `json:"lane_state,omitempty"`
	Resolution coordinator.DoneResolution `json:"resolution,omitempty"`
	StepIndex  int                        `json:"step_index,omitempty"`
	StepCount  int                        `json:"step_count,omitempty"`
	StepName   string                     `json:"step_name,omitempty"`
	DwellSince *time.Time                 `json:"dwell_since,omitempty"`
	Wait       *uiWorkflowWait            `json:"wait,omitempty"`
	Held       bool                       `json:"held,omitempty"`
	BlockedBy  []string                   `json:"blocked_by,omitempty"`
	NeedsYou   bool                       `json:"needs_you,omitempty"`
}

func (s *projectServer) handleGetEpic(w http.ResponseWriter, r *http.Request, taskID string) {
	ctx := r.Context()
	epic, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task_not_found", err.Error())
		return
	}

	memberIDs, err := s.epicMemberIDs(ctx, epic.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_epic_failed", err.Error())
		return
	}

	response := epicResponse{
		Epic:       epicSummary{ID: epic.ID, Title: epic.Title},
		TotalCount: len(memberIDs),
	}
	blockedBy := map[string][]string{}
	for _, memberID := range memberIDs {
		member, err := s.epicMember(ctx, memberID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load_epic_member_failed", err.Error())
			return
		}
		blockedBy[member.ID] = member.BlockedBy
		if member.Resolution == coordinator.ResolutionMerged {
			response.MergedCount++
		}
		response.Members = append(response.Members, member)
	}

	// Members that need a human sort to the top; the rest keep dependency
	// order, longest-waiting first, so the list reads as a queue.
	sort.SliceStable(response.Members, func(i, j int) bool {
		left, right := response.Members[i], response.Members[j]
		if left.NeedsYou != right.NeedsYou {
			return left.NeedsYou
		}
		if left.DwellSince != nil && right.DwellSince != nil && !left.DwellSince.Equal(*right.DwellSince) {
			return left.DwellSince.Before(*right.DwellSince)
		}
		return left.ID < right.ID
	})
	response.CriticalPath = epicCriticalPath(memberIDs, blockedBy)

	writeJSON(w, http.StatusOK, response)
}

// epicMemberIDs returns the tasks this task planned, preferring the parent_of
// relation the materializer writes and falling back to source_task_id for tasks
// created directly by an agent session.
func (s *projectServer) epicMemberIDs(ctx context.Context, epicID string) ([]string, error) {
	relations, err := s.tasks.RelationsForTask(ctx, epicID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, relation := range relations {
		if relation.Kind != coordinator.RelationParentOf || relation.SourceTaskID != epicID {
			continue
		}
		if seen[relation.TargetTaskID] {
			continue
		}
		seen[relation.TargetTaskID] = true
		ids = append(ids, relation.TargetTaskID)
	}
	rows, err := s.tasks.TaskIDsWithSource(ctx, epicID)
	if err != nil {
		return nil, err
	}
	for _, id := range rows {
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *projectServer) epicMember(ctx context.Context, taskID string) (epicMember, error) {
	task, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return epicMember{}, err
	}
	member := epicMember{ID: task.ID, Title: task.Title, State: "unscheduled"}
	if task.State != nil {
		member.State = string(*task.State)
	}
	if task.DoneResolution != nil {
		member.Resolution = *task.DoneResolution
	}

	blockers, err := s.tasks.UnresolvedBlockers(ctx, task.ID)
	if err != nil {
		return epicMember{}, err
	}
	for _, blocker := range blockers {
		member.BlockedBy = append(member.BlockedBy, blocker.ID)
	}

	if s.workflowRuns == nil || task.State == nil || *task.State == coordinator.LifecycleDone {
		return member, nil
	}
	state, ok, err := s.workflowRuns.CardState(ctx, task.ID)
	if err != nil {
		return epicMember{}, err
	}
	if !ok {
		return member, nil
	}
	member.StepIndex = state.StepIndex
	member.StepCount = state.StepCount
	member.StepName = coordinator.NodeKeyLabel(state.StepKey)
	member.Held = state.Held
	if !state.DwellSince.IsZero() {
		dwell := state.DwellSince
		member.DwellSince = &dwell
	}
	if state.Wait != nil {
		member.Wait = &uiWorkflowWait{
			Kind:      state.Wait.Kind,
			Reason:    state.Wait.Reason,
			Message:   state.Wait.Message,
			NodeRunID: state.Wait.NodeRunID,
			CreatedAt: state.Wait.CreatedAt,
		}
		member.NeedsYou = state.Wait.Kind != coordinator.WorkflowWaitAgentRequest
	}
	if state.Held {
		member.LaneState = coordinator.LaneStateHeld
	}
	return member, nil
}

// epicCriticalPath walks the members' blocks relations longest-chain-first, so
// the footer can show which sequence actually gates the epic. Members outside
// any dependency chain are omitted.
func epicCriticalPath(memberIDs []string, blockedBy map[string][]string) []string {
	members := map[string]bool{}
	for _, id := range memberIDs {
		members[id] = true
	}

	var best []string
	memo := map[string][]string{}
	var walk func(id string, seen map[string]bool) []string
	walk = func(id string, seen map[string]bool) []string {
		if cached, ok := memo[id]; ok {
			return cached
		}
		if seen[id] {
			return nil
		}
		seen[id] = true
		defer delete(seen, id)

		var longest []string
		for _, blocker := range blockedBy[id] {
			if !members[blocker] {
				continue
			}
			if chain := walk(blocker, seen); len(chain) > len(longest) {
				longest = chain
			}
		}
		chain := append(append([]string{}, longest...), id)
		memo[id] = chain
		return chain
	}

	for _, id := range memberIDs {
		if chain := walk(id, map[string]bool{}); len(chain) > len(best) {
			best = chain
		}
	}
	if len(best) < 2 {
		return nil
	}
	return best
}
