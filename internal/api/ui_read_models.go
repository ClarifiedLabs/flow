package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/terminal"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func (s *projectServer) buildUITaskCards(ctx context.Context, tasks []coordinator.Task) (map[string]uiTaskCard, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	terminalJobs, err := s.uiTerminalJobsByTask(ctx, tasks)
	if err != nil {
		return nil, err
	}

	cards := make(map[string]uiTaskCard, len(tasks))
	for _, task := range tasks {
		card := uiTaskCard{TaskID: task.ID}
		tags, err := s.tasks.TagsForTask(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load tags for %s: %w", task.ID, err)
		}
		card.Tags = tags
		relations, err := s.tasks.RelationsForTask(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load relations for %s: %w", task.ID, err)
		}
		card.Relations = uiRelationSummaryFromRelations(task.ID, relations)
		if s.workflowRuns != nil && task.State != nil && (*task.State == coordinator.LifecycleScheduled || *task.State == coordinator.LifecycleInProgress) {
			run, active, err := s.workflowRuns.ActiveForTask(ctx, task.ID)
			if err != nil {
				return nil, fmt.Errorf("load active workflow run for %s: %w", task.ID, err)
			}
			if active {
				card.CurrentStep = uiWorkflowStepSummaryFromRun(run)
			}
			state, ok, err := s.workflowRuns.CardState(ctx, task.ID)
			if err != nil {
				return nil, fmt.Errorf("load workflow card state for %s: %w", task.ID, err)
			}
			if ok {
				applyUIWorkflowCardState(&card, state)
			}
		}
		if s.sessions != nil {
			active, ok, err := s.sessions.ActiveAuthorSessionForTask(ctx, task.ID)
			if err != nil {
				return nil, fmt.Errorf("load active session for %s: %w", task.ID, err)
			}
			if ok {
				summary, err := s.uiSessionSummaryWithTerminal(ctx, active)
				if err != nil {
					return nil, fmt.Errorf("load terminal availability for %s: %w", task.ID, err)
				}
				card.ActiveSession = summary
				card.TerminalAvailable = summary.TerminalAvailable
				if active.ChangeID != "" {
					change, err := s.sessions.GetChange(ctx, active.ChangeID)
					if err != nil {
						return nil, fmt.Errorf("load active change for %s: %w", task.ID, err)
					}
					card.Change = uiChangeSummaryFromChange(change)
					if err := s.applyHandoffSummary(ctx, &card, change); err != nil {
						return nil, fmt.Errorf("load handoff summary for %s: %w", task.ID, err)
					}
				}
			}
			readyChange, ok, err := s.sessions.ReadyUnmergedChangeForTask(ctx, task.ID)
			if err != nil {
				return nil, fmt.Errorf("load ready change for %s: %w", task.ID, err)
			}
			if ok {
				card.Change = uiChangeSummaryFromChange(readyChange)
				if err := s.applyHandoffSummary(ctx, &card, readyChange); err != nil {
					return nil, fmt.Errorf("load handoff summary for %s: %w", task.ID, err)
				}
				stats, unavailableReason, err := s.changeDiffStats(ctx, readyChange, false)
				if err != nil {
					card.DiffUnavailableReason = err.Error()
				} else if unavailableReason != "" {
					card.DiffUnavailableReason = unavailableReason
				} else {
					card.DiffStats = &uiDiffStatSummary{
						HeadSHA:    readyChange.HeadSHA,
						TotalFiles: len(stats.Files),
						Additions:  stats.Additions,
						Deletions:  stats.Deletions,
					}
				}
			}
		}
		if s.checks != nil {
			checks, err := s.checks.ListChecks(ctx, task.ID)
			if err != nil {
				return nil, fmt.Errorf("load checks for %s: %w", task.ID, err)
			}
			card.RequiredChecks = uiRequiredCheckSummaryFromChecks(checks)
		}
		if s.status != nil {
			statusLog, err := s.status.ListForTask(ctx, task.ID, 1)
			if err != nil {
				return nil, fmt.Errorf("load latest status for %s: %w", task.ID, err)
			}
			if len(statusLog) > 0 {
				card.LatestStatus = &statusLog[0]
			}
		}
		blockers, err := s.tasks.UnresolvedBlockers(ctx, task.ID)
		if err != nil {
			return nil, fmt.Errorf("load blockers for %s: %w", task.ID, err)
		}
		card.Blockers = uiBlockerSummaryFromTasks(blockers)
		if jobID, ok := terminalJobs[task.ID]; ok {
			card.TerminalJobID = jobID
			card.TerminalAvailable = true
		}
		cards[task.ID] = card
	}

	return cards, nil
}

func (s *projectServer) uiTerminalJobsByTask(ctx context.Context, tasks []coordinator.Task) (map[string]string, error) {
	if len(tasks) == 0 || s.workers == nil || s.sessions == nil {
		return nil, nil
	}
	taskIDs := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		taskIDs[task.ID] = true
	}
	jobs, err := s.workers.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs for terminal availability: %w", err)
	}
	terminalJobs := map[string]string{}
	for _, job := range jobs {
		if job.TaskID == nil || !taskIDs[*job.TaskID] {
			continue
		}
		if _, exists := terminalJobs[*job.TaskID]; exists {
			continue
		}
		available, err := s.sessions.JobTerminalAvailable(ctx, job.ID)
		if err != nil {
			return nil, fmt.Errorf("load job terminal availability %s: %w", job.ID, err)
		}
		if available {
			terminalJobs[*job.TaskID] = job.ID
		}
	}

	return terminalJobs, nil
}

func (s *projectServer) applyHandoffSummary(ctx context.Context, card *uiTaskCard, change coordinator.Change) error {
	if s.reconciler == nil {
		return nil
	}
	snapshot, err := s.reconciler.GetHandoffSnapshot(ctx, change.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	card.Handoff = &uiHandoffSummary{
		HeadSHA:   snapshot.HeadSHA,
		Present:   snapshot.Present,
		Valid:     snapshot.Valid,
		Summary:   snapshot.Summary,
		UpdatedAt: snapshot.UpdatedAt,
	}

	return nil
}

func uiSessionSummaryFromSession(session coordinator.Session) *uiSessionSummary {
	return &uiSessionSummary{
		ID:                  session.ID,
		ChangeID:            session.ChangeID,
		WorkerID:            session.WorkerID,
		State:               session.RuntimeState,
		Branch:              session.Branch,
		Base:                session.Base,
		Harness:             session.Harness,
		TranscriptAvailable: strings.TrimSpace(session.TranscriptPath) != "",
		UpdatedAt:           session.UpdatedAt,
		LastAgentActivityAt: session.LastAgentActivityAt,
	}
}

func (s *projectServer) uiSessionSummaryWithTerminal(ctx context.Context, session coordinator.Session) (*uiSessionSummary, error) {
	summary := uiSessionSummaryFromSession(session)
	available, err := s.sessions.TerminalAvailable(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	summary.TerminalAvailable = available
	return summary, nil
}

func uiChangeSummaryFromChange(change coordinator.Change) *uiChangeSummary {
	return &uiChangeSummary{
		ID:        change.ID,
		Branch:    change.Branch,
		Base:      change.Base,
		HeadSHA:   change.HeadSHA,
		ReadyAt:   change.ReadyAt,
		MergedAt:  change.MergedAt,
		UpdatedAt: change.UpdatedAt,
	}
}

func uiRequiredCheckSummaryFromChecks(checks []coordinator.Check) uiRequiredCheckSummary {
	var summary uiRequiredCheckSummary
	for _, check := range checks {
		if !check.Required {
			continue
		}
		summary.Total++
		switch check.Verdict {
		case coordinator.CheckPending:
			summary.Pending++
			if check.Kind == coordinator.CheckKindHuman {
				summary.PendingHumanReview = true
			}
		case coordinator.CheckSatisfied:
			summary.Satisfied++
		case coordinator.CheckBlocked:
			summary.Blocked++
		case coordinator.CheckSkipped:
			summary.Skipped++
		case coordinator.CheckErrored:
			summary.Errored++
		}
	}

	return summary
}

func uiBlockerSummaryFromTasks(blockers []coordinator.Task) uiBlockerSummary {
	summary := uiBlockerSummary{Count: len(blockers)}
	for i, blocker := range blockers {
		if i >= 3 {
			break
		}
		summary.Tasks = append(summary.Tasks, uiBlockerTaskSummary{
			ID:    blocker.ID,
			Title: blocker.Title,
		})
	}

	return summary
}

func uiRelationSummaryFromRelations(taskID string, relations []coordinator.TaskRelation) uiRelationSummary {
	summary := uiRelationSummary{Total: len(relations)}
	for _, relation := range relations {
		source := strings.TrimSpace(relation.SourceTaskID)
		target := strings.TrimSpace(relation.TargetTaskID)
		switch relation.Kind {
		case coordinator.RelationParentOf:
			if source == taskID {
				summary.Children++
			} else if target == taskID {
				summary.Parents++
			}
		case coordinator.RelationBlocks:
			if source == taskID {
				summary.Blocks++
			} else if target == taskID {
				summary.BlockedBy++
			}
		case coordinator.RelationRelatedTo:
			summary.Related++
		}
	}

	return summary
}

func uiWorkflowStepSummaryFromRun(run coordinator.WorkflowRun) *uiWorkflowStepSummary {
	key := strings.TrimSpace(run.CurrentNodeKey)
	if key == "" {
		return nil
	}

	summary := &uiWorkflowStepSummary{Key: key}
	if node, ok := run.Snapshot.Node(key); ok {
		summary.Kind = node.Kind
		if strings.TrimSpace(node.Name) != "" {
			summary.Name = node.Name
		}
	}
	if summary.Name == "" {
		summary.Name = workflowNodeKeyLabel(key)
	}
	if summary.Name == "" {
		return nil
	}
	return summary
}

func workflowNodeKeyLabel(key string) string {
	return coordinator.NodeKeyLabel(key)
}

// applyUIWorkflowCardState projects the coordinator's per-run board summary
// onto the card the web UI renders.
func applyUIWorkflowCardState(card *uiTaskCard, state coordinator.WorkflowCardState) {
	card.StepIndex = state.StepIndex
	card.StepCount = state.StepCount
	card.Held = state.Held
	card.HeldBy = state.HeldBy
	if !state.DwellSince.IsZero() {
		dwell := state.DwellSince
		card.DwellSince = &dwell
	}
	if state.Wait != nil {
		card.Wait = &uiWorkflowWait{
			Kind:      state.Wait.Kind,
			Reason:    state.Wait.Reason,
			Message:   state.Wait.Message,
			NodeRunID: state.Wait.NodeRunID,
			CreatedAt: state.Wait.CreatedAt,
		}
	}
}

func uiWorkerDiagnosticsFromLeases(workers []worker.Worker, leases []worker.Lease, now time.Time) map[string]uiWorkerDiagnostics {
	if len(workers) == 0 {
		return nil
	}
	diagnostics := make(map[string]uiWorkerDiagnostics, len(workers))
	for _, registeredWorker := range workers {
		diagnostics[registeredWorker.ID] = uiWorkerDiagnostics{}
	}
	for _, lease := range leases {
		if lease.ReleasedAt != nil {
			continue
		}
		diagnostic, ok := diagnostics[lease.WorkerID]
		if !ok {
			continue
		}
		live := uiLeaseIsLive(lease, now)
		if live {
			diagnostic.LiveJobs++
		} else {
			diagnostic.ExpiredUnreleasedJobs++
		}
		switch lease.CapacityBucket {
		case worker.BucketPersistentAgent:
			if live {
				diagnostic.LivePersistentAgent++
			} else {
				diagnostic.ExpiredUnreleasedPersistentAgent++
			}
		case worker.BucketEphemeral:
			if live {
				diagnostic.LiveEphemeral++
			} else {
				diagnostic.ExpiredUnreleasedEphemeral++
			}
		}
		diagnostics[lease.WorkerID] = diagnostic
	}

	return diagnostics
}

func uiQueueSummaryFromJobs(jobs []worker.Job) uiQueueSummary {
	var summary uiQueueSummary
	for _, job := range jobs {
		if job.State != worker.JobQueued {
			continue
		}
		summary.Queued++
		switch job.CapacityBucket {
		case worker.BucketPersistentAgent:
			summary.PersistentAgent++
		case worker.BucketEphemeral:
			summary.Ephemeral++
		}
		switch job.Role {
		case worker.RoleAuthor:
			summary.Author++
		case worker.RoleReviewer:
			summary.Reviewer++
		case worker.RoleVerifier:
			summary.Verifier++
		case worker.RoleCI:
			summary.CI++
		case worker.RoleConsole:
			summary.Console++
		}
	}

	return summary
}

func (s *projectServer) buildUIJobDiagnostics(ctx context.Context, jobs []worker.Job) (map[string]uiJobDiagnostics, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	leases, err := s.workers.ListLeases(ctx)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	latestLeaseByJob := uiLatestLeaseByJob(leases)
	now := time.Now().UTC()
	diagnostics := make(map[string]uiJobDiagnostics, len(jobs))
	for _, job := range jobs {
		diagnostic := uiJobDiagnostics{
			ProjectID:   s.project.ID,
			ProjectName: s.project.Name,
		}
		if lease, ok := latestLeaseByJob[job.ID]; ok {
			leaseCopy := lease
			diagnostic.Lease = &leaseCopy
			diagnostic.LiveLease = uiLeaseIsLive(lease, now)
			diagnostic.LeaseStatus = uiLeaseStatus(lease, now)
		}
		if job.State == worker.JobClaimed || job.State == worker.JobRunning {
			diagnostic.TmuxSession = terminal.TmuxSessionNameForJob(job.ID)
		}
		diagnostic.TranscriptAvailable = strings.TrimSpace(job.TranscriptPath) != ""
		if s.sessions != nil {
			available, err := s.sessions.JobTerminalAvailable(ctx, job.ID)
			if err != nil {
				return nil, fmt.Errorf("load job terminal availability %s: %w", job.ID, err)
			}
			diagnostic.TerminalAvailable = available
		}
		if job.ChangeID != nil && s.sessions != nil {
			change, err := s.sessions.GetChange(ctx, *job.ChangeID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("load job change %s: %w", job.ID, err)
			}
			if err == nil {
				diagnostic.Change = uiChangeSummaryFromChange(change)
			}
		}
		if s.sessions != nil {
			session, ok, err := s.sessions.LatestSessionForJob(ctx, job.ID)
			if err != nil {
				return nil, fmt.Errorf("load job session %s: %w", job.ID, err)
			}
			if ok {
				summary, err := s.uiSessionSummaryWithTerminal(ctx, session)
				if err != nil {
					return nil, fmt.Errorf("load job session terminal availability %s: %w", job.ID, err)
				}
				diagnostic.Session = summary
			}
		}
		diagnostics[job.ID] = diagnostic
	}

	return diagnostics, nil
}

func uiLatestLeaseByJob(leases []worker.Lease) map[string]worker.Lease {
	latest := make(map[string]worker.Lease)
	for _, lease := range leases {
		if _, ok := latest[lease.JobID]; ok {
			continue
		}
		latest[lease.JobID] = lease
	}

	return latest
}

func uiLeaseIsLive(lease worker.Lease, now time.Time) bool {
	return lease.ReleasedAt == nil && lease.ExpiresAt.After(now)
}

func uiLeaseStatus(lease worker.Lease, now time.Time) string {
	if lease.ReleasedAt != nil {
		return "released"
	}
	if !lease.ExpiresAt.After(now) {
		return "expired"
	}
	return "live"
}
