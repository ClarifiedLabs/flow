package prompt

import (
	"strings"
	"testing"
)

func TestBuildAuthorPromptInvokesRoleSkill(t *testing.T) {
	rendered, err := Build(Input{
		Role:                    RoleAuthor,
		IssueID:                 "i-0001",
		IssueTitle:              "Prompt includes issue details",
		IssueBody:               "Make the initial prompt self-contained.",
		IssueAcceptanceCriteria: "Agents do not need to immediately fetch issue details.",
		ChangeID:                "ch-1",
		Branch:                  "issue/i-0001",
		Base:                    "main",
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	for _, want := range []string{
		"Flow role instructions (flow-author):",
		"# Flow Author",
		"Finalize with two actions:",
		"Issue: i-0001",
		"Issue Title: Prompt includes issue details",
		"Issue Body:\nMake the initial prompt self-contained.",
		"Acceptance Criteria:\nAgents do not need to immediately fetch issue details.",
		"Change: ch-1",
		"Branch: issue/i-0001",
		"git commit",
		"flow ready",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("prompt missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "Use $flow-author") {
		t.Fatalf("prompt still points at an external skill:\n%s", rendered)
	}
	// The committed-handoff ritual is gone: flow ready owns the handoff, so the
	// author must not be told to write one separately.
	if strings.Contains(rendered, "flow handoff write") {
		t.Fatalf("author prompt still tells the agent to write a handoff file:\n%s", rendered)
	}
	if strings.Contains(rendered, ".handoff.md") {
		t.Fatalf("author prompt still references the dropped .handoff.md file:\n%s", rendered)
	}
}

func TestBuildInjectsPriorHandoffForAuthorAndVerifier(t *testing.T) {
	priorHandoff := "# Flow Handoff\n\n## Current Goal\nFinish the migration.\n"
	for _, role := range []string{RoleAuthor, RoleVerifier} {
		rendered, err := Build(Input{
			Role:         role,
			IssueID:      "i-0007",
			ChangeID:     "ch-7",
			Branch:       "issue/i-0007",
			Base:         "main",
			PriorHandoff: priorHandoff,
		})
		if err != nil {
			t.Fatalf("build %s prompt: %v", role, err)
		}
		if !strings.Contains(rendered, "Prior Handoff (from the previous session") {
			t.Fatalf("%s prompt missing prior handoff section:\n%s", role, rendered)
		}
		if !strings.Contains(rendered, "Finish the migration.") {
			t.Fatalf("%s prompt missing prior handoff body:\n%s", role, rendered)
		}
	}
}

func TestBuildOmitsPriorHandoffWhenAbsent(t *testing.T) {
	rendered, err := Build(Input{
		Role:     RoleAuthor,
		IssueID:  "i-0008",
		ChangeID: "ch-8",
		Branch:   "issue/i-0008",
		Base:     "main",
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}
	if strings.Contains(rendered, "Prior Handoff (from the previous session") {
		t.Fatalf("prompt included a prior handoff section when none was set:\n%s", rendered)
	}
}

func TestBuildAuthorFixRoundPromptIncludesBlockedReviewContext(t *testing.T) {
	exitCode := 1
	rendered, err := Build(Input{
		Role:        RoleAuthor,
		IssueID:     "i-0006",
		IssueTitle:  "Terminal link issue",
		ChangeID:    "ch-6",
		Branch:      "issue/i-0006",
		Base:        "main",
		ReviewState: "changes_requested",
		FixRound:    true,
		BlockedChecks: []BlockedCheck{{
			ID:          42,
			Name:        "merge-conflict",
			Kind:        "ci",
			Reporter:    "flow-merge",
			SourceJobID: "j-merge",
			ExitCode:    &exitCode,
			Details:     "branch conflicts with base main\nconflicting file: internal/worker/worker.go",
		}},
		ReviewThreads: []ReviewThread{{
			ID:        "th-1",
			State:     "reopened",
			FilePath:  "internal/worker/worker.go",
			Line:      128,
			Context:   "merge conflict markers remain",
			CreatedBy: "verifier",
			Comments: []ReviewComment{{
				Actor: "verifier",
				Body:  "Resolve the conflict against main before calling ready.",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	for _, want := range []string{
		"Round: fix/rework",
		"Current Review State:",
		"Review State: changes_requested",
		"This is a fix/rework round.",
		"Blocked Required Checks:",
		"- merge-conflict (Check ID: 42; Kind: ci; Reporter: flow-merge; Source Job: j-merge; Exit Code: 1)",
		"Details: branch conflicts with base main",
		"conflicting file: internal/worker/worker.go",
		"Open/Reopened Review Threads:",
		"- th-1 at internal/worker/worker.go:128 (State: reopened; Created By: verifier)",
		"Latest Comment by verifier: Resolve the conflict against main before calling ready.",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("prompt missing %q:\n%s", want, rendered)
		}
	}
}

func TestBuildAuthorPromptIncludesReviewCycleInstructions(t *testing.T) {
	rendered, err := Build(Input{
		Role:                    RoleAuthor,
		IssueID:                 "i-0007",
		IssueTitle:              "Recover repeated review loop",
		ReviewCycleInstructions: "Summarize why the loop happened before changing code.",
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	for _, want := range []string{
		"Human Recovery Instructions:",
		"Summarize why the loop happened before changing code.",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("prompt missing %q:\n%s", want, rendered)
		}
	}
}

func TestBuildAuthorPromptIncludesPlanModeInstructions(t *testing.T) {
	rendered, err := Build(Input{
		Role:       RoleAuthor,
		IssueID:    "i-0011",
		IssueTitle: "Plan mode",
		PlanMode:   true,
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	for _, want := range []string{
		"Plan Mode:",
		"Do not make code changes in this planning session.",
		"Create a plan and ask the user for approval.",
		"flow status --kind plan",
		"flow status --kind question",
		"Needs Attention",
		"separate implementation session",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("prompt missing %q:\n%s", want, rendered)
		}
	}
}

func TestBuildReviewerPromptUsesReviewerVerdictInstructions(t *testing.T) {
	rendered, err := Build(Input{
		Role:      RoleReviewer,
		IssueID:   "i-0002",
		ChangeID:  "ch-2",
		CheckName: "reviewer",
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	for _, want := range []string{
		"Flow role instructions (flow-reviewer):",
		"# Flow Reviewer",
		"Check: reviewer",
		"Use flow comment",
		"comments[] entries in $FLOW_VERDICT_FILE",
		"Exit 0 only when the reviewer check is satisfied",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("prompt missing %q:\n%s", want, rendered)
		}
	}
}

func TestBuildReviewerPromptIncludesCompletionAssessmentGuidance(t *testing.T) {
	rendered, err := Build(Input{
		Role:                 RoleReviewer,
		IssueID:              "i-0009",
		ChangeID:             "ch-9",
		CheckName:            "reviewer",
		CompletionAssessment: true,
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	for _, want := range []string{
		"Completion Assessment:",
		"ended without finalizing",
		"whether the task is actually complete",
		"pass this check",
		"block this check",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("completion-assessment prompt missing %q:\n%s", want, rendered)
		}
	}
}

func TestBuildOmitsCompletionAssessmentWhenUnset(t *testing.T) {
	rendered, err := Build(Input{
		Role:      RoleReviewer,
		IssueID:   "i-0010",
		ChangeID:  "ch-10",
		CheckName: "reviewer",
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}
	if strings.Contains(rendered, "Completion Assessment:") {
		t.Fatalf("reviewer prompt unexpectedly carried completion-assessment guidance:\n%s", rendered)
	}
}

func TestBuildOmitsCompletionAssessmentForNonReviewerRoles(t *testing.T) {
	// CompletionAssessment is a reviewer-only signal: an author or verifier
	// session that happens to carry the flag must not render the guidance.
	for _, role := range []string{RoleAuthor, RoleVerifier} {
		rendered, err := Build(Input{
			Role:                 role,
			IssueID:              "i-0011",
			ChangeID:             "ch-11",
			CompletionAssessment: true,
		})
		if err != nil {
			t.Fatalf("build %s prompt: %v", role, err)
		}
		if strings.Contains(rendered, "Completion Assessment:") {
			t.Fatalf("%s prompt unexpectedly carried completion-assessment guidance:\n%s", role, rendered)
		}
	}
}

func TestBuildVerifierPromptUsesVerifierThreadInstructions(t *testing.T) {
	rendered, err := Build(Input{
		Role:      RoleVerifier,
		IssueID:   "i-0003",
		ChangeID:  "ch-3",
		CheckName: "verifier",
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	for _, want := range []string{
		"Flow role instructions (flow-verifier):",
		"# Flow Verifier",
		"flow thread certify",
		"flow thread reopen --body",
		"threads[] entries in $FLOW_VERDICT_FILE",
		"Exit 0 only when verification is satisfied",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("prompt missing %q:\n%s", want, rendered)
		}
	}
}

func TestBuildRejectsUnsupportedRole(t *testing.T) {
	for _, role := range []string{"ci", "console"} {
		if _, err := Build(Input{Role: role}); err == nil {
			t.Fatalf("Build accepted unsupported role %q", role)
		}
	}
}

func TestInputFromEnvironmentPrefersWorkerRole(t *testing.T) {
	env := map[string]string{
		"FLOW_WORKER_ROLE": "reviewer",
		"FLOW_ROLE":        "author",
		"FLOW_ISSUE_ID":    "i-0004",
	}
	input := InputFromEnvironment(func(key string) string {
		return env[key]
	})

	if input.Role != RoleReviewer || input.IssueID != "i-0004" {
		t.Fatalf("input = %+v", input)
	}
}
