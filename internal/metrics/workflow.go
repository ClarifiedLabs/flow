package metrics

// Workflow is the low-cardinality metric set for durable owner rulings and
// review-scope decisions. Labels are closed enums; task, run, wait, job, and
// ruling identifiers must never be attached.
type Workflow struct {
	OwnerRulings                   *Counter
	RulingDeliveries               *Counter
	DecisionsOpened                *Counter
	DecisionsResolved              *Counter
	DecisionReruns                 *Counter
	DecisionRejections             *Counter
	ReviewFollowUpBatches          *Counter
	ReviewFollowUpProposals        *Counter
	ReviewFollowUpPlanOutcomes     *Counter
	ReviewFollowUpOrganizerRuns    *Counter
	ReviewFollowUpMaterializations *Counter
	ReviewFollowUpBlockedTasks     *Counter
}

func RegisterWorkflow(registry *Registry) *Workflow {
	if registry == nil {
		return nil
	}
	return &Workflow{
		OwnerRulings:                   registry.Counter("flow_owner_rulings_total", "Durable owner rulings by source."),
		RulingDeliveries:               registry.Counter("flow_owner_ruling_deliveries_total", "Owner-ruling live-session delivery outcomes."),
		DecisionsOpened:                registry.Counter("flow_review_scope_decisions_opened_total", "Review-scope decision waits opened."),
		DecisionsResolved:              registry.Counter("flow_review_scope_decisions_resolved_total", "Review-scope decisions resolved by owner choice."),
		DecisionReruns:                 registry.Counter("flow_review_scope_decision_reruns_total", "Review-scope decision reruns by breadth."),
		DecisionRejections:             registry.Counter("flow_review_scope_decision_rejections_total", "Review-scope decision requests rejected by convergence guard."),
		ReviewFollowUpBatches:          registry.Counter("flow_review_follow_up_batches_total", "Review follow-up batch ingestion outcomes."),
		ReviewFollowUpProposals:        registry.Counter("flow_review_follow_up_proposals_accepted_total", "Review follow-up proposal occurrences durably accepted."),
		ReviewFollowUpPlanOutcomes:     registry.Counter("flow_review_follow_up_plan_outcomes_total", "Human-reviewed follow-up proposal outcomes."),
		ReviewFollowUpOrganizerRuns:    registry.Counter("flow_review_follow_up_organizer_runs_total", "Review follow-up organizer run outcomes."),
		ReviewFollowUpMaterializations: registry.Counter("flow_review_follow_up_materializations_total", "Review follow-up materialization outcomes."),
		ReviewFollowUpBlockedTasks:     registry.Counter("flow_review_follow_up_dependency_blocked_tasks_total", "Generated review follow-up tasks with declared dependency blockers."),
	}
}
