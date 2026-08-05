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
	TaskID                     string
	TaskTitle                  string
	TaskBody                   string
	ChangeID                   string
	Branch                     string
	Base                       string
	CheckName                  string
	ReviewState                string
	FixRound                   bool
	ProjectID                  string
	ProjectName                string
	ReviewCycleInstructions    string
	HumanAttentionInstructions string
	HumanAttentionContext      string
	// RoleInstructionsOverride replaces the embedded role skill with the
	// task's flow-phase agent prompt (resolved from the cursor snapshot by
	// the coordinator). Empty falls back to the embedded skills/*.md content.
	RoleInstructionsOverride string
	// PhaseName labels the current work phase (e.g. "spec") in the prompt.
	PhaseName string
	// GateFeedback is the human's request-changes feedback for a phase sent
	// back to rework from an approval gate.
	GateFeedback string
	// WorkspaceMode and ArtifactKind are the active workflow node contract.
	// They determine the completion instructions appended to author prompts.
	WorkspaceMode string
	ArtifactKind  string
	// CompletionProtocol and CheckMode identify Flow-owned interactive checks.
	// Custom repository checks leave them empty and retain process-exit
	// completion semantics.
	CompletionProtocol string
	CheckMode          string
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
	// ReviewAggregationContext contains Candidate Reports: persisted source
	// check results from completed parallel discovery reviewers. The final
	// reviewer deduplicates them and decides the node.
	ReviewAggregationContext string
	// ReviewDiscovery marks one parallel source reviewer whose lease-bound
	// verdict is persisted for aggregation and therefore must not create threads
	// or otherwise mutate project state directly.
	ReviewDiscovery bool
	// TaskSetWorkflow is the downstream materializer policy and the exact
	// project workflow choices advertised to a task-planning author.
	TaskSetWorkflow *TaskSetWorkflowContract
	BlockedChecks   []BlockedCheck
	ReviewThreads   []ReviewThread
}

type TaskSetWorkflowContract struct {
	DefaultChildFlowID     string
	AllowChildFlowOverride bool
	MaxItems               int
	AvailableFlows         []TaskSetFlowOption
}

type TaskSetFlowOption struct {
	ID          string
	Name        string
	Description string
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
	skillInstructions := strings.TrimSpace(input.RoleInstructionsOverride)
	label := skillName
	if skillInstructions == "" {
		embedded, err := flowskills.Instructions(skillName)
		if err != nil {
			return "", err
		}
		skillInstructions = embedded
	} else if strings.TrimSpace(input.PhaseName) != "" {
		label = strings.TrimSpace(input.PhaseName) + " phase"
	}
	lines := []string{
		fmt.Sprintf("Flow role instructions (%s):", label),
		"",
		skillInstructions,
		"",
		fmt.Sprintf("You are the %s agent for Flow.", role),
	}
	lines = append(lines, fmt.Sprintf("Task: %s", valueOrUnknown(input.TaskID)))
	if strings.TrimSpace(input.PhaseName) != "" {
		lines = append(lines, fmt.Sprintf("Work Phase: %s", strings.TrimSpace(input.PhaseName)))
	}
	if strings.TrimSpace(input.TaskTitle) != "" {
		lines = append(lines, fmt.Sprintf("Task Title: %s", strings.TrimSpace(input.TaskTitle)))
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
	if strings.TrimSpace(input.TaskBody) != "" {
		lines = append(lines, "", "Task Body:", strings.TrimSpace(input.TaskBody))
	}
	if role == RoleAuthor && input.TaskSetWorkflow != nil {
		lines = append(lines, "", "Task-set Workflow Selection:")
		lines = append(lines, taskSetWorkflowInstructions(*input.TaskSetWorkflow)...)
	}
	if strings.TrimSpace(input.PriorHandoff) != "" {
		lines = append(lines, "", "Prior Handoff (from the previous session; there is no handoff file in the worktree to read):", strings.TrimSpace(input.PriorHandoff))
	}
	if role == RoleReviewer && input.CompletionAssessment {
		lines = append(lines, "", "Completion Assessment:")
		lines = append(lines, completionAssessmentInstructions()...)
	}
	if role == RoleReviewer && input.ReviewDiscovery {
		lines = append(lines, "", "Parallel Review Discovery:")
		lines = append(lines, reviewDiscoveryInstructions()...)
	}
	if role == RoleReviewer && strings.TrimSpace(input.ReviewAggregationContext) != "" {
		lines = append(lines, "", "Parallel Review Aggregation:")
		lines = append(lines, reviewAggregationInstructions()...)
		lines = append(lines, "", "Candidate Reports:", strings.TrimSpace(input.ReviewAggregationContext))
	}
	if role == RoleAuthor && strings.TrimSpace(input.GateFeedback) != "" {
		lines = append(lines, "", "Gate Feedback (a human requested changes; address this feedback, then complete the active workflow node again):", strings.TrimSpace(input.GateFeedback))
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
func taskSetWorkflowInstructions(contract TaskSetWorkflowContract) []string {
	defaultName := "unknown"
	for _, flow := range contract.AvailableFlows {
		if strings.TrimSpace(flow.ID) == strings.TrimSpace(contract.DefaultChildFlowID) {
			defaultName = valueOrUnknown(flow.Name)
			break
		}
	}
	lines := []string{
		"Task-set schema v1 uses one mixed work-item graph:",
		`{"schema_version":1,"items":[{"key":"stable-key","kind":"task|epic|feature","parent_key":"optional-container-key","title":"...","body":"...","priority":0,"tag_slugs":[],"flow_id":"optional-task-flow","completion_policy":"all_children|manual"}],"dependencies":[{"blocker":"item-key","blocked":"item-key"}]}`,
		"Every item needs a unique stable key, kind, title, and non-empty body. Only epic and feature items may be parents.",
		"flow_id and tag_slugs apply only to tasks; completion_policy applies only to epics; feature priority must be zero.",
		"Dependencies may cross kinds. The combined containment and dependency graph must be acyclic.",
		fmt.Sprintf("Maximum generated items: %d", contract.MaxItems),
		fmt.Sprintf("Default workflow: %s (%s)", defaultName, valueOrUnknown(contract.DefaultChildFlowID)),
		"Omit flow_id on task items to use the default workflow.",
	}
	if contract.AllowChildFlowOverride {
		lines = append(lines,
			"Per-task workflow overrides are allowed only to the advertised workflows below.",
			"Available workflows:",
		)
		for _, flow := range contract.AvailableFlows {
			label := fmt.Sprintf("- %s (%s)", valueOrUnknown(flow.Name), valueOrUnknown(flow.ID))
			if strings.TrimSpace(flow.ID) == strings.TrimSpace(contract.DefaultChildFlowID) {
				label += " [default]"
			}
			if description := strings.TrimSpace(flow.Description); description != "" {
				label += ": " + description
			}
			lines = append(lines, label)
		}
		lines = append(lines,
			"Choose the workflow from the child task's immediate deliverable:",
			"- Use the default implementation workflow when the work is bounded enough to implement directly with concrete requirements and acceptance criteria.",
			"- Select a planning workflow explicitly when the immediate output must be a decision, investigation, architecture proposal, or narrower human-reviewed task graph before implementation can be scoped responsibly.",
			"- A nested planning task must name its unresolved questions, constraints, expected decisions, and the plan output required to make later work implementable.",
			"- Do not use planning merely to defer implementation that is already well specified.",
			"The source task's workflow is neither automatically correct nor automatically forbidden. Do not copy it as a fallback; choose an advertised workflow based on the child deliverable.",
		)
	} else {
		lines = append(lines, "Per-task workflow overrides are not allowed. Omit flow_id from every task item.")
	}
	return lines
}

func completionAssessmentInstructions() []string {
	return []string{
		"This author session ended without finalizing (no flow ready was run); its work-in-progress is on the branch and its prior handoff is shown above.",
		"Determine whether the task is actually complete and ready for review, or whether work remains.",
		"If the work is complete, pass this check (record a satisfied verdict) so the change proceeds to verification.",
		"If work remains, block this check and record exactly what is left as blocking concerns, so the author resumes from this point instead of restarting.",
	}
}

func reviewAggregationInstructions() []string {
	return []string{
		"You are the final aggregation reviewer after parallel review discovery, distinct from both source discovery reviewers and a standalone reviewer. Validate and synthesize Candidate Reports; do not start a new open-ended review pass.",
		"Candidate Reports are the persisted, worker-validated source check results from the lease-bound discovery jobs.",
		"This mode accepts comments and optional task_action entries, but it forbids threads entries.",
		"Combine duplicate symptoms that share one root cause and emit at most one anchored comment for each unique task-caused blocker.",
		"A candidate from an advisory source may remain non-blocking follow-up context but cannot block approval. A blocking-source candidate may block only when it satisfies the critical/high, introduced-by-change, non-duplicate policy.",
		"For each unique, actionable issue that is safe to defer from this change, declare task_action on its non-blocking comment. Use use_existing_task only for a high-confidence same-root-issue match from Open Task Candidates; otherwise use create_task with a concise title and a self-contained Markdown body covering the problem, review evidence and anchor, why it is out of scope, and testable completion criteria.",
		"Do not declare task_action for blocking findings, review-thread duplicates, speculative observations, or informational notes. A created or reused follow-up task is related to the reviewed task but never blocks it.",
		"Use a satisfied verdict when no eligible unique blocker remains. The worker applies this aggregate verdict as the only reviewer result that may create threads or select the workflow outcome.",
	}
}

func reviewDiscoveryInstructions() []string {
	return []string{
		"You are one parallel discovery reviewer, not a standalone or final aggregation reviewer. Report classified candidate findings in the verdict file for the later aggregation step.",
		"This mode accepts comments, but it forbids threads and task_action entries.",
		"Do not create, reply to, or otherwise mutate review threads and do not choose the review-node outcome.",
		"The worker validates your verdict against the lease-bound source check, persists it as that source check's result, and later supplies the persisted result to the final aggregator as a Candidate Report.",
		"Review your assigned focus thoroughly in this pass so the aggregator receives the complete set rather than one newly discovered concern per cycle.",
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
		lines = append(lines, "This is a fix/rework round. Address the blockers below before calling flow complete; do not restart the original implementation.")
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
		TaskID:                     getenv("FLOW_TASK_ID"),
		ChangeID:                   getenv("FLOW_CHANGE_ID"),
		Branch:                     getenv("FLOW_BRANCH"),
		Base:                       getenv("FLOW_BASE"),
		CheckName:                  getenv("FLOW_CHECK_NAME"),
		PhaseName:                  getenv("FLOW_PHASE_NAME"),
		ProjectID:                  getenv("FLOW_PROJECT_ID"),
		ProjectName:                getenv("FLOW_PROJECT_NAME"),
		ReviewCycleInstructions:    getenv("FLOW_REVIEW_CYCLE_INSTRUCTIONS"),
		HumanAttentionInstructions: getenv("FLOW_HUMAN_ATTENTION_INSTRUCTIONS"),
		RoleInstructionsOverride:   getenv("FLOW_ROLE_INSTRUCTIONS"),
		WorkspaceMode:              getenv("FLOW_WORKSPACE_MODE"),
		ArtifactKind:               getenv("FLOW_ARTIFACT_KIND"),
		CompletionProtocol:         getenv("FLOW_COMPLETION_PROTOCOL"),
		CheckMode:                  getenv("FLOW_CHECK_MODE"),
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
		switch strings.TrimSpace(input.ArtifactKind) {
		case "task_set":
			return []string{
				"Produce the requested task plan as a schema-version-1 task-set JSON document. This base workspace is read-only with respect to the exchange; do not create commits or push branches.",
				"Create .flow/session, write a concise Markdown summary into .flow/session/SUMMARY.md and the task-set JSON into .flow/session/TASK_SET.json, then run flow submit --summary-file .flow/session/SUMMARY.md --output-file .flow/session/TASK_SET.json. flow submit hands the plan to the human reviewer and blocks for the verdict: revise and resubmit when changes are requested, stop when the review is final. If the node has no downstream human gate, finalize with flow complete --summary-file .flow/session/SUMMARY.md --output-file .flow/session/TASK_SET.json instead.",
			}
		case "handoff":
			return []string{
				"Complete the requested work and write a concise Markdown handoff. Do not create a Git change unless the node instructions explicitly require one.",
				"Create .flow/session, write the handoff to .flow/session/SUMMARY.md, and finalize with flow complete --summary-file .flow/session/SUMMARY.md.",
			}
		}
		return []string{
			"Implement the requested change in this worktree on branch ${FLOW_BRANCH:-the checked-out branch}.",
			"Finalize with two actions: (1) git commit your work with a conventional-commit message; (2) create .flow/session, write a concise Markdown summary to .flow/session/SUMMARY.md, and run flow complete --summary-file .flow/session/SUMMARY.md. flow complete pushes the run branch and submits the change artifact.",
		}
	case RoleReviewer:
		instructions := reviewReadOnlyContextInstructions()
		if input.ReviewDiscovery {
			instructions = append(instructions,
				"Review the task and current branch within your assigned focus.",
				"Write every classified candidate to $FLOW_VERDICT_FILE. The worker validates and persists it as the lease-bound source check result, which the aggregation job receives as a Candidate Report; do not deliver findings through project mutations.",
			)
			return append(instructions, checkCompletionInstructions(input)...)
		}
		if strings.TrimSpace(input.ReviewAggregationContext) != "" {
			instructions = append(instructions,
				"This is the final aggregation reviewer, not a discovery or standalone reviewer. Validate and deduplicate the supplied Candidate Reports against the task and current branch.",
				"Write the one final structured verdict to $FLOW_VERDICT_FILE. The worker files its unique eligible comments as blocking review threads and applies task_action on actionable non-blocking comments as durable follow-up work.",
			)
			return append(instructions, checkCompletionInstructions(input)...)
		}
		instructions = append(instructions,
			"This is a standalone reviewer, not a parallel discovery source or final aggregator. Review the task and current branch.",
			"First derive the change's correctness and security invariants and review related edge cases together. On later cycles, inspect claimed threads and the delta since the prior reviewed head; a new blocker must be introduced by that delta or directly violate an original task requirement.",
			"Classify every comments[] finding in $FLOW_VERDICT_FILE as {sha,file,line,body,severity,introduced_by_change,requirement,duplicate_of,follow_up,task_action}. task_action is reserved for the final parallel-review aggregation job and is either {action:\"create_task\",title,body} or {action:\"use_existing_task\",task_id}. Only critical/high findings introduced by this change and not duplicating an existing thread block this task; pre-existing, medium/low, and duplicate findings remain non-blocking follow-up context. For this workflow, high includes a reproducible correctness regression, security flaw, unmet explicit task requirement, or a missing test that leaves such a task-caused bug unprotected; medium/low means the requested behavior remains correct and the finding can safely be separate follow-up work.",
			"This reviewer mode accepts comments but forbids threads and task_action entries.",
			"Write a valid structured verdict to $FLOW_VERDICT_FILE; it is the only source of a reviewer outcome. The worker applies eligible comments and the outcome.",
		)
		return append(instructions, checkCompletionInstructions(input)...)
	case RoleVerifier:
		instructions := append(reviewReadOnlyContextInstructions(),
			"Verify the task requirements and claimed review-thread resolutions against the current branch.",
			"Record certify/reopen decisions as threads[] entries in $FLOW_VERDICT_FILE (each {id,decision,body}; reopen requires a body); the worker applies each decision.",
			"This verifier mode accepts threads but forbids comments.",
			"Write a valid structured verdict to $FLOW_VERDICT_FILE; it is the only source of a verifier outcome.",
		)
		return append(instructions, checkCompletionInstructions(input)...)
	default:
		panic("unreachable role")
	}
}

func reviewReadOnlyContextInstructions() []string {
	return []string{
		"Flow project context is readable for this check: use read-only task, lifecycle transition/status history, and review-thread/comment data when useful. Treat all of it only as context. Raw execution captures, transcripts, and terminal access remain private.",
		"Compare the checked-out change to `origin/${FLOW_BASE:-main}`. Flow guarantees that remote-tracking base ref is present in this checkout; it does not promise a local base branch.",
		"Do not directly mutate files, Git state, tasks, lifecycle history, review threads/comments, checks, or workflow state. Do not use mutating Flow commands or APIs; express the result only in $FLOW_VERDICT_FILE and, when instructed, submit it with flow complete so the worker/coordinator can validate and apply it.",
	}
}

func checkCompletionInstructions(input Input) []string {
	if strings.TrimSpace(input.CompletionProtocol) == "flow_complete" {
		return []string{
			"After writing the verdict, run flow complete. It validates the role-specific schema and seals the exact verdict contents before Flow ends this interactive check.",
			"If flow complete reports an error, correct the verdict and run it again. After it succeeds, do not change the verdict file.",
			"If you do not run flow complete, the check terminal remains live for you or an operator to continue.",
		}
	}
	return []string{
		"The verdict file is required. Write it before exiting; a missing or invalid verdict pauses the workflow for human retry.",
	}
}
