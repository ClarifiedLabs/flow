package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestDecodeConfigFileRejectsRemovedFlowFields(t *testing.T) {
	formats := []struct {
		name    string
		ext     string
		content func(alias, value string) string
	}{
		{
			name: "json", ext: "json",
			content: func(alias, value string) string {
				return fmt.Sprintf(`{"name":"strict","start_node":"done","nodes":[{"key":"done","name":"Done","kind":"terminal","config":{"terminal":{"resolution":"completed"}}}],"%s":%s}`, alias, value)
			},
		},
		{
			name: "yaml", ext: "yaml",
			content: func(alias, value string) string {
				return fmt.Sprintf("name: strict\nstart_node: done\nnodes:\n  - key: done\n    name: Done\n    kind: terminal\n    config:\n      terminal:\n        resolution: completed\n%s: %s\n", alias, value)
			},
		},
	}
	aliases := []struct {
		name  string
		value string
	}{
		{name: "phases", value: "[]"},
		{name: "review_agents", value: "[]"},
		{name: "fix_agent_def_id", value: `"legacy"`},
	}

	for _, format := range formats {
		for _, alias := range aliases {
			t.Run(format.name+"/"+alias.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "flow."+format.ext)
				if err := os.WriteFile(path, []byte(format.content(alias.name, alias.value)), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
				var input coordinator.FlowInput
				err := decodeConfigFile(path, &input)
				if err == nil || !strings.Contains(err.Error(), alias.name) {
					t.Fatalf("decode %s config with %q error = %v, want strict unknown-field rejection", format.name, alias.name, err)
				}
			})
		}
	}
}
