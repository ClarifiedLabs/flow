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
		{name: "harness model and reasoning", harness: "harness", model: "anthropic:claude-opus-4-8", effort: "high", want: []string{"--model", "anthropic:claude-opus-4-8", "--reasoning", "high"}},
		{name: "harness model only", harness: "harness", model: "anthropic:claude-haiku-4-5", want: []string{"--model", "anthropic:claude-haiku-4-5"}},
		{name: "harness reasoning only", harness: "harness", effort: "max", want: []string{"--reasoning", "max"}},
		{name: "empty selection", harness: "harness", want: nil},
		{name: "whitespace trimmed", harness: "harness", model: " anthropic:claude-opus-4-8 ", effort: " high ", want: []string{"--model", "anthropic:claude-opus-4-8", "--reasoning", "high"}},
		{name: "unknown harness", harness: "shell", model: "x", wantErr: true},
		{name: "model with whitespace", harness: "harness", model: "opus 4.8", wantErr: true},
		{name: "effort with whitespace", harness: "harness", effort: "very high", wantErr: true},
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
