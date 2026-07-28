package harness

import (
	"strings"
	"testing"
)

func TestResolveAgentSelectionDefaultsToHarness(t *testing.T) {
	t.Parallel()
	resolved, err := ResolveAgentSelection(AgentSelection{})
	if err != nil {
		t.Fatalf("resolve empty selection: %v", err)
	}
	want := DefaultAgentSelection()
	if resolved != want {
		t.Fatalf("resolved = %+v, want %+v", resolved, want)
	}
	if resolved.Harness != Harness {
		t.Fatalf("resolved harness = %q, want %q", resolved.Harness, Harness)
	}
}

func TestResolveAgentSelectionNormalizesAndTrims(t *testing.T) {
	t.Parallel()
	resolved, err := ResolveAgentSelection(AgentSelection{
		Harness:         " Harness ",
		Model:           " anthropic:claude-sonnet-4-6 ",
		ReasoningEffort: " high ",
	})
	if err != nil {
		t.Fatalf("resolve selection: %v", err)
	}
	want := AgentSelection{Harness: Harness, Model: "anthropic:claude-sonnet-4-6", ReasoningEffort: "high"}
	if resolved != want {
		t.Fatalf("resolved = %+v, want %+v", resolved, want)
	}
}

func TestResolveAgentSelectionRejectsInvalidHarness(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"bogus", Agents, Shell} {
		if _, err := ResolveAgentSelection(AgentSelection{Harness: harness}); err == nil {
			t.Fatalf("resolve harness %q: expected error", harness)
		}
	}
}

func TestResolveAgentSelectionRejectsWhitespaceModel(t *testing.T) {
	t.Parallel()
	if _, err := ResolveAgentSelection(AgentSelection{Model: "gpt 5"}); err == nil {
		t.Fatal("resolve whitespace model: expected error")
	}
	if _, err := ResolveAgentSelection(AgentSelection{ReasoningEffort: "very high"}); err == nil {
		t.Fatal("resolve whitespace effort: expected error")
	}
}

func TestAgentSelectionModelArgs(t *testing.T) {
	t.Parallel()

	t.Run("empty selection yields no tokens", func(t *testing.T) {
		t.Parallel()
		args, err := DefaultAgentSelection().ModelArgs()
		if err != nil {
			t.Fatalf("model args: %v", err)
		}
		if len(args) != 0 {
			t.Fatalf("model args = %#v, want empty", args)
		}
	})

	t.Run("model without harness applies to the default harness", func(t *testing.T) {
		t.Parallel()
		resolved, err := ResolveAgentSelection(AgentSelection{Model: "anthropic:claude-sonnet-4-6", ReasoningEffort: "high"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		args, err := resolved.ModelArgs()
		if err != nil {
			t.Fatalf("model args: %v", err)
		}
		want := []string{"--model", "anthropic:claude-sonnet-4-6", "--reasoning", "high"}
		if strings.Join(args, " ") != strings.Join(want, " ") {
			t.Fatalf("model args = %#v, want %#v", args, want)
		}
	})

	t.Run("harness target serializes as --model/--reasoning", func(t *testing.T) {
		t.Parallel()
		args, err := AgentSelection{Harness: Harness, Model: "anthropic:claude-sonnet-4", ReasoningEffort: "max"}.ModelArgs()
		if err != nil {
			t.Fatalf("model args: %v", err)
		}
		want := []string{"--model", "anthropic:claude-sonnet-4", "--reasoning", "max"}
		if strings.Join(args, " ") != strings.Join(want, " ") {
			t.Fatalf("model args = %#v, want %#v", args, want)
		}
	})
}
