package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// WorkItemMoveIssue is a stable, item-specific reason a bulk parent change was
// rejected. ItemID names the participant that must be fixed; it can be empty
// only for request-wide issues such as an empty item_ids list.
type WorkItemMoveIssue struct {
	ItemID  string `json:"item_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WorkItemMoveValidationError reports every validation issue found before a
// bulk move mutates the transaction.
type WorkItemMoveValidationError struct {
	Issues []WorkItemMoveIssue
}

func (e *WorkItemMoveValidationError) Error() string {
	if len(e.Issues) == 0 {
		return "work item move validation failed"
	}
	return e.Issues[0].Message
}

// MoveMany atomically replaces the organizational parent of every requested
// item. Validation evaluates the final hierarchy and dependency graph before
// the first write; response order follows itemIDs.
func (s *WorkItemService) MoveMany(ctx context.Context, itemIDs []string, newParentID string, actor Actor) ([]WorkItemSummary, error) {
	actor = defaultActor(actor, ActorHuman)
	if err := validateActor(actor); err != nil {
		return nil, err
	}
	newParentID = strings.TrimSpace(newParentID)
	normalized := make([]string, len(itemIDs))
	for i, id := range itemIDs {
		normalized[i] = strings.TrimSpace(id)
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	issues := make([]WorkItemMoveIssue, 0)
	seen := make(map[string]bool, len(normalized))
	selected := make(map[string]bool, len(normalized))
	if len(normalized) == 0 {
		issues = append(issues, WorkItemMoveIssue{Code: "item_ids_required", Message: "item_ids must contain at least one work item"})
	}
	for _, id := range normalized {
		if id == "" {
			issues = append(issues, WorkItemMoveIssue{Code: "item_id_required", Message: "item_ids cannot contain a blank work item ID"})
			continue
		}
		if seen[id] {
			issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "duplicate_item_id", Message: fmt.Sprintf("work item %s appears more than once", id)})
			continue
		}
		seen[id] = true
		selected[id] = true
	}

	kinds, parentByChild, children, dependency, err := loadWorkItemMoveGraph(ctx, tx)
	if err != nil {
		return nil, err
	}
	oldParents := make(map[string]string, len(selected))
	for id := range selected {
		kind, ok := kinds[id]
		if !ok {
			issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "item_not_found", Message: fmt.Sprintf("work item %s was not found", id)})
			continue
		}
		oldParents[id] = parentByChild[id]
		if err := requireWorkItemRelationMutableTx(ctx, tx, id, kind); err != nil {
			if errors.Is(err, ErrWorkItemParentClosed) {
				issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "item_immutable", Message: fmt.Sprintf("work item %s is archived and cannot be moved", id)})
				continue
			}
			return nil, err
		}
		if oldParentID := parentByChild[id]; oldParentID != "" {
			oldKind, ok := kinds[oldParentID]
			if !ok {
				return nil, fmt.Errorf("load parent %s for %s: %w", oldParentID, id, ErrWorkItemNotFound)
			}
			if err := requireWorkItemRelationMutableTx(ctx, tx, oldParentID, oldKind); err != nil {
				if errors.Is(err, ErrWorkItemParentClosed) {
					issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "old_parent_immutable", Message: fmt.Sprintf("current parent %s is archived and cannot participate in a move", oldParentID)})
					continue
				}
				return nil, err
			}
		}
	}

	if newParentID != "" {
		parentKind, ok := kinds[newParentID]
		if !ok {
			issues = append(issues, WorkItemMoveIssue{ItemID: newParentID, Code: "parent_not_found", Message: fmt.Sprintf("parent work item %s was not found", newParentID)})
		} else {
			if err := requireWorkItemRelationMutableTx(ctx, tx, newParentID, parentKind); err != nil {
				if errors.Is(err, ErrWorkItemParentClosed) {
					issues = append(issues, WorkItemMoveIssue{ItemID: newParentID, Code: "parent_immutable", Message: fmt.Sprintf("parent work item %s is archived", newParentID)})
				} else {
					return nil, err
				}
			}
			if parentKind != WorkItemEpic && parentKind != WorkItemFeature {
				issues = append(issues, WorkItemMoveIssue{ItemID: newParentID, Code: "parent_not_container", Message: fmt.Sprintf("%s work item %s cannot contain children", parentKind, newParentID)})
			} else {
				open, err := workItemContainerOpenTx(ctx, tx, newParentID, parentKind)
				if err != nil {
					return nil, err
				}
				if !open {
					issues = append(issues, WorkItemMoveIssue{ItemID: newParentID, Code: "parent_closed", Message: fmt.Sprintf("parent work item %s is closed", newParentID)})
				}
			}
		}
	}

	for _, id := range normalized {
		if id == "" || !selected[id] || kinds[id] == "" {
			continue
		}
		if id == newParentID {
			issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "self_parent", Message: fmt.Sprintf("work item %s cannot be its own parent", id)})
		} else if selected[newParentID] {
			issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "parent_selected", Message: fmt.Sprintf("parent work item %s is also selected", newParentID)})
		}
	}
	for i, ancestorID := range normalized {
		if ancestorID == "" || kinds[ancestorID] == "" {
			continue
		}
		for _, descendantID := range normalized[i+1:] {
			if descendantID == "" || kinds[descendantID] == "" {
				continue
			}
			if hierarchyPathContains(parentByChild, descendantID, ancestorID) || hierarchyPathContains(parentByChild, ancestorID, descendantID) {
				issues = append(issues, WorkItemMoveIssue{ItemID: descendantID, Code: "selected_ancestor_descendant", Message: fmt.Sprintf("work items %s and %s are an ancestor and descendant and cannot be moved together", ancestorID, descendantID)})
			}
		}
	}

	prospectiveParents := make(map[string]string, len(parentByChild)+len(selected))
	for childID, parentID := range parentByChild {
		prospectiveParents[childID] = parentID
	}
	for id := range selected {
		prospectiveParents[id] = newParentID
	}
	prospectiveDependency := cloneMoveAdjacency(dependency)
	for id := range selected {
		if oldParentID := parentByChild[id]; oldParentID != "" {
			prospectiveDependency[id] = removeMoveEdge(prospectiveDependency[id], oldParentID)
		}
		if newParentID != "" {
			prospectiveDependency[id] = append(prospectiveDependency[id], newParentID)
		}
	}
	if newParentID != "" && !selected[newParentID] && kinds[newParentID] != "" {
		for _, id := range normalized {
			if id == "" || kinds[id] == "" {
				continue
			}
			if hierarchyPathContains(parentByChild, newParentID, id) {
				issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "parent_descendant", Message: fmt.Sprintf("parent work item %s is a descendant of %s", newParentID, id)})
				continue
			}
			if movePathExists(prospectiveDependency, newParentID, id) {
				issues = append(issues, WorkItemMoveIssue{ItemID: id, Code: "dependency_cycle", Message: fmt.Sprintf("moving work item %s under %s would create a dependency cycle", id, newParentID)})
			}
		}
	}

	validatedSubtree := make(map[string]bool)
	for _, rootID := range normalized {
		if rootID == "" || kinds[rootID] == "" {
			continue
		}
		for _, participantID := range moveSubtreeIDs(children, rootID) {
			if validatedSubtree[participantID] {
				continue
			}
			validatedSubtree[participantID] = true
			switch kinds[participantID] {
			case WorkItemFeature:
				var stored sql.NullString
				if err := tx.QueryRowContext(ctx, `SELECT integration_feature_id FROM features WHERE id = ?`, participantID).Scan(&stored); err != nil {
					return nil, err
				}
				target := nearestFeatureInMoveGraph(prospectiveParents, kinds, prospectiveParents[participantID])
				if stored.String != target {
					issues = append(issues, WorkItemMoveIssue{ItemID: participantID, Code: "feature_integration_target_immutable", Message: fmt.Sprintf("feature %s has an immutable integration target", participantID)})
				}
			case WorkItemTask:
				target := nearestFeatureInMoveGraph(prospectiveParents, kinds, prospectiveParents[participantID])
				if err := guardTaskFeatureCacheChangeTx(ctx, tx, participantID, target); err != nil {
					if errors.Is(err, ErrWorkItemMoveConflict) {
						issues = append(issues, WorkItemMoveIssue{ItemID: participantID, Code: "task_feature_base_conflict", Message: fmt.Sprintf("task %s has workflow or Git state that forbids changing its feature base", participantID)})
						continue
					}
					return nil, err
				}
			}
		}
	}

	if len(issues) != 0 {
		return nil, &WorkItemMoveValidationError{Issues: issues}
	}

	now := s.now().UTC()
	changed := make([]string, 0, len(normalized))
	reconcileIDs := make([]string, 0, len(normalized)*2+1)
	for _, id := range normalized {
		if oldParents[id] == newParentID {
			continue
		}
		changed = append(changed, id)
		reconcileIDs = append(reconcileIDs, id, oldParents[id])
		if _, err := tx.ExecContext(ctx, `DELETE FROM work_item_relations WHERE target_item_id = ? AND kind = 'parent_of'`, id); err != nil {
			return nil, err
		}
	}
	for _, id := range changed {
		if newParentID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO work_item_relations (source_item_id, target_item_id, kind, created_by, created_at)
VALUES (?, ?, 'parent_of', ?, ?)`, newParentID, id, string(actor), formatTime(now)); err != nil {
			return nil, fmt.Errorf("link bulk work item parent: %w", err)
		}
	}
	for _, id := range changed {
		if err := s.syncSubtreeFeatureCachesTx(ctx, tx, id, now); err != nil {
			return nil, err
		}
	}
	if newParentID != "" {
		reconcileIDs = append(reconcileIDs, newParentID)
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, reconcileIDs, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetMany(ctx, normalized)
}

func loadWorkItemMoveGraph(ctx context.Context, q workItemRelationQuerier) (map[string]WorkItemKind, map[string]string, map[string][]string, map[string][]string, error) {
	kinds := make(map[string]WorkItemKind)
	rows, err := q.QueryContext(ctx, `SELECT id, kind FROM work_items`)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		kinds[id] = WorkItemKind(kind)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, nil, err
	}

	parents := make(map[string]string)
	children := make(map[string][]string)
	dependency := make(map[string][]string)
	rows, err = q.QueryContext(ctx, `SELECT source_item_id, target_item_id, kind FROM work_item_relations WHERE kind IN ('parent_of', 'blocks')`)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sourceID, targetID, kind string
		if err := rows.Scan(&sourceID, &targetID, &kind); err != nil {
			return nil, nil, nil, nil, err
		}
		if RelationKind(kind) == RelationParentOf {
			parents[targetID] = sourceID
			children[sourceID] = append(children[sourceID], targetID)
			dependency[targetID] = append(dependency[targetID], sourceID)
		} else {
			dependency[sourceID] = append(dependency[sourceID], targetID)
		}
	}
	return kinds, parents, children, dependency, rows.Err()
}

func hierarchyPathContains(parents map[string]string, startID, wantID string) bool {
	seen := make(map[string]bool)
	for id := startID; id != "" && !seen[id]; id = parents[id] {
		if id == wantID {
			return true
		}
		seen[id] = true
	}
	return false
}

func cloneMoveAdjacency(source map[string][]string) map[string][]string {
	clone := make(map[string][]string, len(source))
	for id, edges := range source {
		clone[id] = append([]string(nil), edges...)
	}
	return clone
}

func removeMoveEdge(edges []string, target string) []string {
	result := edges[:0]
	for _, edge := range edges {
		if edge != target {
			result = append(result, edge)
		}
	}
	return result
}

func movePathExists(adjacency map[string][]string, startID, goalID string) bool {
	stack := []string{startID}
	seen := make(map[string]bool)
	for len(stack) != 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == goalID {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, adjacency[id]...)
	}
	return false
}

func moveSubtreeIDs(children map[string][]string, rootID string) []string {
	result := make([]string, 0, 1)
	stack := []string{rootID}
	seen := make(map[string]bool)
	for len(stack) != 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
		stack = append(stack, children[id]...)
	}
	return result
}

func nearestFeatureInMoveGraph(parents map[string]string, kinds map[string]WorkItemKind, parentID string) string {
	seen := make(map[string]bool)
	for id := parentID; id != "" && !seen[id]; id = parents[id] {
		if kinds[id] == WorkItemFeature {
			return id
		}
		seen[id] = true
	}
	return ""
}
