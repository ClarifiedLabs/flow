package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderHookConfigMatchesGolden locks the unified renderer's JSON output to
// the byte-for-byte goldens captured from the pre-unification per-harness
// encoders, so claude and harness hook files never silently change shape.
func TestRenderHookConfigMatchesGolden(t *testing.T) {
	cases := []struct {
		name   string
		golden string
	}{
		{name: Claude, golden: "hooks_claude.json"},
		{name: Harness, golden: "hooks_harness.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := Lookup(tc.name)
			if !ok {
				t.Fatalf("lookup %q", tc.name)
			}
			got, err := RenderHookConfig(def)
			if err != nil {
				t.Fatalf("RenderHookConfig: %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("%s hook config mismatch:\n got:\n%s\nwant:\n%s", tc.name, got, want)
			}
		})
	}
}

// TestCodexInlineHookArgsRendersBroadenedEvents locks the inline `-c` fallback to
// the broadened codex event set, so the fallback and the managed profile (both
// driven by codexNativeHookEvents) can't silently diverge.
func TestCodexInlineHookArgsRendersBroadenedEvents(t *testing.T) {
	want := []string{
		"-c", "features.hooks=true",
		"-c", `hooks.SessionStart=[{hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
		"-c", `hooks.UserPromptSubmit=[{hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
		"-c", `hooks.PreToolUse=[{matcher="*",hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
		"-c", `hooks.PostToolUse=[{matcher="*",hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
		"-c", `hooks.PreCompact=[{hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
		"-c", `hooks.PostCompact=[{hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
		"-c", `hooks.PermissionRequest=[{matcher="*",hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
		"-c", `hooks.Stop=[{hooks=[{type="command",command="flow hook codex ingest",timeout=5}]}]`,
	}
	got := DefaultCodexNativeHookArgs()
	if len(got) != len(want) {
		t.Fatalf("inline args len = %d, want %d\n%#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("inline arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRenderHookConfigRejectsUnknownFormat ensures an unconfigured HookFormat is
// a loud error rather than a silently empty file.
func TestRenderHookConfigRejectsUnknownFormat(t *testing.T) {
	if _, err := RenderHookConfig(Definition{Name: "bogus", HookFormat: "yaml"}); err == nil {
		t.Fatal("RenderHookConfig accepted unsupported format")
	}
}

// TestHookEventsMapperParity is the anti-drift guard: every native-hook event a
// harness's generator emits must be explicitly classified by that harness's
// consumer-side mapper (HookState), so a new event can never be wired into the
// generated config without a corresponding liveness classification.
func TestHookEventsMapperParity(t *testing.T) {
	for _, name := range AgentNames() {
		def, ok := Lookup(name)
		if !ok {
			t.Fatalf("lookup %q", name)
		}
		if len(def.HookEvents) == 0 {
			continue
		}
		if def.HookState == nil {
			t.Fatalf("%s emits hook events but has no HookState mapper", name)
		}
		seen := map[string]bool{}
		for _, event := range def.HookEvents {
			if event.Name == "" {
				t.Fatalf("%s has a hook event with an empty name", name)
			}
			if seen[event.Name] {
				t.Fatalf("%s has duplicate hook event %q", name, event.Name)
			}
			seen[event.Name] = true
			if state := def.HookState(event.Name, ""); state == "" {
				t.Fatalf("%s hook event %q is unclassified by HookState", name, event.Name)
			}
		}
	}
}

// TestRenderHookConfigCodexProfileMatchesHookEvents renders codex's managed
// hook profile from the real definition and locks the structural contract behind
// the `codex --profile` mechanism without depending on a TOML parser in tests.
func TestRenderHookConfigCodexProfileMatchesHookEvents(t *testing.T) {
	def, ok := Lookup(Codex)
	if !ok {
		t.Fatal("lookup codex definition")
	}
	if def.HookFormat != "toml" {
		t.Fatalf("codex HookFormat = %q, want toml", def.HookFormat)
	}
	data, err := RenderHookConfig(def)
	if err != nil {
		t.Fatalf("RenderHookConfig codex: %v", err)
	}

	const header = "features.hooks = true\n\n[hooks]\n"
	rendered := string(data)
	if !strings.HasPrefix(rendered, header) {
		t.Fatalf("codex profile missing hooks feature/header:\n%s", data)
	}
	body := strings.TrimPrefix(rendered, header)
	body = strings.TrimSuffix(body, "\n")
	lines := []string{}
	if body != "" {
		lines = strings.Split(body, "\n")
	}
	if len(lines) != len(def.HookEvents) {
		t.Fatalf("codex profile has %d hook events, want %d:\n%s", len(lines), len(def.HookEvents), data)
	}
	for i, event := range def.HookEvents {
		want := event.Name + " = " + codexHookValue(def, event)
		if lines[i] != want {
			t.Fatalf("codex profile event line %d = %q, want %q\n%s", i, lines[i], want, data)
		}
	}
}
