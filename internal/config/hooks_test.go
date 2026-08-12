package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHooks(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		hooks   []HookConfig
		wantErr string
	}{
		{
			name:  "valid exact and glob",
			hooks: []HookConfig{{Events: []string{"task.done", "epic.*"}, Command: []string{"/bin/sh", "-c", "true"}}},
		},
		{
			name:    "no events",
			hooks:   []HookConfig{{Command: []string{"/bin/sh"}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "empty pattern",
			hooks:   []HookConfig{{Events: []string{"task.done", "  "}, Command: []string{"/bin/sh"}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "bare glob",
			hooks:   []HookConfig{{Events: []string{"*"}, Command: []string{"/bin/sh"}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "interior glob",
			hooks:   []HookConfig{{Events: []string{"task.*.done"}, Command: []string{"/bin/sh"}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "glob without prefix",
			hooks:   []HookConfig{{Events: []string{".*"}, Command: []string{"/bin/sh"}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "no command",
			hooks:   []HookConfig{{Events: []string{"task.done"}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "blank command",
			hooks:   []HookConfig{{Events: []string{"task.done"}, Command: []string{" "}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "relative command",
			hooks:   []HookConfig{{Events: []string{"task.done"}, Command: []string{"bin/sh"}}},
			wantErr: "absolute path",
		},
		{
			name:    "missing executable",
			hooks:   []HookConfig{{Events: []string{"task.done"}, Command: []string{filepath.Join(dir, "nope")}}},
			wantErr: "hooks[0]",
		},
		{
			name:    "directory executable",
			hooks:   []HookConfig{{Events: []string{"task.done"}, Command: []string{dir}}},
			wantErr: "is a directory",
		},
		{
			name: "second hook named in error",
			hooks: []HookConfig{
				{Events: []string{"task.done"}, Command: []string{"/bin/sh"}},
				{Events: []string{"task.done"}, Command: []string{"relative"}},
			},
			wantErr: "hooks[1]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHooks(tc.hooks)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateHooks: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateHooks err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
	if err := ValidateHooks(nil); err != nil {
		t.Fatalf("nil hooks must validate: %v", err)
	}
}

func TestLoadCoordinatorCarriesHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"hooks":[{"events":["task.done"],"command":["/bin/sh","-c","true"]}]}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadCoordinator(path)
	if err != nil {
		t.Fatalf("LoadCoordinator: %v", err)
	}
	if len(cfg.Hooks) != 1 || cfg.Hooks[0].Events[0] != "task.done" || cfg.Hooks[0].Command[0] != "/bin/sh" {
		t.Fatalf("hooks = %+v", cfg.Hooks)
	}
	if err := ValidateHooks(cfg.Hooks); err != nil {
		t.Fatalf("loaded hooks must validate: %v", err)
	}
}
