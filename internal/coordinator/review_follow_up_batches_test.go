package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/checkverdict"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func followUpBatchReport(head string, comments int) []byte {
	introduced := false
	report := checkverdict.VerdictReport{Verdict: "satisfied", Reason: "deferred findings"}
	for index := 0; index < comments; index++ {
		report.Comments = append(report.Comments, checkverdict.ReviewCommentReport{
			SHA: head, File: "follow_up.go", Line: index + 1, Body: "deferred concern " + string(rune('a'+index)),
			Severity: "medium", IntroducedByChange: &introduced, Requirement: "preserve deferred behavior",
			RequirementSource: "explicit", FindingBasis: "explicit_requirement", RemediationScope: "local",
			ScopeRationale: "the concern is independent of source approval", FollowUp: "organize after approval",
			TaskAction: &checkverdict.ReviewTaskActionReport{
				Action: "create_task", Title: "Deferred fix " + string(rune('A'+index)), Body: "Implement the deferred fix.",
			},
		})
	}
	data, err := json.Marshal(report)
	if err != nil {
		panic(err)
	}
	return data
}

func batchInput(f scopeDecisionFixture, report []byte) ApplyReviewFollowUpBatchInput {
	sum := sha256.Sum256(report)
	return ApplyReviewFollowUpBatchInput{
		SourceTaskID: f.task.ID, LeaseID: f.leaseID, WorkerID: "w-scope", ReportJSON: report,
		ReportSHA256: hex.EncodeToString(sum[:]),
	}
}

func TestApplyReviewFollowUpBatchAcceptsAtomicallyWithoutCreatingTasksAndReplays(t *testing.T) {
	f := newScopeDecisionFixture(t)
	report := followUpBatchReport(f.change.HeadSHA, 2)
	service := NewTaskService(f.runs.db, "p-test")

	var tasksBefore int
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasksBefore); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.ApplyReviewFollowUpBatch(context.Background(), batchInput(f, report))
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.Replayed || accepted.BatchID == "" || accepted.SetID == "" ||
		accepted.SetRevision != 1 || accepted.ProposalCount != 2 || accepted.SetState != ReviewFollowUpSetOpen {
		t.Fatalf("accepted receipt = %+v", accepted)
	}
	var tasksAfter, batches, proposals int
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasksAfter); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_proposals`).Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if tasksAfter != tasksBefore || batches != 1 || proposals != 2 {
		t.Fatalf("tasks/batches/proposals = %d/%d/%d, want %d/1/2", tasksAfter, batches, proposals, tasksBefore)
	}

	replayed, err := service.ApplyReviewFollowUpBatch(context.Background(), batchInput(f, report))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.BatchID != accepted.BatchID || replayed.SetID != accepted.SetID || replayed.SetRevision != 1 {
		t.Fatalf("replayed receipt = %+v, want ids from %+v", replayed, accepted)
	}
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_proposals`).Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if proposals != 2 {
		t.Fatalf("proposals after replay = %d, want 2", proposals)
	}

	changed := followUpBatchReport(f.change.HeadSHA, 1)
	_, err = service.ApplyReviewFollowUpBatch(context.Background(), batchInput(f, changed))
	if err == nil || !strings.Contains(err.Error(), "different report digest") {
		t.Fatalf("digest conflict error = %v", err)
	}
}

func TestApplyReviewFollowUpBatchRollsBackEveryProposal(t *testing.T) {
	f := newScopeDecisionFixture(t)
	if _, err := f.runs.db.Exec(`
CREATE TRIGGER fail_second_review_follow_up_proposal
BEFORE INSERT ON review_follow_up_proposals
WHEN NEW.comment_index = 1
BEGIN SELECT RAISE(ABORT, 'injected proposal failure'); END`); err != nil {
		t.Fatal(err)
	}
	service := NewTaskService(f.runs.db, "p-test")
	_, err := service.ApplyReviewFollowUpBatch(context.Background(), batchInput(f, followUpBatchReport(f.change.HeadSHA, 2)))
	if err == nil || !strings.Contains(err.Error(), "injected proposal failure") {
		t.Fatalf("batch error = %v", err)
	}
	for _, table := range []string{"review_follow_up_sets", "review_follow_up_batches", "review_follow_up_proposals"} {
		var count int
		if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0", table, count)
		}
	}
}

func TestApplyReviewFollowUpBatchAccumulatesReviewVisitsInOneSet(t *testing.T) {
	f := newScopeDecisionFixture(t)
	service := NewTaskService(f.runs.db, "p-test")
	first, err := service.ApplyReviewFollowUpBatch(context.Background(), batchInput(f, followUpBatchReport(f.change.HeadSHA, 1)))
	if err != nil {
		t.Fatal(err)
	}

	queue := flowworker.NewService(f.runs.db)
	secondJob, err := queue.EnqueueJob(context.Background(), flowworker.EnqueueJobInput{
		TaskID: &f.task.ID, ChangeID: &f.change.ID, WorkflowRunID: &f.run.ID, NodeRunID: &f.node.ID,
		Role: flowworker.RoleReviewer, CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{"review_aggregation": true, "check_name": f.checkName, "head_sha": f.change.HeadSHA, "blocking": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := f.runs.db.Exec(`UPDATE jobs SET state = 'running' WHERE id = ?`, secondJob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runs.db.Exec(`
INSERT INTO leases (id, job_id, worker_id, capacity_bucket, leased_at, expires_at)
VALUES ('l-second-visit', ?, 'w-scope', 'ephemeral', ?, ?)`, secondJob.ID, formatTime(now), formatTime(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runs.db.Exec(`UPDATE checks SET source_job_id = ? WHERE task_id = ? AND name = ?`, secondJob.ID, f.task.ID, f.checkName); err != nil {
		t.Fatal(err)
	}
	secondInput := batchInput(f, followUpBatchReport(f.change.HeadSHA, 2))
	secondInput.LeaseID = "l-second-visit"
	second, err := service.ApplyReviewFollowUpBatch(context.Background(), secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if second.SetID != first.SetID || second.SetRevision != 2 || second.ProposalCount != 2 {
		t.Fatalf("second receipt = %+v, first = %+v", second, first)
	}
	var sets, batches, proposals int
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_sets`).Scan(&sets); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_proposals`).Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if sets != 1 || batches != 2 || proposals != 3 {
		t.Fatalf("sets/batches/proposals = %d/%d/%d, want 1/2/3", sets, batches, proposals)
	}
}

func TestApplyReviewFollowUpBatchRejectsStaleAndUnauthorizedSources(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, scopeDecisionFixture)
	}{
		{name: "expired lease", mutate: func(t *testing.T, f scopeDecisionFixture) {
			_, err := f.runs.db.Exec(`UPDATE leases SET expires_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Minute)), f.leaseID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "terminal job", mutate: func(t *testing.T, f scopeDecisionFixture) {
			_, err := f.runs.db.Exec(`UPDATE jobs SET state = 'finished' WHERE id = ?`, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non pending check", mutate: func(t *testing.T, f scopeDecisionFixture) {
			_, err := f.runs.db.Exec(`UPDATE checks SET verdict = 'satisfied' WHERE task_id = ? AND name = ?`, f.task.ID, f.checkName)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stale head", mutate: func(t *testing.T, f scopeDecisionFixture) {
			_, err := f.runs.db.Exec(`UPDATE changes SET head_sha = 'new-head' WHERE id = ?`, f.change.ID)
			if err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newScopeDecisionFixture(t)
			test.mutate(t, f)
			_, err := NewTaskService(f.runs.db, "p-test").ApplyReviewFollowUpBatch(
				context.Background(), batchInput(f, followUpBatchReport(f.change.HeadSHA, 1)),
			)
			if err == nil {
				t.Fatal("expected rejection")
			}
			var batches int
			if queryErr := f.runs.db.QueryRow(`SELECT COUNT(*) FROM review_follow_up_batches`).Scan(&batches); queryErr != nil {
				t.Fatal(queryErr)
			}
			if batches != 0 {
				t.Fatalf("batches = %d, want 0", batches)
			}
		})
	}
}
