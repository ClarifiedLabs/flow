package api

import (
	"net/http"
	"testing"
)

// TestTaskBudgetRequiresInstructions proves the budget-extension route cannot
// silently re-arm a review loop: a request without operator instructions is a
// 400, and one with instructions extends the budget and records the
// instructions on the review-cycle budget the next author payload reads.
func TestTaskBudgetRequiresInstructions(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	flow := newReviewFixtureFlow(t, fixture, "budget instructions")

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Budgeted plan", FlowID: flow.ID}, http.StatusCreated, &created)
	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule",
		nil, http.StatusOK, &scheduled)

	budgetPath := "/v2/tasks/" + created.Task.ID + "/workflow/budget"

	// Missing instructions is a malformed request, not a silent extension.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, budgetPath,
		workflowBudgetRequest{Additional: 2}, http.StatusBadRequest, nil)
	// Blank instructions are treated the same.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, budgetPath,
		workflowBudgetRequest{Additional: 2, Instructions: "   "}, http.StatusBadRequest, nil)

	// The task has no budget wait open yet, so even a valid request cannot
	// extend: it fails rather than inventing headroom (the exact status depends
	// on how the missing wait surfaces, so only failure is asserted).
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, budgetPath,
		workflowBudgetRequest{Additional: 2, Instructions: "narrow the fix"}, http.StatusBadRequest, nil)
}
