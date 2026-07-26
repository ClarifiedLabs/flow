package coordinator

import (
	"context"
	"database/sql"
	"strings"
)

// NodeKeyLabel turns a snapshot node key into prose ("automated_checks" →
// "automated checks"). Used when a frozen node carries no display name.
func NodeKeyLabel(key string) string {
	label := strings.NewReplacer("_", " ", "-", " ").Replace(key)
	return strings.Join(strings.Fields(label), " ")
}

func workflowNodeKeyLabel(key string) string { return NodeKeyLabel(key) }

// attachNodeRunJobs hangs each node run's fanned-out jobs off it, joined to the
// check the job reported so a review agent shows as "security-review ·
// satisfied" rather than an opaque job id. Jobs are matched to checks through
// checks.source_job_id.
func (s *WorkflowRunService) attachNodeRunJobs(ctx context.Context, run WorkflowRun, nodeRuns []WorkflowNodeRun) error {
	if len(nodeRuns) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT j.node_run_id, j.id, j.role, j.state,
	COALESCE(c.name, ''), COALESCE(c.verdict, ''), COALESCE(c.details, ''),
	COALESCE(l.worker_id, '')
FROM jobs j
LEFT JOIN checks c ON c.source_job_id = j.id
LEFT JOIN leases l ON l.id = (
	SELECT id FROM leases WHERE job_id = j.id ORDER BY leased_at DESC, id DESC LIMIT 1
)
WHERE j.workflow_run_id = ? AND j.node_run_id IS NOT NULL
ORDER BY j.created_at, j.id`, run.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	byNodeRun := map[string][]WorkflowNodeRunJob{}
	for rows.Next() {
		var nodeRunID sql.NullString
		var job WorkflowNodeRunJob
		var verdict string
		if err := rows.Scan(&nodeRunID, &job.ID, &job.Role, &job.State,
			&job.Name, &verdict, &job.Details, &job.WorkerID); err != nil {
			return err
		}
		job.Verdict = CheckVerdict(verdict)
		byNodeRun[nodeRunID.String] = append(byNodeRun[nodeRunID.String], job)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range nodeRuns {
		nodeRuns[index].Jobs = byNodeRun[nodeRuns[index].ID]
	}
	return nil
}

// transitionCounts tallies how often the run traversed each graph edge.
// Lifecycle-only rows (task_scheduled, workflow_completed) carry no
// from/to/outcome triple and are skipped so terminal edges are not
// double-counted.
func (s *WorkflowRunService) transitionCounts(ctx context.Context, runID string) ([]WorkflowEdgeCount, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT from_node_key, outcome, to_node_key, COUNT(*)
FROM workflow_transitions
WHERE workflow_run_id = ?
	AND from_node_key != '' AND to_node_key != '' AND outcome != ''
GROUP BY from_node_key, outcome, to_node_key
ORDER BY from_node_key, outcome, to_node_key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counts []WorkflowEdgeCount
	for rows.Next() {
		var count WorkflowEdgeCount
		if err := rows.Scan(&count.From, &count.Outcome, &count.To, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}
