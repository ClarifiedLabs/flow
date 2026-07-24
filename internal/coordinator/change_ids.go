package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// insertWithTaskChangeID ensures a logical change has a task-derived ID. It
// retries when a concurrent creator takes the same ID sequence and returns the
// winner when another creator inserts the same task/branch or workflow run.
func insertWithTaskChangeID(
	ctx context.Context,
	database *sql.DB,
	taskID string,
	branch string,
	workflowRunID string,
	insert func(string) error,
) (string, bool, error) {
	if id, ok, err := existingLogicalChangeID(ctx, database, taskID, branch, workflowRunID); err != nil {
		return "", false, err
	} else if ok {
		return id, false, nil
	}

	for {
		id, err := nextChangeID(ctx, database, taskID)
		if err != nil {
			return "", false, err
		}
		if err := insert(id); err != nil {
			existingID, ok, lookupErr := existingLogicalChangeID(ctx, database, taskID, branch, workflowRunID)
			if lookupErr != nil {
				return "", false, fmt.Errorf("find logical change after insert conflict: %w", lookupErr)
			}
			if ok {
				return existingID, false, nil
			}
			if isUniqueViolation(err, "changes.id") {
				continue
			}
			return "", false, err
		}
		return id, true, nil
	}
}

func existingLogicalChangeID(ctx context.Context, database *sql.DB, taskID string, branch string, workflowRunID string) (string, bool, error) {
	query := `
SELECT id
FROM changes
WHERE task_id = ? AND branch = ?
LIMIT 1`
	args := []any{taskID, branch}
	if workflowRunID != "" {
		query = `
SELECT id
FROM changes
WHERE workflow_run_id = ? OR (task_id = ? AND branch = ?)
ORDER BY CASE WHEN workflow_run_id = ? THEN 0 ELSE 1 END, created_at DESC, id DESC
LIMIT 1`
		args = []any{workflowRunID, taskID, branch, workflowRunID}
	}

	var id string
	if err := database.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("find existing logical change: %w", err)
	}
	return id, true, nil
}

func nextChangeID(ctx context.Context, database *sql.DB, taskID string) (string, error) {
	baseID := "ch-" + strings.TrimPrefix(strings.TrimSpace(taskID), "t-")
	rows, err := database.QueryContext(ctx, `
SELECT id
FROM changes
WHERE id = ? OR id LIKE ?`, baseID, baseID+"-%")
	if err != nil {
		return "", fmt.Errorf("list task change ids: %w", err)
	}
	defer rows.Close()

	nextSequence := int64(1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("scan task change id: %w", err)
		}
		if id == baseID {
			if nextSequence == 1 {
				nextSequence = 2
			}
			continue
		}
		sequence, err := strconv.ParseInt(strings.TrimPrefix(id, baseID+"-"), 10, 64)
		if err == nil && sequence >= nextSequence {
			nextSequence = sequence + 1
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate task change ids: %w", err)
	}
	if nextSequence == 1 {
		return baseID, nil
	}
	return fmt.Sprintf("%s-%d", baseID, nextSequence), nil
}
