package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func (s *projectServer) handleEpicsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if s.epics == nil || s.workItems == nil {
		writeError(w, http.StatusServiceUnavailable, "epics_unavailable", "epic service is not configured")
		return
	}
	parts := splitResourcePath(r.URL.Path, "/v2/epics")
	read := func() bool {
		return requireScope(w, principal, "owner, console, or session token is required",
			coordinator.TokenScopeOwner, coordinator.TokenScopeConsole, coordinator.TokenScopeSession)
	}
	write := func() bool {
		if !requireScope(w, principal, "owner or console token is required",
			coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return false
		}
		if principal.Scope == coordinator.TokenScopeConsole && principal.SourceTaskID != nil {
			writeError(w, http.StatusForbidden, "forbidden", "task-bound console cannot create or edit containers")
			return false
		}
		return true
	}
	switch {
	case len(parts) == 0 && r.Method == http.MethodGet:
		if read() {
			s.handleListEpics(w, r)
		}
	case len(parts) == 0 && r.Method == http.MethodPost:
		if write() {
			s.handleCreateEpic(w, r, principal)
		}
	case len(parts) == 1 && r.Method == http.MethodGet:
		if read() {
			s.handleGetEpicItem(w, r, parts[0])
		}
	case len(parts) == 1 && r.Method == http.MethodPatch:
		if write() {
			s.handleEditEpic(w, r, parts[0])
		}
	case len(parts) == 2 && r.Method == http.MethodPost:
		switch parts[1] {
		case "start":
			if requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
				s.handleStartContainer(w, r, parts[0], principal)
			}
		case "complete":
			if write() {
				s.handleCompleteEpic(w, r, parts[0], principal)
			}
		case "reopen":
			if write() {
				s.handleReopenEpic(w, r, parts[0], principal)
			}
		case "archive":
			if write() {
				s.handleArchiveEpic(w, r, parts[0], principal)
			}
		default:
			writeError(w, http.StatusNotFound, "not_found", "unknown epics route")
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *projectServer) handleListEpics(w http.ResponseWriter, r *http.Request) {
	status := coordinator.EpicStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	if status == "all" {
		status = ""
	}
	epics, err := s.epics.List(r.Context(), status)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	response := contract.EpicsResponse{Epics: make([]contract.EpicResponse, 0, len(epics))}
	for _, epic := range epics {
		payload, err := s.epicResponse(r, epic)
		if err != nil {
			writeWorkItemError(w, err)
			return
		}
		response.Epics = append(response.Epics, payload)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) handleCreateEpic(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request contract.CreateEpicRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	epic, err := s.epics.Create(r.Context(), coordinator.CreateEpicInput{
		Title: request.Title, Body: request.Body, Priority: request.Priority,
		CompletionPolicy: coordinator.EpicCompletionPolicy(request.CompletionPolicy),
		ParentItemID:     request.ParentItemID, WorkItemRelations: createWorkItemRelationInputs(request.WorkItemRelations, workflowActor(principal)),
		CreatedBy: workflowActor(principal),
	})
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	payload, err := s.epicResponse(r, epic)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, payload)
}

func (s *projectServer) handleGetEpicItem(w http.ResponseWriter, r *http.Request, id string) {
	epic, err := s.epics.Get(r.Context(), id)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	payload, err := s.epicResponse(r, epic)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) handleEditEpic(w http.ResponseWriter, r *http.Request, id string) {
	var request contract.EditEpicRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	var policy *coordinator.EpicCompletionPolicy
	if request.CompletionPolicy != nil {
		value := coordinator.EpicCompletionPolicy(*request.CompletionPolicy)
		policy = &value
	}
	epic, err := s.epics.Edit(r.Context(), id, coordinator.EditEpicInput{
		Title: request.Title, Body: request.Body, Priority: request.Priority, CompletionPolicy: policy,
	})
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	payload, err := s.epicResponse(r, epic)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) handleCompleteEpic(w http.ResponseWriter, r *http.Request, id string, principal coordinator.Principal) {
	epic, err := s.epics.Complete(r.Context(), id, workflowActor(principal))
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	payload, err := s.epicResponse(r, epic)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) handleReopenEpic(w http.ResponseWriter, r *http.Request, id string, principal coordinator.Principal) {
	epic, err := s.epics.Reopen(r.Context(), id, workflowActor(principal))
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	payload, err := s.epicResponse(r, epic)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) handleArchiveEpic(w http.ResponseWriter, r *http.Request, id string, principal coordinator.Principal) {
	epic, err := s.epics.Archive(r.Context(), id, workflowActor(principal))
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	payload, err := s.epicResponse(r, epic)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *projectServer) epicResponse(r *http.Request, epic coordinator.Epic) (contract.EpicResponse, error) {
	item, err := s.workItems.Get(r.Context(), epic.ID)
	if err != nil {
		return contract.EpicResponse{}, err
	}
	children, err := s.workItems.Children(r.Context(), epic.ID)
	if err != nil {
		return contract.EpicResponse{}, err
	}
	blockers, err := s.workItems.EffectiveBlockers(r.Context(), epic.ID, true)
	if err != nil {
		return contract.EpicResponse{}, err
	}
	return contract.EpicResponse{Epic: epic, Item: item, Children: children, Blockers: blockers}, nil
}

func (s *projectServer) handleWorkItemsPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	parts := splitResourcePath(r.URL.Path, "/v2/work-items")
	if !requireScope(w, principal, "owner, console, or session token is required",
		coordinator.TokenScopeOwner, coordinator.TokenScopeConsole, coordinator.TokenScopeSession) {
		return
	}
	if s.workItems == nil {
		writeError(w, http.StatusServiceUnavailable, "work_items_unavailable", "work-item service is not configured")
		return
	}
	switch {
	case len(parts) == 0 && r.Method == http.MethodGet:
		s.handleListWorkItems(w, r)
	case len(parts) == 1 && parts[0] == "overview" && r.Method == http.MethodGet:
		s.handleWorkItemOverview(w, r)
	case len(parts) == 1 && parts[0] == "doctor" && r.Method == http.MethodGet:
		if !requireScope(w, principal, "owner token is required", coordinator.TokenScopeOwner) {
			return
		}
		report, err := s.workItems.CheckConsistency(r.Context())
		if err != nil {
			writeWorkItemError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, report)
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.handleGetWorkItem(w, r, parts[0], false)
	case len(parts) == 2 && parts[1] == "context" && r.Method == http.MethodGet:
		s.handleWorkItemContext(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "tree" && r.Method == http.MethodGet:
		s.handleGetWorkItem(w, r, parts[0], true)
	case len(parts) == 2 && parts[1] == "relations" && r.Method == http.MethodGet:
		relations, err := s.workItems.Relations(r.Context(), parts[0])
		if err != nil {
			writeWorkItemError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"relations": relations})
	case len(parts) == 2 && parts[1] == "relations" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleMutateWorkItemRelation(w, r, principal, parts[0])
	case len(parts) == 1 && parts[0] == "parents" && r.Method == http.MethodPatch:
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		s.handleMoveWorkItems(w, r, principal)
	case len(parts) == 2 && parts[1] == "parent" && r.Method == http.MethodPatch:
		if !requireScope(w, principal, "owner or console token is required", coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
			return
		}
		if principal.Scope == coordinator.TokenScopeConsole && principal.SourceTaskID != nil && *principal.SourceTaskID != parts[0] {
			writeError(w, http.StatusForbidden, "forbidden", "task-bound console may move only its bound task")
			return
		}
		var request contract.MoveWorkItemRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if err := s.workItems.Move(r.Context(), parts[0], request.ParentItemID, workflowActor(principal)); err != nil {
			writeWorkItemError(w, err)
			return
		}
		s.handleGetWorkItem(w, r, parts[0], false)
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown work-items route")
	}
}

func (s *projectServer) handleMoveWorkItems(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	var request contract.MoveWorkItemsRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if principal.Scope == coordinator.TokenScopeConsole && principal.SourceTaskID != nil {
		boundID := strings.TrimSpace(*principal.SourceTaskID)
		if len(request.ItemIDs) != 1 || strings.TrimSpace(request.ItemIDs[0]) != boundID {
			writeError(w, http.StatusForbidden, "forbidden", "task-bound console may move only its bound task in a singleton request")
			return
		}
	}
	items, err := s.workItems.MoveMany(r.Context(), request.ItemIDs, request.ParentItemID, workflowActor(principal))
	if err != nil {
		var validation *coordinator.WorkItemMoveValidationError
		if errors.As(err, &validation) {
			writeJSON(w, http.StatusUnprocessableEntity, contract.ErrorResponse{Error: contract.ErrorBody{
				Code: "work_item_move_invalid", Message: "bulk parent change validation failed", Issues: validation.Issues,
			}})
			return
		}
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, contract.MoveWorkItemsResponse{Items: items})
}

func (s *projectServer) handleListWorkItems(w http.ResponseWriter, r *http.Request) {
	items, err := s.workItems.List(r.Context())
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	kind := coordinator.WorkItemKind(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind != "" && kind != coordinator.WorkItemTask && kind != coordinator.WorkItemEpic && kind != coordinator.WorkItemFeature {
		writeError(w, http.StatusBadRequest, "invalid_filter", "kind must be task, epic, or feature")
		return
	}
	parentID := strings.TrimSpace(r.URL.Query().Get("parent"))
	featureID := strings.TrimSpace(r.URL.Query().Get("feature_scope"))
	terminal, terminalSet, err := optionalBoolQuery(r, "terminal")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	blocked, blockedSet, err := optionalBoolQuery(r, "blocked")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	filtered := items[:0]
	for _, item := range items {
		if kind != "" && item.Kind != kind || parentID != "" && item.ParentItemID != parentID || featureID != "" && item.EffectiveFeatureID != featureID ||
			terminalSet && item.State.Terminal != terminal || blockedSet && (item.UnresolvedBlockers != 0) != blocked {
			continue
		}
		filtered = append(filtered, item)
	}
	writeJSON(w, http.StatusOK, contract.WorkItemsResponse{Items: filtered})
}

func optionalBoolQuery(r *http.Request, name string) (bool, bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return false, false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false, errors.New(name + " must be true or false")
	}
	return value, true, nil
}

func (s *projectServer) handleWorkItemOverview(w http.ResponseWriter, r *http.Request) {
	terminal, terminalSet, err := optionalBoolQuery(r, "terminal")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	var terminalFilter *bool
	if terminalSet {
		terminalFilter = &terminal
	}
	aggregates, err := s.workItems.Overview(r.Context(), terminalFilter)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	items, err := s.composeWorkItemOverview(r.Context(), aggregates)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "work_item_overview_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, contract.WorkItemOverviewResponse{Items: items})
}

func (s *projectServer) handleWorkItemContext(w http.ResponseWriter, r *http.Request, id string) {
	contextView, err := s.workItems.Context(r.Context(), id)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	entries, err := s.composeWorkItemOverview(r.Context(), []coordinator.WorkItemOverview{contextView.Overview})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "work_item_context_failed", err.Error())
		return
	}
	if len(entries) != 1 {
		writeError(w, http.StatusInternalServerError, "work_item_context_failed", "context composition returned no item")
		return
	}
	entry := entries[0]
	writeJSON(w, http.StatusOK, contract.WorkItemContextResponse{
		Item: contextView.Overview.Item, Ancestors: contextView.Ancestors,
		Children: contextView.Children, Blockers: contextView.Blockers, Relations: contextView.Relations,
		Rollup: entry.Rollup, AttentionCount: entry.AttentionCount, Feature: entry.Feature, Actions: entry.Actions,
	})
}

func (s *projectServer) composeWorkItemOverview(ctx context.Context, aggregates []coordinator.WorkItemOverview) ([]contract.WorkItemOverviewEntry, error) {
	attention := map[string]bool{}
	if s.tasks != nil {
		board, err := s.tasks.BoardResult(ctx)
		if err != nil {
			return nil, err
		}
		cards, err := s.buildUITaskCards(ctx, boardTasks(board.Board))
		if err != nil {
			return nil, err
		}
		for taskID, card := range cards {
			attention[taskID] = uiTaskNeedsAttention(card, board.LaneStates[taskID])
		}
	}

	epics := map[string]coordinator.Epic{}
	if s.epics != nil {
		listed, err := s.epics.List(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, epic := range listed {
			epics[epic.ID] = epic
		}
	}
	features := map[string]coordinator.Feature{}
	if s.features != nil {
		listed, err := s.features.List(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, feature := range listed {
			features[feature.ID] = feature
		}
	}

	result := make([]contract.WorkItemOverviewEntry, 0, len(aggregates))
	for _, aggregate := range aggregates {
		entry := contract.WorkItemOverviewEntry{
			Item:   aggregate.Item,
			Rollup: contract.WorkItemRollup{DirectChildren: aggregate.Rollup.DirectChildren, DescendantTasks: aggregate.Rollup.DescendantTasks},
		}
		for _, taskID := range aggregate.DescendantTaskIDs {
			if attention[taskID] {
				entry.AttentionCount++
			}
		}
		entry.Actions.Start = startReadiness(aggregate)
		if epic, ok := epics[aggregate.Item.ID]; ok {
			entry.Actions.Complete = completeReadiness(aggregate, epic)
		}
		if feature, ok := features[aggregate.Item.ID]; ok {
			entry.Feature, entry.Actions.Rebase, entry.Actions.Land = s.featureOverview(ctx, aggregate, feature, features)
		}
		result = append(result, entry)
	}
	return result, nil
}

func startReadiness(aggregate coordinator.WorkItemOverview) *contract.ActionReadiness {
	if !aggregate.Item.Capabilities.CanStart {
		return nil
	}
	if aggregate.Item.State.Terminal {
		return deniedReadiness("item_terminal", "Terminal containers cannot be started.")
	}
	return &contract.ActionReadiness{Allowed: true}
}

func completeReadiness(aggregate coordinator.WorkItemOverview, epic coordinator.Epic) *contract.ActionReadiness {
	if aggregate.Item.State.Terminal {
		return deniedReadiness("item_terminal", "This epic is already terminal.")
	}
	if aggregate.Item.UnresolvedBlockers > 0 {
		return deniedReadiness("effective_blockers", "Resolve the epic's effective blockers before completing it.")
	}
	children := aggregate.Rollup.DirectChildren
	if children.Terminal != children.Total {
		return deniedReadiness("active_direct_children", "All direct children must be terminal before completing this epic.")
	}
	if epic.CompletionPolicy == coordinator.EpicAllChildren && children.Total == 0 {
		return deniedReadiness("no_direct_children", "An all-children epic needs at least one direct child before completion.")
	}
	return &contract.ActionReadiness{Allowed: true}
}

func (s *projectServer) featureOverview(ctx context.Context, aggregate coordinator.WorkItemOverview, feature coordinator.Feature, features map[string]coordinator.Feature) (*contract.FeatureOverview, *contract.ActionReadiness, *contract.ActionReadiness) {
	target := s.project.BaseBranch
	if feature.IntegrationFeatureID != nil {
		if parent, ok := features[*feature.IntegrationFeatureID]; ok {
			target = parent.Branch
		} else {
			target = *feature.IntegrationFeatureID
		}
	}
	view := &contract.FeatureOverview{Branch: feature.Branch, IntegrationTarget: target}
	running, found, runningErr := s.features.RunningRebase(ctx, feature.ID)
	if runningErr == nil && found {
		view.RunningRebase = &running
	}
	branchState, gitErr := s.features.BranchState(ctx, feature.ID)
	if gitErr != nil {
		view.GitUnavailableReason = gitErr.Error()
	} else {
		view.GitAvailable = true
		view.Ahead, view.Behind = branchState.Ahead, branchState.Behind
	}

	if feature.Status != coordinator.FeatureOpen {
		denial := deniedReadiness("feature_closed", "Only open features can be rebased or landed.")
		return view, denial, denial
	}
	if runningErr != nil {
		denial := deniedReadiness("rebase_state_unavailable", "The running rebase state could not be read.")
		return view, denial, denial
	}
	if found {
		denial := deniedReadiness("rebase_running", "A feature rebase is already running.")
		return view, denial, denial
	}
	if gitErr != nil {
		denial := deniedReadiness("git_unavailable", "Git state is unavailable; retry after the exchange is accessible.")
		return view, denial, denial
	}
	rebase := &contract.ActionReadiness{Allowed: true}
	if aggregate.Item.UnresolvedBlockers > 0 {
		return view, rebase, deniedReadiness("effective_blockers", "Resolve the feature's effective blockers before landing it.")
	}
	rollup := aggregate.Rollup.DescendantTasks
	if rollup.SuccessfulTerminal+rollup.UnsuccessfulTerminal != rollup.Total {
		return view, rebase, deniedReadiness("active_descendant_tasks", "All descendant tasks must be terminal before landing this feature.")
	}
	if aggregate.Rollup.DirectChildren.Terminal != aggregate.Rollup.DirectChildren.Total {
		return view, rebase, deniedReadiness("active_direct_children", "All direct child containers must be terminal before landing this feature.")
	}
	return view, rebase, &contract.ActionReadiness{Allowed: true}
}

func deniedReadiness(code, text string) *contract.ActionReadiness {
	return &contract.ActionReadiness{Allowed: false, ReasonCode: code, DenialText: text}
}

func (s *projectServer) handleGetWorkItem(w http.ResponseWriter, r *http.Request, id string, tree bool) {
	item, err := s.workItems.Get(r.Context(), id)
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	response := contract.WorkItemResponse{Item: item}
	response.Relations, err = s.workItems.Relations(r.Context(), id)
	if err == nil {
		response.Blockers, err = s.workItems.EffectiveBlockers(r.Context(), id, true)
	}
	if err == nil && tree {
		response.Children, err = s.workItemChildren(r, id)
	}
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *projectServer) workItemChildren(r *http.Request, id string) ([]contract.WorkItemResponse, error) {
	children, err := s.workItems.Children(r.Context(), id)
	if err != nil {
		return nil, err
	}
	result := make([]contract.WorkItemResponse, 0, len(children))
	for _, child := range children {
		nested, err := s.workItemChildren(r, child.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, contract.WorkItemResponse{Item: child, Children: nested})
	}
	return result, nil
}

func (s *projectServer) handleMutateWorkItemRelation(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, itemID string) {
	var request contract.WorkItemRelationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if request.SourceItemID != itemID && request.TargetItemID != itemID {
		writeError(w, http.StatusBadRequest, "invalid_work_item_relation", "relation must involve the work item named by the route")
		return
	}
	if err := checkBoundTaskRelationScope(principal, request.SourceItemID, request.TargetItemID); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	var err error
	if r.Method == http.MethodPost {
		err = s.workItems.Link(r.Context(), request.SourceItemID, request.TargetItemID, coordinator.RelationKind(request.Kind), workflowActor(principal))
	} else {
		err = s.workItems.Unlink(r.Context(), request.SourceItemID, request.TargetItemID, coordinator.RelationKind(request.Kind))
	}
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *projectServer) handleStartContainer(w http.ResponseWriter, r *http.Request, id string, principal coordinator.Principal) {
	if s.containers == nil {
		writeError(w, http.StatusServiceUnavailable, "containers_unavailable", "container service is not configured")
		return
	}
	if principal.Scope == coordinator.TokenScopeConsole && principal.SourceTaskID != nil {
		ancestors, err := s.workItems.Ancestors(r.Context(), *principal.SourceTaskID)
		if err != nil {
			writeWorkItemError(w, err)
			return
		}
		allowed := false
		for _, ancestor := range ancestors {
			if ancestor.ID == id {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "forbidden", "task-bound console may start only a container that owns its task")
			return
		}
	}
	result, err := s.containers.Start(r.Context(), id, workflowActor(principal))
	if err != nil {
		writeWorkItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func splitResourcePath(path, prefix string) []string {
	sub := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if sub == "" {
		return nil
	}
	return strings.Split(sub, "/")
}

func writeWorkItemError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, coordinator.ErrWorkItemNotFound), errors.Is(err, coordinator.ErrEpicNotFound):
		writeError(w, http.StatusNotFound, "work_item_not_found", err.Error())
	case errors.Is(err, coordinator.ErrWorkItemNotSchedulable):
		writeError(w, http.StatusConflict, "work_item_not_schedulable", err.Error())
	case errors.Is(err, coordinator.ErrActiveRebaseBlocker):
		writeError(w, http.StatusConflict, "active_rebase_blocker", err.Error())
	case errors.Is(err, coordinator.ErrWorkItemHasParent), errors.Is(err, coordinator.ErrWorkItemCycle),
		errors.Is(err, coordinator.ErrWorkItemMoveConflict), errors.Is(err, coordinator.ErrWorkItemParentClosed),
		errors.Is(err, coordinator.ErrWorkItemBlocked),
		errors.Is(err, coordinator.ErrEpicClosed), errors.Is(err, coordinator.ErrEpicNotCompletable),
		errors.Is(err, coordinator.ErrWorkItemRelationExists):
		writeError(w, http.StatusConflict, "work_item_conflict", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "invalid_work_item", err.Error())
	}
}
