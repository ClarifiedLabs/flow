package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// ResetTask discards every artifact of the task's authoring attempts so the
// next author job starts from the base branch: live author jobs are canceled,
// active session tokens revoked, the task's branches removed from the
// exchange, and the change projections (with their sessions, review threads,
// handoff snapshots, and merge intents, via FK cascade) plus the task's check
// rows deleted. The task's schedule and triage state are left untouched;
// re-enqueueing a fresh author job is the lifecycle engine's follow-up.
func (s *SessionService) ResetTask(ctx context.Context, taskID string) (Task, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Task{}, errors.New("task id is required")
	}
	task, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	if task.ScheduleState == ScheduleClosed {
		return Task{}, errors.New("closed tasks cannot be reset")
	}
	if task.TriageState != TriageAccepted {
		return Task{}, errors.New("reset requires an accepted task")
	}

	changes, err := s.changesForTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	for _, change := range changes {
		if change.MergedAt != nil {
			return Task{}, errors.New("tasks with a merged change cannot be reset")
		}
	}
	if len(changes) > 0 && strings.TrimSpace(s.project.ExchangePath) == "" {
		return Task{}, errors.New("project exchange path is required to reset task branches")
	}

	if _, err := s.workers.CancelLiveJobsForTask(ctx, taskID, flowworker.RoleAuthor); err != nil {
		return Task{}, fmt.Errorf("cancel live author jobs: %w", err)
	}

	// Token hashes are collected before the change delete cascades the session
	// rows away; the tokens themselves live in the global credential store.
	tokenHashes, err := s.activeSessionTokenHashesForTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}

	for _, change := range changes {
		if err := flowgit.DeleteBranch(ctx, flowgit.DeleteBranchInput{
			ExchangeRepoPath: s.project.ExchangePath,
			Branch:           change.Branch,
		}); err != nil {
			return Task{}, err
		}
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM changes WHERE task_id = ?`, taskID); err != nil {
		return Task{}, fmt.Errorf("delete change projections: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE task_id = ?`, taskID); err != nil {
		return Task{}, fmt.Errorf("delete task checks: %w", err)
	}
	// Discard the flow cursor and phase handoffs too: the next author job
	// re-freezes a fresh snapshot from the live flow and starts at phase 0.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM task_flow_cursor WHERE task_id = ?`, taskID); err != nil {
		return Task{}, fmt.Errorf("delete task flow cursor: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM task_phase_handoffs WHERE task_id = ?`, taskID); err != nil {
		return Task{}, fmt.Errorf("delete task phase handoffs: %w", err)
	}

	var revokeErr error
	for _, tokenHash := range tokenHashes {
		revokeErr = errors.Join(revokeErr, s.revokeSessionTokenHash(ctx, tokenHash))
	}
	if revokeErr != nil {
		return Task{}, fmt.Errorf("revoke session tokens: %w", revokeErr)
	}

	return s.tasks.GetTask(ctx, taskID)
}

func (s *SessionService) changesForTask(ctx context.Context, taskID string) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, task_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at
FROM changes
WHERE task_id = ?
ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("select changes for task: %w", err)
	}
	defer rows.Close()

	var changes []Change
	for rows.Next() {
		change, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate changes for task: %w", err)
	}

	return changes, nil
}

func (s *SessionService) activeSessionTokenHashesForTask(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT token_hash
FROM sessions
WHERE task_id = ?
	AND runtime_state IN (?, ?, ?)`,
		taskID,
		string(SessionStarting),
		string(SessionWorking),
		string(SessionWaiting),
	)
	if err != nil {
		return nil, fmt.Errorf("select session tokens for task: %w", err)
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scan session token hash: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session token hashes: %w", err)
	}

	return hashes, nil
}
