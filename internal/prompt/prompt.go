package prompt

import (
	"fmt"
	"strings"

	flowskills "github.com/ClarifiedLabs/flow/skills"
)

const (
	RoleAuthor   = "author"
	RoleReviewer = "reviewer"
	RoleVerifier = "verifier"
)

type Input struct {
	Role                       string
	IssueID                    string
	IssueTitle                 string
	IssueBody                  string
	IssueAcceptanceCriteria    string
	ChangeID                   string
	Branch                     string
	Base                       string
	CheckName                  string
	SessionPurpose             string
	ReviewState                string
	FixRound                   bool
	PlanMode                   bool
	ProjectID                  string
	ProjectName                string
	ApprovedPlan               string
	ReviewCycleInstructions    string
	HumanAttentionInstructions string
	HumanAttentionContext      string
	// PriorHandoff is the previous session's handoff body, fetched from the
	// coordinator by the session builder. It replaces the committed .handoff.md
	// the next author (fix round) and verifier used to read from the worktree.
	PriorHandoff string
	// CompletionAssessment marks a reviewer check enqueued to recover a crashed
	// author session that never ran flow ready (Mode-B recovery). It is
	// reviewer-only: Build renders extra guidance asking the reviewer to judge
	// whether the work is actually complete (pass) or still has work remaining
	// (block, so the author resumes) instead of running a blind full relaunch.
	CompletionAssessment bool
	BlockedChecks        []BlockedCheck
	ReviewThreads        []ReviewThread
}

type BlockedCheck struct {
	ID          int64
	Name        string
	Kind        string
	Reporter    string
	SourceJobID string
	ExitCode    *int
	Details     string
}

type ReviewThread struct {
	ID        string
	State     string
	FilePath  string
	Line      int
	Context   string
	CreatedBy string
	Comments  []ReviewComment
}

type ReviewComment struct {
	Actor string
	Body  string
}

func Build(input Input) (string, error) {
	role := normalizeRole(input.Role)
	if role == "" {
		role = RoleAuthor
	}
	if err := validateRole(role); err != nil {
		return "", err
	}

	skillName := "flow-" + role
	skillInstructions, err := flowskills.Instructions(skillName)
	if err != nil {
		return "", err
	}
	lines := []string{
		fmt.Sprintf("Flow role instructions (%s):", skillName),
		"",
		skillInstructions,
		"",
		fmt.Sprintf("You are the %s agent for Flow.", role),
	}
	lines = append(lines, fmt.Sprintf("Issue: %s", valueOrUnknown(input.IssueID)))
	if strings.TrimSpace(input.IssueTitle) != "" {
		lines = append(lines, fmt.Sprintf("Issue Title: %s", strings.TrimSpace(input.IssueTitle)))
	}
	if strings.TrimSpace(input.ChangeID) != "" {
		lines = append(lines, fmt.Sprintf("Change: %s", strings.TrimSpace(input.ChangeID)))
	}
	if strings.TrimSpace(input.Branch) != "" {
		lines = append(lines, fmt.Sprintf("Branch: %s", strings.TrimSpace(input.Branch)))
	}
	if strings.TrimSpace(input.Base) != "" {
		lines = append(lines, fmt.Sprintf("Base: %s", strings.TrimSpace(input.Base)))
	}
	if strings.TrimSpace(input.CheckName) != "" {
		lines = append(lines, fmt.Sprintf("Check: %s", strings.TrimSpace(input.CheckName)))
	}
	if input.FixRound {
		lines = append(lines, "Round: fix/rework")
	}
	lines = appendReviewContext(lines, input)
	if strings.TrimSpace(input.IssueBody) != "" {
		lines = append(lines, "", "Issue Body:", strings.TrimSpace(input.IssueBody))
	}
	if strings.TrimSpace(input.IssueAcceptanceCriteria) != "" {
		lines = append(lines, "", "Acceptance Criteria:", strings.TrimSpace(input.IssueAcceptanceCriteria))
	}
	if strings.TrimSpace(input.PriorHandoff) != "" {
		lines = append(lines, "", "Prior Handoff (from the previous session; there is no handoff file in the worktree to read):", strings.TrimSpace(input.PriorHandoff))
	}
	if role == RoleReviewer && input.CompletionAssessment {
		lines = append(lines, "", "Completion Assessment:")
		lines = append(lines, completionAssessmentInstructions()...)
	}
	if role == RoleAuthor && input.PlanMode {
		lines = append(lines, "", "Plan Mode:")
		lines = append(lines, planModeInstructions()...)
	}
	if role == RoleAuthor && strings.TrimSpace(input.ApprovedPlan) != "" {
		lines = append(lines, "", "Approved Plan:", strings.TrimSpace(input.ApprovedPlan))
	}
	if role == RoleAuthor && strings.TrimSpace(input.ReviewCycleInstructions) != "" {
		lines = append(lines, "", "Human Recovery Instructions:", strings.TrimSpace(input.ReviewCycleInstructions))
	}
	if role == RoleAuthor && strings.TrimSpace(input.HumanAttentionInstructions) != "" {
		lines = append(lines, "", "Human Response:", strings.TrimSpace(input.HumanAttentionInstructions))
	}
	if role == RoleAuthor && strings.TrimSpace(input.HumanAttentionContext) != "" {
		lines = append(lines, "", "Recent Human Attention Context:", strings.TrimSpace(input.HumanAttentionContext))
	}

	lines = append(lines, "")
	lines = append(lines, roleInstructions(role, input)...)
	return strings.Join(lines, "\n"), nil
}

// completionAssessmentInstructions is the reviewer guidance for a Mode-B
// recovery review: the prior author session exited without running flow ready,
// so rather than blindly relaunching a full author, a reviewer judges whether
// the work is actually finished. A satisfied verdict lets the change proceed to
// normal verification; a blocked verdict routes back to an author fix round (the
// existing review→fix cycle, bounded by the review-author cycle limit).
func completionAssessmentInstructions() []string {
	return []string{
		"This author session ended without finalizing (no flow ready was run); its work-in-progress is on the branch and its prior handoff is shown above.",
		"Determine whether the task is actually complete and ready for review, or whether work remains.",
		"If the work is complete, pass this check (record a satisfied verdict) so the change proceeds to verification.",
		"If work remains, block this check and record exactly what is left as blocking concerns, so the author resumes from this point instead of restarting.",
	}
}

func planModeInstructions() []string {
	return []string{
		"Do not make code changes in this planning session.",
		"Create a plan and ask the user for approval.",
		"After presenting the plan, record it with `flow status --kind plan \"<plan>\"`; Flow will move the issue to Needs Attention while this terminal session stays open.",
		"If you need to ask a question before implementing, ask it and record it with `flow status --kind question \"<question>\"` so Flow moves the issue to Needs Attention.",
		"After the user approves the plan, Flow will finish this planning session and start a separate implementation session.",
	}
}

func appendReviewContext(lines []string, input Input) []string {
	if strings.TrimSpace(input.ReviewState) == "" && !input.FixRound && len(input.BlockedChecks) == 0 && len(input.ReviewThreads) == 0 {
		return lines
	}

	lines = append(lines, "", "Current Review State:")
	if strings.TrimSpace(input.ReviewState) != "" {
		lines = append(lines, fmt.Sprintf("Review State: %s", strings.TrimSpace(input.ReviewState)))
	}
	if input.FixRound {
		lines = append(lines, "This is a fix/rework round. Address the blockers below before calling flow ready; do not restart the original implementation.")
	}
	if len(input.BlockedChecks) > 0 {
		lines = append(lines, "", "Blocked Required Checks:")
		for _, check := range input.BlockedChecks {
			lines = append(lines, formatBlockedCheck(check)...)
		}
	}
	if len(input.ReviewThreads) > 0 {
		lines = append(lines, "", "Open/Reopened Review Threads:")
		for _, thread := range input.ReviewThreads {
			lines = append(lines, formatReviewThread(thread)...)
		}
	}

	return lines
}

func formatBlockedCheck(check BlockedCheck) []string {
	name := valueOrUnknown(check.Name)
	metadata := []string{}
	if check.ID != 0 {
		metadata = append(metadata, fmt.Sprintf("Check ID: %d", check.ID))
	}
	if strings.TrimSpace(check.Kind) != "" {
		metadata = append(metadata, "Kind: "+strings.TrimSpace(check.Kind))
	}
	if strings.TrimSpace(check.Reporter) != "" {
		metadata = append(metadata, "Reporter: "+strings.TrimSpace(check.Reporter))
	}
	if strings.TrimSpace(check.SourceJobID) != "" {
		metadata = append(metadata, "Source Job: "+strings.TrimSpace(check.SourceJobID))
	}
	if check.ExitCode != nil {
		metadata = append(metadata, fmt.Sprintf("Exit Code: %d", *check.ExitCode))
	}

	line := "- " + name
	if len(metadata) > 0 {
		line += " (" + strings.Join(metadata, "; ") + ")"
	}
	lines := []string{line}
	return appendLabeledMultiline(lines, "Details", check.Details)
}

func formatReviewThread(thread ReviewThread) []string {
	id := valueOrUnknown(thread.ID)
	location := strings.TrimSpace(thread.FilePath)
	if thread.Line > 0 {
		location = fmt.Sprintf("%s:%d", valueOrUnknown(location), thread.Line)
	}
	if location == "" {
		location = "unknown"
	}

	metadata := []string{}
	if strings.TrimSpace(thread.State) != "" {
		metadata = append(metadata, "State: "+strings.TrimSpace(thread.State))
	}
	if strings.TrimSpace(thread.CreatedBy) != "" {
		metadata = append(metadata, "Created By: "+strings.TrimSpace(thread.CreatedBy))
	}

	line := fmt.Sprintf("- %s at %s", id, location)
	if len(metadata) > 0 {
		line += " (" + strings.Join(metadata, "; ") + ")"
	}
	lines := []string{line}
	lines = appendLabeledMultiline(lines, "Context", thread.Context)
	if latest, ok := latestReviewComment(thread.Comments); ok {
		label := "Latest Comment"
		if strings.TrimSpace(latest.Actor) != "" {
			label += " by " + strings.TrimSpace(latest.Actor)
		}
		lines = appendLabeledMultiline(lines, label, latest.Body)
	}
	return lines
}

func latestReviewComment(comments []ReviewComment) (ReviewComment, bool) {
	for index := len(comments) - 1; index >= 0; index-- {
		if strings.TrimSpace(comments[index].Body) != "" {
			return comments[index], true
		}
	}
	return ReviewComment{}, false
}

func appendLabeledMultiline(lines []string, label string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return lines
	}
	parts := strings.Split(value, "\n")
	first := strings.TrimSpace(parts[0])
	if first != "" {
		lines = append(lines, fmt.Sprintf("  %s: %s", label, first))
	}
	for _, part := range parts[1:] {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			lines = append(lines, "    "+trimmed)
		}
	}
	return lines
}

func RoleFromEnvironment(getenv func(string) string) string {
	if role := strings.TrimSpace(getenv("FLOW_WORKER_ROLE")); role != "" {
		return role
	}
	return strings.TrimSpace(getenv("FLOW_ROLE"))
}

func InputFromEnvironment(getenv func(string) string) Input {
	return Input{
		Role:                       RoleFromEnvironment(getenv),
		IssueID:                    getenv("FLOW_ISSUE_ID"),
		ChangeID:                   getenv("FLOW_CHANGE_ID"),
		Branch:                     getenv("FLOW_BRANCH"),
		Base:                       getenv("FLOW_BASE"),
		CheckName:                  getenv("FLOW_CHECK_NAME"),
		SessionPurpose:             getenv("FLOW_SESSION_PURPOSE"),
		ProjectID:                  getenv("FLOW_PROJECT_ID"),
		ProjectName:                getenv("FLOW_PROJECT_NAME"),
		ReviewCycleInstructions:    getenv("FLOW_REVIEW_CYCLE_INSTRUCTIONS"),
		HumanAttentionInstructions: getenv("FLOW_HUMAN_ATTENTION_INSTRUCTIONS"),
	}
}

func normalizeRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

func validateRole(role string) error {
	switch role {
	case RoleAuthor, RoleReviewer, RoleVerifier:
		return nil
	default:
		return fmt.Errorf("unsupported Flow worker role %q", role)
	}
}

func valueOrUnknown(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func roleInstructions(role string, input Input) []string {
	switch role {
	case RoleAuthor:
		if strings.TrimSpace(input.SessionPurpose) == "planning" || input.PlanMode {
			return []string{
				"Complete planning only. Do not commit, push, write a handoff, or implement the change in this session.",
				"Record the plan with flow status --kind plan, then wait for human approval or rejection.",
			}
		}
		return []string{
			"Implement the requested change in this worktree on branch ${FLOW_BRANCH:-the checked-out branch}.",
			"Finalize with two actions: (1) git commit your work with a conventional-commit message; (2) run flow ready, piping the handoff on stdin (e.g. a heredoc). flow ready pushes the branch, submits the handoff, and marks the change ready — do not push or write a handoff file separately.",
		}
	case RoleReviewer:
		return []string{
			"Review the issue and current branch against ${FLOW_BASE:-the base branch}.",
			"Record actionable blocking concerns as comments[] entries in $FLOW_VERDICT_FILE (each {sha,file,line,body}); the worker files each as a review thread. Use flow comment to file one directly instead if you prefer. Do not edit files, commit, push, certify threads, or call flow ready.",
			"Write the structured verdict to $FLOW_VERDICT_FILE as the source of truth. Exit 0 only when the reviewer check is satisfied; exit nonzero after filing blocking concerns or when review is unreliable, as the belt-and-braces fallback.",
		}
	case RoleVerifier:
		return []string{
			"Verify acceptance criteria and claimed review-thread resolutions against the current branch.",
			"Record certify/reopen decisions as threads[] entries in $FLOW_VERDICT_FILE (each {id,decision,body}; reopen requires a body); the worker applies each. Use flow thread certify and flow thread reopen --body to apply one directly instead if you prefer. Do not edit files, commit, push, or call flow ready.",
			"Write the structured verdict to $FLOW_VERDICT_FILE as the source of truth. Exit 0 only when verification is satisfied; exit nonzero when acceptance fails, claims are reopened, or verification is unreliable, as the belt-and-braces fallback.",
		}
	default:
		panic("unreachable role")
	}
}
