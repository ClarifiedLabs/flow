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

func (s *projectServer) buildUIIssueCards(ctx context.Context, issues []coordinator.Issue) (map[string]uiIssueCard, error) {
	if len(issues) == 0 {
		return nil, nil
	}

	terminalJobs, err := s.uiTerminalJobsByIssue(ctx, issues)
	if err != nil {
		return nil, err
	}

	cards := make(map[string]uiIssueCard, len(issues))
	for _, issue := range issues {
		card := uiIssueCard{IssueID: issue.ID}
		tags, err := s.issues.TagsForIssue(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load tags for %s: %w", issue.ID, err)
		}
		card.Tags = tags
		relations, err := s.issues.RelationsForIssue(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load relations for %s: %w", issue.ID, err)
		}
		card.Relations = uiRelationSummaryFromRelations(issue.ID, relations)
		if s.sessions != nil {
			active, ok, err := s.sessions.ActiveAuthorSessionForIssue(ctx, issue.ID)
			if err != nil {
				return nil, fmt.Errorf("load active session for %s: %w", issue.ID, err)
			}
			if ok {
				summary, err := s.uiSessionSummaryWithTerminal(ctx, active)
				if err != nil {
					return nil, fmt.Errorf("load terminal availability for %s: %w", issue.ID, err)
				}
				card.ActiveSession = summary
				card.TerminalAvailable = summary.TerminalAvailable
				change, err := s.sessions.GetChange(ctx, active.ChangeID)
				if err != nil {
					return nil, fmt.Errorf("load active change for %s: %w", issue.ID, err)
				}
				card.Change = uiChangeSummaryFromChange(change)
				if err := s.applyHandoffSummary(ctx, &card, change); err != nil {
					return nil, fmt.Errorf("load handoff summary for %s: %w", issue.ID, err)
				}
			}
			readyChange, ok, err := s.sessions.ReadyUnmergedChangeForIssue(ctx, issue.ID)
			if err != nil {
				return nil, fmt.Errorf("load ready change for %s: %w", issue.ID, err)
			}
			if ok {
				card.Change = uiChangeSummaryFromChange(readyChange)
				if err := s.applyHandoffSummary(ctx, &card, readyChange); err != nil {
					return nil, fmt.Errorf("load handoff summary for %s: %w", issue.ID, err)
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
			checks, err := s.checks.ListChecks(ctx, issue.ID)
			if err != nil {
				return nil, fmt.Errorf("load checks for %s: %w", issue.ID, err)
			}
			card.RequiredChecks = uiRequiredCheckSummaryFromChecks(checks)
			reviewState, err := s.checks.ReviewState(ctx, issue.ID)
			if err != nil {
				return nil, fmt.Errorf("load review state for %s: %w", issue.ID, err)
			}
			card.ReviewState = reviewState
		}
		if s.status != nil {
			statusLog, err := s.status.ListForIssue(ctx, issue.ID, 1)
			if err != nil {
				return nil, fmt.Errorf("load latest status for %s: %w", issue.ID, err)
			}
			if len(statusLog) > 0 {
				card.LatestStatus = &statusLog[0]
			}
		}
		if s.sessions != nil {
			budget, err := s.sessions.ReviewCycleBudget(ctx, issue.ID)
			if err != nil {
				return nil, fmt.Errorf("load review cycle budget for %s: %w", issue.ID, err)
			}
			card.ReviewCycleBudget = &budget
		}
		blockers, err := s.issues.UnresolvedBlockers(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load blockers for %s: %w", issue.ID, err)
		}
		card.Blockers = uiBlockerSummaryFromIssues(blockers)
		crashRetry, err := s.issues.CrashRetryAvailable(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("load crash retry availability for %s: %w", issue.ID, err)
		}
		card.CrashRetryAvailable = crashRetry
		card.BlockingReason = uiBlockingReason(issue, card)
		card.PrimaryAction = uiPrimaryAction(issue, card)
		if jobID, ok := terminalJobs[issue.ID]; ok {
			card.TerminalJobID = jobID
			card.TerminalAvailable = true
		}
		cards[issue.ID] = card
	}

	return cards, nil
}

func (s *projectServer) uiTerminalJobsByIssue(ctx context.Context, issues []coordinator.Issue) (map[string]string, error) {
	if len(issues) == 0 || s.workers == nil || s.sessions == nil {
		return nil, nil
	}
	issueIDs := make(map[string]bool, len(issues))
	for _, issue := range issues {
		issueIDs[issue.ID] = true
	}
	jobs, err := s.workers.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs for terminal availability: %w", err)
	}
	terminalJobs := map[string]string{}
	for _, job := range jobs {
		if job.IssueID == nil || !issueIDs[*job.IssueID] {
			continue
		}
		if _, exists := terminalJobs[*job.IssueID]; exists {
			continue
		}
		available, err := s.sessions.JobTerminalAvailable(ctx, job.ID)
		if err != nil {
			return nil, fmt.Errorf("load job terminal availability %s: %w", job.ID, err)
		}
		if available {
			terminalJobs[*job.IssueID] = job.ID
		}
	}

	return terminalJobs, nil
}

func (s *projectServer) applyHandoffSummary(ctx context.Context, card *uiIssueCard, change coordinator.Change) error {
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
		}
	}

	return summary
}

func uiBlockerSummaryFromIssues(blockers []coordinator.Issue) uiBlockerSummary {
	summary := uiBlockerSummary{Count: len(blockers)}
	for i, blocker := range blockers {
		if i >= 3 {
			break
		}
		summary.Issues = append(summary.Issues, uiBlockerIssueSummary{
			ID:    blocker.ID,
			Title: blocker.Title,
		})
	}

	return summary
}

func uiRelationSummaryFromRelations(issueID string, relations []coordinator.IssueRelation) uiRelationSummary {
	summary := uiRelationSummary{Total: len(relations)}
	for _, relation := range relations {
		source := strings.TrimSpace(relation.SourceIssueID)
		target := strings.TrimSpace(relation.TargetIssueID)
		switch relation.Kind {
		case coordinator.RelationParentOf:
			if source == issueID {
				summary.Children++
			} else if target == issueID {
				summary.Parents++
			}
		case coordinator.RelationBlocks:
			if source == issueID {
				summary.Blocks++
			} else if target == issueID {
				summary.BlockedBy++
			}
		case coordinator.RelationRelatedTo:
			summary.Related++
		}
	}

	return summary
}

func uiBlockingReason(issue coordinator.Issue, card uiIssueCard) string {
	switch {
	case issue.TriageState == coordinator.TriagePending:
		return "awaiting triage"
	case card.Blockers.Count > 0:
		return "blocked by issue"
	case card.ReviewCycleBudget != nil && card.ReviewCycleBudget.Exhausted:
		return "review cycle limit"
	case card.RequiredChecks.Blocked > 0:
		return "required check blocked"
	case card.RequiredChecks.PendingHumanReview:
		return "human review pending"
	case card.RequiredChecks.Pending > 0:
		return "required check pending"
	default:
		return ""
	}
}

func uiPrimaryAction(issue coordinator.Issue, card uiIssueCard) string {
	if issue.TriageState == coordinator.TriagePending {
		return "triage"
	}
	if card.ActiveSession != nil {
		if card.ActiveSession.State == coordinator.SessionWaiting {
			return "respond"
		}
		return "monitor"
	}
	if card.Change != nil && card.ReviewState == coordinator.ReviewApproved && !issue.AutoMerge {
		return "merge"
	}
	if card.ReviewCycleBudget != nil && card.ReviewCycleBudget.Exhausted {
		return "recover"
	}
	if card.RequiredChecks.PendingHumanReview {
		return "review"
	}
	if card.RequiredChecks.Blocked > 0 {
		return "review"
	}
	if card.Blockers.Count > 0 {
		return "unblock"
	}
	if issue.ScheduleState == coordinator.ScheduleBacklog {
		return "queue"
	}
	if issue.ScheduleState == coordinator.ScheduleUpNext {
		return "start"
	}

	return ""
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
