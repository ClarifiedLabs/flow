package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	ReviewFollowUpCreateTask      = "create_task"
	ReviewFollowUpUseExistingTask = "use_existing_task"

	reviewFollowUpTaskTitleMaxBytes = 256
	reviewFollowUpTaskBodyMaxBytes  = 4096
)

type ReviewFollowUpFinding struct {
	SHA                string
	File               string
	Line               int
	Body               string
	Severity           string
	IntroducedByChange bool
	Requirement        string
	DuplicateOf        string
}

type ReviewFollowUpTaskAction struct {
	Action string
	Title  string
	Body   string
	TaskID string
}

type ApplyReviewFollowUpInput struct {
	SourceTaskID   string
	SourceChangeID string
	CheckName      string
	Finding        ReviewFollowUpFinding
	TaskAction     ReviewFollowUpTaskAction
}

type ApplyReviewFollowUpResult struct {
	Task        Task
	Disposition string
}

func (s *TaskService) ApplyReviewFollowUp(ctx context.Context, input ApplyReviewFollowUpInput) (ApplyReviewFollowUpResult, error) {
	normalized, findingHash, requestHash, err := normalizeReviewFollowUpInput(input)
	if err != nil {
		return ApplyReviewFollowUpResult{}, err
	}
	if result, found, err := s.reviewFollowUpResult(ctx, normalized.SourceTaskID, normalized.CheckName, findingHash, requestHash); err != nil {
		return ApplyReviewFollowUpResult{}, err
	} else if found {
		return result, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyReviewFollowUpResult{}, fmt.Errorf("begin review follow-up: %w", err)
	}
	defer tx.Rollback()

	if _, err := taskInTx(ctx, tx, normalized.SourceTaskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ApplyReviewFollowUpResult{}, errors.New("review follow-up source task not found")
		}
		return ApplyReviewFollowUpResult{}, err
	}

	var target Task
	switch normalized.TaskAction.Action {
	case ReviewFollowUpCreateTask:
		target, err = s.createReviewFollowUpTaskInTx(ctx, tx, normalized)
	case ReviewFollowUpUseExistingTask:
		target, err = taskInTx(ctx, tx, normalized.TaskAction.TaskID)
		if err == nil && target.State != nil && *target.State == LifecycleDone {
			err = errors.New("review follow-up task must be open")
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ApplyReviewFollowUpResult{}, errors.New("review follow-up task not found")
	}
	if err != nil {
		return ApplyReviewFollowUpResult{}, err
	}
	if target.ID == normalized.SourceTaskID {
		return ApplyReviewFollowUpResult{}, errors.New("review follow-up cannot use the reviewed task")
	}

	nowText := formatTime(s.now().UTC())
	relatedSourceID, relatedTargetID := normalized.SourceTaskID, target.ID
	if relatedTargetID < relatedSourceID {
		relatedSourceID, relatedTargetID = relatedTargetID, relatedSourceID
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO work_item_relations (
	source_item_id, target_item_id, kind, created_by, created_at
) VALUES (?, ?, ?, ?, ?)`,
		relatedSourceID,
		relatedTargetID,
		string(RelationRelatedTo),
		string(ActorSystem),
		nowText,
	); err != nil {
		return ApplyReviewFollowUpResult{}, fmt.Errorf("relate review follow-up task: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO review_follow_up_actions (
	source_task_id, check_name, finding_hash, request_hash, action, task_id, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		normalized.SourceTaskID,
		normalized.CheckName,
		findingHash,
		requestHash,
		normalized.TaskAction.Action,
		target.ID,
		nowText,
	); err != nil {
		_ = tx.Rollback()
		if replay, found, replayErr := s.reviewFollowUpResult(
			ctx,
			normalized.SourceTaskID,
			normalized.CheckName,
			findingHash,
			requestHash,
		); replayErr != nil {
			return ApplyReviewFollowUpResult{}, errors.Join(err, replayErr)
		} else if found {
			return replay, nil
		}
		return ApplyReviewFollowUpResult{}, fmt.Errorf("record review follow-up: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ApplyReviewFollowUpResult{}, fmt.Errorf("commit review follow-up: %w", err)
	}

	persisted, err := s.GetTask(ctx, target.ID)
	if err != nil {
		return ApplyReviewFollowUpResult{}, err
	}
	return ApplyReviewFollowUpResult{
		Task:        persisted,
		Disposition: reviewFollowUpDisposition(normalized.TaskAction.Action),
	}, nil
}

func (s *TaskService) createReviewFollowUpTaskInTx(ctx context.Context, tx *sql.Tx, input ApplyReviewFollowUpInput) (Task, error) {
	id, err := s.allocateTaskID(ctx, tx)
	if err != nil {
		return Task{}, err
	}
	nowText := formatTime(s.now().UTC())
	if err := insertWorkItem(ctx, tx, id, WorkItemTask, nowText); err != nil {
		return Task{}, err
	}
	// Follow-up tasks inherit the source task's feature so fix work stays on
	// the same feature branch.
	var sourceFeatureID any
	if err := tx.QueryRowContext(ctx, `SELECT feature_id FROM tasks WHERE id = ?`, input.SourceTaskID).Scan(&sourceFeatureID); err != nil {
		return Task{}, fmt.Errorf("load source task feature: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (
	id, title, body, priority, feature_id, created_by, source_task_id, source_change_id,
	created_at, updated_at
) VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		id,
		input.TaskAction.Title,
		input.TaskAction.Body,
		sourceFeatureID,
		string(ActorSystem),
		input.SourceTaskID,
		input.SourceChangeID,
		nowText,
		nowText,
	); err != nil {
		return Task{}, fmt.Errorf("create review follow-up task: %w", err)
	}
	return taskInTx(ctx, tx, id)
}

func (s *TaskService) reviewFollowUpResult(
	ctx context.Context,
	sourceTaskID string,
	checkName string,
	findingHash string,
	requestHash string,
) (ApplyReviewFollowUpResult, bool, error) {
	var storedRequestHash, action, taskID string
	err := s.db.QueryRowContext(ctx, `
SELECT request_hash, action, task_id
FROM review_follow_up_actions
WHERE source_task_id = ? AND check_name = ? AND finding_hash = ?`,
		sourceTaskID,
		checkName,
		findingHash,
	).Scan(&storedRequestHash, &action, &taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return ApplyReviewFollowUpResult{}, false, nil
	}
	if err != nil {
		return ApplyReviewFollowUpResult{}, false, fmt.Errorf("load review follow-up action: %w", err)
	}
	if storedRequestHash != requestHash {
		return ApplyReviewFollowUpResult{}, false, errors.New("review follow-up replay conflicts with the recorded action")
	}
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return ApplyReviewFollowUpResult{}, false, err
	}
	return ApplyReviewFollowUpResult{
		Task:        task,
		Disposition: reviewFollowUpDisposition(action),
	}, true, nil
}

func normalizeReviewFollowUpInput(input ApplyReviewFollowUpInput) (ApplyReviewFollowUpInput, string, string, error) {
	input.SourceTaskID = strings.TrimSpace(input.SourceTaskID)
	input.SourceChangeID = strings.TrimSpace(input.SourceChangeID)
	input.CheckName = strings.TrimSpace(input.CheckName)
	input.Finding.SHA = strings.TrimSpace(input.Finding.SHA)
	input.Finding.File = strings.TrimSpace(input.Finding.File)
	input.Finding.Body = strings.TrimSpace(input.Finding.Body)
	input.Finding.Severity = strings.ToLower(strings.TrimSpace(input.Finding.Severity))
	input.Finding.Requirement = strings.TrimSpace(input.Finding.Requirement)
	input.Finding.DuplicateOf = strings.TrimSpace(input.Finding.DuplicateOf)
	input.TaskAction.Action = strings.TrimSpace(input.TaskAction.Action)
	input.TaskAction.Title = strings.TrimSpace(input.TaskAction.Title)
	input.TaskAction.Body = strings.TrimSpace(input.TaskAction.Body)
	input.TaskAction.TaskID = strings.TrimSpace(input.TaskAction.TaskID)

	switch {
	case input.SourceTaskID == "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up source task is required")
	case input.SourceChangeID == "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up source change is required")
	case input.CheckName == "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up check name is required")
	case input.Finding.SHA == "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up finding sha is required")
	case input.Finding.File == "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up finding file is required")
	case input.Finding.Line <= 0:
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up finding line must be positive")
	case input.Finding.Body == "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up finding body is required")
	case input.Finding.Requirement == "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review follow-up finding requirement is required")
	case input.Finding.DuplicateOf != "":
		return ApplyReviewFollowUpInput{}, "", "", errors.New("review-thread duplicate cannot create a review follow-up task")
	}
	switch input.Finding.Severity {
	case "critical", "high", "medium", "low":
	default:
		return ApplyReviewFollowUpInput{}, "", "", fmt.Errorf("invalid review follow-up severity %q", input.Finding.Severity)
	}
	if input.Finding.IntroducedByChange &&
		(input.Finding.Severity == "critical" || input.Finding.Severity == "high") {
		return ApplyReviewFollowUpInput{}, "", "", errors.New("blocking finding cannot create a review follow-up task")
	}

	switch input.TaskAction.Action {
	case ReviewFollowUpCreateTask:
		if input.TaskAction.Title == "" || input.TaskAction.Body == "" {
			return ApplyReviewFollowUpInput{}, "", "", errors.New("create_task requires title and body")
		}
		if input.TaskAction.TaskID != "" {
			return ApplyReviewFollowUpInput{}, "", "", errors.New("create_task must not specify task_id")
		}
		if len(input.TaskAction.Title) > reviewFollowUpTaskTitleMaxBytes {
			return ApplyReviewFollowUpInput{}, "", "", fmt.Errorf("review follow-up task title exceeds %d bytes", reviewFollowUpTaskTitleMaxBytes)
		}
		if len(input.TaskAction.Body) > reviewFollowUpTaskBodyMaxBytes {
			return ApplyReviewFollowUpInput{}, "", "", fmt.Errorf("review follow-up task body exceeds %d bytes", reviewFollowUpTaskBodyMaxBytes)
		}
	case ReviewFollowUpUseExistingTask:
		if input.TaskAction.TaskID == "" {
			return ApplyReviewFollowUpInput{}, "", "", errors.New("use_existing_task requires task_id")
		}
		if input.TaskAction.Title != "" || input.TaskAction.Body != "" {
			return ApplyReviewFollowUpInput{}, "", "", errors.New("use_existing_task must not specify title or body")
		}
	default:
		return ApplyReviewFollowUpInput{}, "", "", fmt.Errorf("invalid review follow-up action %q", input.TaskAction.Action)
	}

	findingHash, err := reviewFollowUpHash(struct {
		SHA                string
		File               string
		Line               int
		Body               string
		Severity           string
		IntroducedByChange bool
		Requirement        string
	}{
		SHA:                input.Finding.SHA,
		File:               input.Finding.File,
		Line:               input.Finding.Line,
		Body:               input.Finding.Body,
		Severity:           input.Finding.Severity,
		IntroducedByChange: input.Finding.IntroducedByChange,
		Requirement:        input.Finding.Requirement,
	})
	if err != nil {
		return ApplyReviewFollowUpInput{}, "", "", err
	}
	requestHash, err := reviewFollowUpHash(input.TaskAction)
	if err != nil {
		return ApplyReviewFollowUpInput{}, "", "", err
	}
	return input, findingHash, requestHash, nil
}

func reviewFollowUpHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func reviewFollowUpDisposition(action string) string {
	if action == ReviewFollowUpCreateTask {
		return "created"
	}
	return "existing"
}

func taskInTx(ctx context.Context, tx *sql.Tx, id string) (Task, error) {
	return scanTask(tx.QueryRowContext(ctx, "SELECT"+taskSelectColumns+`
FROM tasks i
WHERE i.id = ?`, strings.TrimSpace(id)))
}
