package harness

import (
	"reflect"
	"testing"
)

// TestSerializeModelSelection mirrors the serializeHarnessModelSelection table
// in internal/web/assets/harness_models.test.mjs: the Go and JS serializers
// must emit identical argv tokens for the same selection.
func TestSerializeModelSelection(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		model   string
		effort  string
		want    []string
		wantErr bool
	}{
		{name: "claude model and effort", harness: "claude", model: "claude-opus-4-8", effort: "xhigh", want: []string{"--model", "claude-opus-4-8", "--effort", "xhigh"}},
		{name: "claude model only", harness: "claude", model: "claude-haiku-4-5", want: []string{"--model", "claude-haiku-4-5"}},
		{name: "claude effort only", harness: "claude", effort: "max", want: []string{"--effort", "max"}},
		{name: "codex model and effort", harness: "codex", model: "gpt-5.5", effort: "high", want: []string{"--model", "gpt-5.5", "-c", "model_reasoning_effort=high"}},
		{name: "codex effort only", harness: "codex", effort: "low", want: []string{"-c", "model_reasoning_effort=low"}},
		{name: "harness model and reasoning", harness: "harness", model: "anthropic:claude-opus-4-8", effort: "high", want: []string{"--model", "anthropic:claude-opus-4-8", "--reasoning", "high"}},
		{name: "empty selection", harness: "claude", want: nil},
		{name: "whitespace trimmed", harness: "claude", model: " claude-opus-4-8 ", effort: " high ", want: []string{"--model", "claude-opus-4-8", "--effort", "high"}},
		{name: "unknown harness", harness: "shell", model: "x", wantErr: true},
		{name: "model with whitespace", harness: "claude", model: "opus 4.8", wantErr: true},
		{name: "effort with whitespace", harness: "claude", effort: "very high", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SerializeModelSelection(tc.harness, tc.model, tc.effort)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SerializeModelSelection: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
