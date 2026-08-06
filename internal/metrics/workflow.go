package metrics

// Workflow is the low-cardinality metric set for durable owner rulings and
// review-scope decisions. Labels are closed enums; task, run, wait, job, and
// ruling identifiers must never be attached.
type Workflow struct {
	OwnerRulings       *Counter
	RulingDeliveries   *Counter
	DecisionsOpened    *Counter
	DecisionsResolved  *Counter
	DecisionReruns     *Counter
	DecisionRejections *Counter
}

func RegisterWorkflow(registry *Registry) *Workflow {
	if registry == nil {
		return nil
	}
	return &Workflow{
		OwnerRulings:       registry.Counter("flow_owner_rulings_total", "Durable owner rulings by source."),
		RulingDeliveries:   registry.Counter("flow_owner_ruling_deliveries_total", "Owner-ruling live-session delivery outcomes."),
		DecisionsOpened:    registry.Counter("flow_review_scope_decisions_opened_total", "Review-scope decision waits opened."),
		DecisionsResolved:  registry.Counter("flow_review_scope_decisions_resolved_total", "Review-scope decisions resolved by owner choice."),
		DecisionReruns:     registry.Counter("flow_review_scope_decision_reruns_total", "Review-scope decision reruns by breadth."),
		DecisionRejections: registry.Counter("flow_review_scope_decision_rejections_total", "Review-scope decision requests rejected by convergence guard."),
	}
}
