package handoff

import (
	"strings"
	"testing"
	"time"
)

func TestRenderTemplateIncludesRequiredSections(t *testing.T) {
	rendered := RenderTemplate(TemplateInput{
		IssueID:               "i-0001",
		ChangeID:              "ch-1",
		SessionID:             "s-1",
		Branch:                "issue/i-0001",
		Base:                  "main",
		UpdatedAt:             time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC),
		CurrentGoal:           "Finish handoff durability.",
		CompletedWork:         "Added template rendering.",
		RemainingWork:         "Wire ready validation.",
		TestsRun:              "go test ./internal/handoff",
		FailedApproaches:      "None.",
		ImportantFiles:        "internal/handoff/handoff.go",
		NextRecommendedAction: "Run focused tests.",
	})

	if err := Validate(rendered); err != nil {
		t.Fatalf("rendered template did not validate: %v\n%s", err, rendered)
	}
	for _, section := range RequiredSections {
		if !strings.Contains(rendered, "## "+section) {
			t.Fatalf("rendered template missing section %q:\n%s", section, rendered)
		}
	}
	if summary := Summary(rendered); summary != "Finish handoff durability." {
		t.Fatalf("summary = %q, want current goal", summary)
	}
	if !strings.HasSuffix(rendered, "\n") || strings.HasSuffix(rendered, "\n\n") {
		t.Fatalf("rendered template must end with exactly one newline:\n%q", rendered)
	}
}

func TestValidateRejectsMissingRequiredSection(t *testing.T) {
	err := Validate("# Flow Handoff\n\n## Current Goal\n\nGoal\n")
	if err == nil {
		t.Fatal("Validate accepted an incomplete handoff")
	}
	if !strings.Contains(err.Error(), "Completed Work") {
		t.Fatalf("error = %v, want missing Completed Work", err)
	}
}

func TestValidateRejectsPlaceholderOnlyTemplate(t *testing.T) {
	rendered := RenderTemplate(TemplateInput{})
	err := Validate(rendered)
	if err == nil {
		t.Fatal("Validate accepted a placeholder-only handoff")
	}
	if !strings.Contains(err.Error(), "non-placeholder content") {
		t.Fatalf("error = %v, want non-placeholder content error", err)
	}
}
