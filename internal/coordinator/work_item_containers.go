package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type ContainerTaskStartStatus string

const (
	ContainerTaskScheduled        ContainerTaskStartStatus = "scheduled"
	ContainerTaskAlreadyScheduled ContainerTaskStartStatus = "already_scheduled"
	ContainerTaskDone             ContainerTaskStartStatus = "done"
	ContainerTaskError            ContainerTaskStartStatus = "error"
)

type ContainerTaskStartResult struct {
	TaskID string                   `json:"task_id"`
	Status ContainerTaskStartStatus `json:"status"`
	RunID  string                   `json:"run_id,omitempty"`
	Error  string                   `json:"error,omitempty"`
}

type ContainerStartResult struct {
	Container WorkItemSummary            `json:"container"`
	Tasks     []ContainerTaskStartResult `json:"tasks"`
}

type ContainerService struct {
	db    *sql.DB
	items *WorkItemService
	runs  *WorkflowRunService
}

func NewContainerService(database *sql.DB, items *WorkItemService, runs *WorkflowRunService) *ContainerService {
	if items == nil {
		items = NewWorkItemService(database, "")
	}
	return &ContainerService{db: database, items: items, runs: runs}
}

// Start schedules every unscheduled descendant task. Blocked tasks are still
// scheduled: their workflow run waits at its first node until the inherited
// blocker gate clears. Per-task results make retries safe and transparent.
func (s *ContainerService) Start(ctx context.Context, containerID string, actor Actor) (ContainerStartResult, error) {
	container, err := s.items.Get(ctx, containerID)
	if err != nil {
		return ContainerStartResult{}, err
	}
	if !container.Capabilities.CanStart {
		return ContainerStartResult{}, fmt.Errorf("%w: %s %s is not a container", ErrWorkItemNotSchedulable, container.Kind, container.ID)
	}
	if container.State.Terminal {
		return ContainerStartResult{}, ErrWorkItemParentClosed
	}
	if s.runs == nil {
		return ContainerStartResult{}, errors.New("workflow run service is not configured")
	}

	taskIDs, err := s.descendantTaskIDs(ctx, container)
	if err != nil {
		return ContainerStartResult{}, err
	}
	result := ContainerStartResult{Container: container, Tasks: make([]ContainerTaskStartResult, 0, len(taskIDs))}
	for _, taskID := range taskIDs {
		task, err := s.runs.tasks.GetTask(ctx, taskID)
		if err != nil {
			result.Tasks = append(result.Tasks, ContainerTaskStartResult{TaskID: taskID, Status: ContainerTaskError, Error: err.Error()})
			continue
		}
		if task.State != nil {
			status := ContainerTaskAlreadyScheduled
			if *task.State == LifecycleDone {
				status = ContainerTaskDone
			}
			result.Tasks = append(result.Tasks, ContainerTaskStartResult{TaskID: taskID, Status: status})
			continue
		}
		run, err := s.runs.ScheduleAs(ctx, taskID, actor)
		if err != nil {
			result.Tasks = append(result.Tasks, ContainerTaskStartResult{TaskID: taskID, Status: ContainerTaskError, Error: err.Error()})
			continue
		}
		result.Tasks = append(result.Tasks, ContainerTaskStartResult{TaskID: taskID, Status: ContainerTaskScheduled, RunID: run.ID})
	}
	return result, nil
}

func (s *ContainerService) descendantTaskIDs(ctx context.Context, container WorkItemSummary) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE descendants(id) AS (
	VALUES (?)
	UNION
	SELECT r.target_item_id
	FROM descendants
	JOIN work_item_relations r ON r.source_item_id = descendants.id AND r.kind = 'parent_of'
), feature_descendants(id) AS (
	SELECT d.id FROM descendants d JOIN work_items wi ON wi.id = d.id AND wi.kind = 'feature'
)
SELECT id FROM (
	SELECT t.id AS id FROM tasks t JOIN descendants d ON d.id = t.id
	UNION
	SELECT t.id AS id FROM tasks t JOIN feature_descendants f ON f.id = t.feature_id
)
ORDER BY id`, container.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Move atomically replaces an item's organizational parent. Feature
// integration targets are immutable; moving a feature across target scope is
// rejected. A task's feature_id is maintained only as the validated cache of
// its nearest feature ancestor.
func (s *WorkItemService) Move(ctx context.Context, itemID, newParentID string, actor Actor) error {
	itemID = strings.TrimSpace(itemID)
	newParentID = strings.TrimSpace(newParentID)
	if itemID == "" {
		return errors.New("work item id is required")
	}
	if itemID == newParentID {
		return ErrWorkItemCycle
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kind, err := workItemKindTx(ctx, tx, itemID)
	if err != nil {
		return err
	}
	if err := requireWorkItemRelationMutableTx(ctx, tx, itemID, kind); err != nil {
		return err
	}
	var oldParentID string
	err = tx.QueryRowContext(ctx, `
SELECT source_item_id FROM work_item_relations
WHERE target_item_id = ? AND kind = 'parent_of'`, itemID).Scan(&oldParentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if oldParentID == newParentID {
		return nil
	}
	if oldParentID != "" {
		oldParentKind, err := workItemKindTx(ctx, tx, oldParentID)
		if err != nil {
			return err
		}
		if err := requireWorkItemRelationMutableTx(ctx, tx, oldParentID, oldParentKind); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
DELETE FROM work_item_relations WHERE target_item_id = ? AND kind = 'parent_of'`, itemID); err != nil {
		return err
	}
	if newParentID != "" {
		if err := s.linkTx(ctx, tx, newParentID, itemID, RelationParentOf, actor); err != nil {
			return err
		}
	}
	if err := s.syncSubtreeFeatureCachesTx(ctx, tx, itemID, s.now().UTC()); err != nil {
		return err
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{itemID, oldParentID, newParentID}, s.now().UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// syncSubtreeFeatureCachesTx validates an immutable feature integration target
// and refreshes the derived feature_id cache for every task in the moved
// organizational subtree. Any base-dependent task makes the entire move fail.
func (s *WorkItemService) syncSubtreeFeatureCachesTx(ctx context.Context, q workItemRelationQuerier, itemID string, now time.Time) error {
	kind, err := workItemKindTx(ctx, q, itemID)
	if err != nil {
		return err
	}
	if kind == WorkItemFeature {
		parentID, err := workItemParentIDTx(ctx, q, itemID)
		if err != nil {
			return err
		}
		targetFeatureID, err := nearestFeatureFromParentTx(ctx, q, parentID)
		if err != nil {
			return err
		}
		var stored sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT integration_feature_id FROM features WHERE id = ?`, itemID).Scan(&stored); err != nil {
			return err
		}
		if stored.String != targetFeatureID {
			return fmt.Errorf("%w: feature integration target is immutable", ErrWorkItemMoveConflict)
		}
	}

	rows, err := q.QueryContext(ctx, `
WITH RECURSIVE descendants(id) AS (
	VALUES (?)
	UNION ALL
	SELECT r.target_item_id FROM descendants
	JOIN work_item_relations r ON r.source_item_id = descendants.id AND r.kind = 'parent_of'
)
SELECT d.id FROM descendants d JOIN work_items wi ON wi.id = d.id AND wi.kind = 'task'
ORDER BY d.id`, itemID)
	if err != nil {
		return err
	}
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		parentID, err := workItemParentIDTx(ctx, q, taskID)
		if err != nil {
			return err
		}
		featureID, err := nearestFeatureFromParentTx(ctx, q, parentID)
		if err != nil {
			return err
		}
		if err := guardTaskFeatureCacheChangeTx(ctx, q, taskID, featureID); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `UPDATE tasks SET feature_id = ?, updated_at = ? WHERE id = ?`,
			sqlitex.NullableNonEmptyString(featureID), formatTime(now), taskID); err != nil {
			return err
		}
	}
	return nil
}

func workItemParentIDTx(ctx context.Context, q workItemRelationQuerier, itemID string) (string, error) {
	var parentID string
	err := q.QueryRowContext(ctx, `
SELECT source_item_id FROM work_item_relations
WHERE target_item_id = ? AND kind = 'parent_of'`, itemID).Scan(&parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return parentID, err
}

func nearestFeatureFromParentTx(ctx context.Context, q workItemRelationQuerier, parentID string) (string, error) {
	if strings.TrimSpace(parentID) == "" {
		return "", nil
	}
	var featureID string
	err := q.QueryRowContext(ctx, `
WITH RECURSIVE ancestors(id, depth) AS (
	VALUES (?, 0)
	UNION ALL
	SELECT r.source_item_id, ancestors.depth + 1
	FROM ancestors
	JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
)
SELECT ancestors.id FROM ancestors
JOIN work_items wi ON wi.id = ancestors.id AND wi.kind = 'feature'
ORDER BY ancestors.depth LIMIT 1`, parentID).Scan(&featureID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return featureID, err
}

func guardTaskFeatureCacheChangeTx(ctx context.Context, q workItemRelationQuerier, taskID, featureID string) error {
	var current sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT feature_id FROM tasks WHERE id = ?`, taskID).Scan(&current); err != nil {
		return err
	}
	if current.String == featureID {
		return nil
	}
	var active int
	if err := q.QueryRowContext(ctx, `
SELECT EXISTS(
	SELECT 1 FROM workflow_runs WHERE task_id = ?
	UNION ALL SELECT 1 FROM changes WHERE task_id = ?
	UNION ALL SELECT 1 FROM merge_intents WHERE task_id = ?
)`, taskID, taskID, taskID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return fmt.Errorf("%w: task has base-dependent workflow or Git state", ErrWorkItemMoveConflict)
	}
	return nil
}
