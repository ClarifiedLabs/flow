package execution

import (
	"testing"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
)

func TestResolveHarnessPrefersStoredKind(t *testing.T) {
	tests := []struct {
		name  string
		input tmuxInput
		want  string
	}{
		{
			name: "stored entrypoint harness beats misleading argv",
			input: tmuxInput{
				Entrypoint: Entrypoint{Harness: flowharness.Codex, Argv: []string{`/usr/local/bin/claude "$prompt"`}},
			},
			want: flowharness.Codex,
		},
		{
			name: "agent_harness payload fallback",
			input: tmuxInput{
				Payload:    JobPayload{AgentHarness: flowharness.Harness},
				Entrypoint: Entrypoint{Argv: []string{`custom-agent run`}},
			},
			want: flowharness.Harness,
		},
		{
			name: "console_harness payload fallback",
			input: tmuxInput{
				Payload:    JobPayload{ConsoleHarness: flowharness.Claude},
				Entrypoint: Entrypoint{Argv: []string{`custom-agent run`}},
			},
			want: flowharness.Claude,
		},
		{
			name: "entrypoint harness wins over payload harnesses",
			input: tmuxInput{
				Payload:    JobPayload{AgentHarness: flowharness.Codex, ConsoleHarness: flowharness.Codex},
				Entrypoint: Entrypoint{Harness: flowharness.Claude, Argv: []string{`codex "$prompt"`}},
			},
			want: flowharness.Claude,
		},
		{
			name: "agent_harness wins over console_harness",
			input: tmuxInput{
				Payload: JobPayload{AgentHarness: flowharness.Codex, ConsoleHarness: flowharness.Claude},
			},
			want: flowharness.Codex,
		},
		{
			name: "empty falls back to argv heuristic",
			input: tmuxInput{
				Entrypoint: Entrypoint{Argv: []string{`harness --hooks "$FLOW_HARNESS_HOOKS" -i "$prompt"`}},
			},
			want: flowharness.Harness,
		},
		{
			name: "empty argv falls back to default agent",
			input: tmuxInput{
				Entrypoint: Entrypoint{},
			},
			want: flowharness.DefaultAgentName(),
		},
		{
			name: "normalizes stored value",
			input: tmuxInput{
				Entrypoint: Entrypoint{Harness: "  Codex  "},
			},
			want: flowharness.Codex,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveHarness(test.input); got != test.want {
				t.Fatalf("resolveHarness() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestPromptConventionHarnessKeepsPromptDistinction verifies that the explicit
// prompt_harness payload (which may legitimately be "agents") wins over the
// stored agent harness, preserving the deliberate prompt-vs-agent distinction.
func TestPromptConventionHarnessKeepsPromptDistinction(t *testing.T) {
	got := promptConventionHarness(tmuxInput{
		Payload:    JobPayload{PromptHarness: flowharness.Agents, AgentHarness: flowharness.Codex},
		Entrypoint: Entrypoint{Harness: flowharness.Codex},
	})
	if got != flowharness.Agents {
		t.Fatalf("promptConventionHarness() = %q, want %q (prompt convention wins)", got, flowharness.Agents)
	}

	fallback := promptConventionHarness(tmuxInput{
		Payload:    JobPayload{AgentHarness: flowharness.Claude},
		Entrypoint: Entrypoint{Argv: []string{`custom-agent run`}},
	})
	if fallback != flowharness.Claude {
		t.Fatalf("promptConventionHarness() fallback = %q, want %q", fallback, flowharness.Claude)
	}
}
