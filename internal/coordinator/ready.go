package coordinator

import (
	"context"
	"sort"
)

// ReadyTasks lists unscheduled tasks with no unresolved effective blockers —
// the same rule the schedule-time dependency gate (EnsureCurrentNode) applies
// before starting workflow nodes: blockers are work items of any kind linked
// by a blocks relation to the task or one of its ancestors, and a blocker is
// resolved once it reaches its terminal state. The tag and search fields of
// the filter apply; any lifecycle filter is replaced with unscheduled.
// Results order by priority (desc), then creation time, then id, so the first
// row is the natural "next" task.
func (s *TaskService) ReadyTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	filter.LifecycleStates = []string{"unscheduled"}
	candidates, err := s.ListTasks(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return []Task{}, nil
	}

	ids := make([]string, 0, len(candidates))
	for _, task := range candidates {
		ids = append(ids, task.ID)
	}
	blockedCounts, err := s.effectiveUnresolvedBlockerCounts(ctx, ids)
	if err != nil {
		return nil, err
	}

	ready := make([]Task, 0, len(candidates))
	for _, task := range candidates {
		if blockedCounts[task.ID] == 0 {
			ready = append(ready, task)
		}
	}
	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority > ready[j].Priority
		}
		if !ready[i].CreatedAt.Equal(ready[j].CreatedAt) {
			return ready[i].CreatedAt.Before(ready[j].CreatedAt)
		}
		return ready[i].ID < ready[j].ID
	})
	return ready, nil
}

// TaskBlocked reports whether the task is currently blocked. An unscheduled
// task is blocked when it has unresolved effective blockers; an in-progress
// task is blocked when it has open workflow waits or a system convergence
// hold (the board's needs-attention rule); scheduled and done tasks are never
// reported blocked.
func (s *TaskService) TaskBlocked(ctx context.Context, taskID string) (bool, error) {
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	if task.State == nil {
		blockers, err := effectiveUnresolvedBlockerCountTx(ctx, s.db, task.ID)
		if err != nil {
			return false, err
		}
		return blockers > 0, nil
	}
	if *task.State != LifecycleInProgress {
		return false, nil
	}
	openWaits, _, heldBySystem, err := s.openWaitAndHoldCounts(ctx, task.ID)
	if err != nil {
		return false, err
	}
	return openWaits > 0 || heldBySystem > 0, nil
}

// effectiveUnresolvedBlockerCounts computes, in one query, how many blockers
// keep each candidate unready. It mirrors effectiveUnresolvedBlockerCountTx
// and workItemTerminalTx terminal semantics across all work-item kinds, batched
// per root task id.
func (s *TaskService) effectiveUnresolvedBlockerCounts(ctx context.Context, taskIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(taskIDs))
	if len(taskIDs) == 0 {
		return counts, nil
	}
	args := make([]any, 0, len(taskIDs))
	for _, id := range taskIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE ancestors(root, id) AS (
	SELECT wi.id, wi.id FROM work_items wi WHERE `+inPredicate("wi.id", len(taskIDs))+`
	UNION ALL
	SELECT a.root, r.source_item_id FROM ancestors a
	JOIN work_item_relations r ON r.target_item_id = a.id AND r.kind = 'parent_of'
), blockers(root, id) AS (
	SELECT DISTINCT a.root, r.source_item_id FROM ancestors a
	JOIN work_item_relations r ON r.target_item_id = a.id AND r.kind = 'blocks'
)
SELECT b.root, COUNT(*) FROM blockers b
JOIN work_items bwi ON bwi.id = b.id
LEFT JOIN tasks bt ON bt.id = b.id AND bwi.kind = 'task'
LEFT JOIN epics be ON be.id = b.id AND bwi.kind = 'epic'
LEFT JOIN features bf ON bf.id = b.id AND bwi.kind = 'feature'
WHERE CASE bwi.kind
	WHEN 'task' THEN COALESCE(bt.lifecycle_state = 'done', 0)
	WHEN 'epic' THEN COALESCE(be.status IN ('completed', 'archived'), 0)
	WHEN 'feature' THEN COALESCE(bf.status IN ('landed', 'archived'), 0)
END = 0
GROUP BY b.root`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		var count int
		if err := rows.Scan(&root, &count); err != nil {
			return nil, err
		}
		counts[root] = count
	}
	return counts, rows.Err()
}

// openWaitAndHoldCounts returns the live in-progress signals behind the
// board's blocked/held derivation: open workflow waits, active operator
// holds, and active holds owned by the system (convergence scope decisions).
func (s *TaskService) openWaitAndHoldCounts(ctx context.Context, taskID string) (openWaits, held, heldBySystem int, err error) {
	err = s.db.QueryRowContext(ctx, `
SELECT
	(SELECT COUNT(*) FROM workflow_waits w
		JOIN workflow_runs r ON r.id = w.workflow_run_id
		WHERE r.task_id = ? AND w.state = 'open'),
	(SELECT COUNT(*) FROM workflow_runs r
		WHERE r.task_id = ? AND r.held_at IS NOT NULL
			AND r.state IN ('scheduled', 'running', 'waiting')),
	(SELECT COUNT(*) FROM workflow_runs r
		WHERE r.task_id = ? AND r.held_at IS NOT NULL
			AND r.held_by = ?
			AND r.state IN ('scheduled', 'running', 'waiting'))`,
		taskID, taskID, taskID, string(ActorSystem)).Scan(&openWaits, &held, &heldBySystem)
	if err != nil {
		return 0, 0, 0, err
	}
	return openWaits, held, heldBySystem, nil
}
