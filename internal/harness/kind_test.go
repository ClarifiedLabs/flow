package harness

import (
	"sort"
	"testing"
)

func TestKindStringConstsMatch(t *testing.T) {
	pairs := []struct {
		str  string
		kind Kind
	}{
		{Harness, KindHarness},
		{Agents, KindAgents},
		{Shell, KindShell},
	}
	for _, pair := range pairs {
		if pair.str != string(pair.kind) {
			t.Fatalf("string const %q != Kind %q", pair.str, string(pair.kind))
		}
	}
}

func TestDefinitionKindMatchesName(t *testing.T) {
	for _, name := range AgentNames() {
		definition, ok := Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) missing", name)
		}
		if string(definition.Kind) != definition.Name {
			t.Fatalf("definition %q kind = %q, want kind matching name", definition.Name, definition.Kind)
		}
	}
}

// TestDefaultAgentCheckCommandMatchesBuilders locks the table-driven
// DefaultAgentCheckCommandWithArgs to exactly the string the harness's
// CheckCommand builder returns.
func TestDefaultAgentCheckCommandMatchesBuilders(t *testing.T) {
	args := []string{"--model", "fast"}
	got, err := DefaultAgentCheckCommandWithArgs(Harness, args)
	if err != nil {
		t.Fatalf("DefaultAgentCheckCommandWithArgs(%q): %v", Harness, err)
	}
	if want := DefaultHarnessInteractiveCheckCommandWithArgs(args); got != want {
		t.Fatalf("DefaultAgentCheckCommandWithArgs(%q) =\n%s\nwant\n%s", Harness, got, want)
	}
	if _, err := DefaultAgentCheckCommandWithArgs(Shell, args); err == nil {
		t.Fatal("DefaultAgentCheckCommandWithArgs(shell) err = nil, want error")
	}
}

// TestManagedArgValidationGolden proves the table-driven managed-flag
// validation accepts/rejects exactly the intended inputs.
func TestManagedArgValidationGolden(t *testing.T) {
	reject := [][]string{
		{"--hooks", "/tmp/h.json"},
		{"--hooks=/tmp/h.json"},
		{"-p", "prompt"},
		{"--prompt", "prompt"},
		{"-i", "prompt"},
		{"--initial-prompt", "prompt"},
	}
	for _, args := range reject {
		if _, err := NormalizeArgs(args); err == nil {
			t.Fatalf("NormalizeArgs(%+v) err = nil, want rejection", args)
		}
	}

	accept := [][]string{
		{"--provider", "anthropic", "--model", "claude-sonnet-4-6"},
		{"--profile", "review"},
	}
	for _, args := range accept {
		if _, err := NormalizeArgs(args); err != nil {
			t.Fatalf("NormalizeArgs(%+v) err = %v, want accept", args, err)
		}
	}
}

// TestNativeHookMappingGolden asserts the HookState wiring on the harness
// Definition classifies events as intended.
func TestNativeHookMappingGolden(t *testing.T) {
	cases := []struct {
		event string
		want  string
	}{
		{"SessionStart", StateWorking},
		{"UserPromptSubmit", StateWorking},
		{"PreToolUse", StateWorking},
		{"Stop", StateWaiting},
		{"PostToolUse", SignalActivity},
		{"PreCompact", SignalActivity},
		{"PostCompact", SignalActivity},
	}
	definition, ok := Lookup(Harness)
	if !ok || definition.HookState == nil {
		t.Fatalf("definition %q missing HookState", Harness)
	}
	for _, test := range cases {
		got := definition.HookState(test.event)
		if got == "" {
			got = SignalActivity // ParseNativeHook applies the default
		}
		if got != test.want {
			t.Fatalf("HookState(%q) = %q, want %q", test.event, got, test.want)
		}
	}
}

// TestHookEventsParityWithHookState ensures every declared HookEvent maps to an
// explicit (non-default) classification, so the renderer's event table stays in
// lockstep with the runtime classifier.
func TestHookEventsParityWithHookState(t *testing.T) {
	for _, name := range AgentNames() {
		definition, ok := Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) missing", name)
		}
		if len(definition.HookEvents) == 0 {
			t.Fatalf("definition %q declares no HookEvents", name)
		}
		if definition.HookState == nil {
			t.Fatalf("definition %q has no HookState mapper", name)
		}
		for _, event := range definition.HookEvents {
			if mapped := definition.HookState(event.Name); mapped == "" {
				t.Fatalf("definition %q HookEvent %q falls through to the default mapping", name, event.Name)
			}
		}
	}
}

func TestHookEventsDataMatchesGenerators(t *testing.T) {
	definition, ok := Lookup(Harness)
	if !ok {
		t.Fatalf("Lookup(%q) missing", Harness)
	}
	if len(definition.HookEvents) != 7 {
		t.Fatalf("HookEvents count = %d, want 7", len(definition.HookEvents))
	}
	if definition.HookEnvVar != "FLOW_HARNESS_HOOKS" {
		t.Fatalf("HookEnvVar = %q, want FLOW_HARNESS_HOOKS", definition.HookEnvVar)
	}
	byName := map[string]string{}
	for _, event := range definition.HookEvents {
		byName[event.Name] = event.Matcher
	}
	for _, want := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PreCompact", "PostCompact", "Stop"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("HookEvents missing %q: %+v", want, definition.HookEvents)
		}
	}
	for event, matcher := range map[string]string{"PreToolUse": "*", "PostToolUse": "*"} {
		if byName[event] != matcher {
			t.Fatalf("HookEvent %q matcher = %q, want %q", event, byName[event], matcher)
		}
	}
}

func TestDetectEntrypointHarnessDeterministic(t *testing.T) {
	// A token mentioning the harness executable resolves to the harness; an
	// unrelated token falls back to the agents pseudo-harness.
	argv := []string{"env FOO=1 harness run"}
	for i := 0; i < 8; i++ {
		if got := DetectEntrypointHarness(argv); got != Harness {
			t.Fatalf("DetectEntrypointHarness(%v) = %q, want %q (deterministic)", argv, got, Harness)
		}
	}
}

func TestDefaultEntrypointsStampHarness(t *testing.T) {
	author, err := DefaultAuthorEntrypoint(Harness)
	if err != nil {
		t.Fatalf("DefaultAuthorEntrypoint(harness): %v", err)
	}
	if author["harness"] != Harness {
		t.Fatalf("author harness = %#v, want %q", author["harness"], Harness)
	}

	console, err := DefaultConsoleEntrypointWithArgs(Harness, nil)
	if err != nil {
		t.Fatalf("DefaultConsoleEntrypointWithArgs(harness): %v", err)
	}
	if console["harness"] != Harness {
		t.Fatalf("console harness = %#v, want %q", console["harness"], Harness)
	}

	shell, err := DefaultConsoleEntrypointWithArgs(Shell, nil)
	if err != nil {
		t.Fatalf("DefaultConsoleEntrypointWithArgs(shell): %v", err)
	}
	if shell["harness"] != Shell {
		t.Fatalf("shell console harness = %#v, want %q", shell["harness"], Shell)
	}
}

func TestAgentNamesSorted(t *testing.T) {
	names := AgentNames()
	if !sort.StringsAreSorted(names) {
		t.Fatalf("AgentNames() not sorted: %v", names)
	}
}
