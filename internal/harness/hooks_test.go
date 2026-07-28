package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRenderHookConfigMatchesGolden locks the renderer's JSON output to the
// byte-for-byte golden, so the harness hook file never silently changes shape.
func TestRenderHookConfigMatchesGolden(t *testing.T) {
	def, ok := Lookup(Harness)
	if !ok {
		t.Fatalf("lookup %q", Harness)
	}
	got, err := RenderHookConfig(def)
	if err != nil {
		t.Fatalf("RenderHookConfig: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "hooks_harness.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("hook config mismatch:\n got:\n%s\nwant:\n%s", got, want)
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
