package harness

import (
	"sort"
	"strings"
	"testing"
)

func TestKindStringConstsMatch(t *testing.T) {
	pairs := []struct {
		str  string
		kind Kind
	}{
		{Codex, KindCodex},
		{Claude, KindClaude},
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

// TestDefaultAgentCheckCommandMatchesBuilders is a golden behavior-unchanged
// test: the table-driven DefaultAgentCheckCommandWithArgs must produce exactly
// the same string the per-harness builders return.
func TestDefaultAgentCheckCommandMatchesBuilders(t *testing.T) {
	args := []string{"--model", "fast"}
	cases := []struct {
		name string
		want string
	}{
		{Codex, DefaultCodexExecCommandWithArgs(args)},
		{Claude, DefaultClaudePrintCommandWithArgs(args)},
		{Harness, DefaultHarnessPrintCommandWithArgs(args)},
	}
	for _, test := range cases {
		got, err := DefaultAgentCheckCommandWithArgs(test.name, args)
		if err != nil {
			t.Fatalf("DefaultAgentCheckCommandWithArgs(%q): %v", test.name, err)
		}
		if got != test.want {
			t.Fatalf("DefaultAgentCheckCommandWithArgs(%q) =\n%s\nwant\n%s", test.name, got, test.want)
		}
	}
	if _, err := DefaultAgentCheckCommandWithArgs(Shell, args); err == nil {
		t.Fatal("DefaultAgentCheckCommandWithArgs(shell) err = nil, want error")
	}
}

// TestManagedArgValidationGolden proves the table-driven managed-flag and
// managed-config-key validation accepts/rejects exactly the same inputs the
// hand-written switches did.
func TestManagedArgValidationGolden(t *testing.T) {
	reject := []Args{
		// claude reserved flags (exact + value form)
		{Claude: []string{"--settings", "/tmp/s.json"}},
		{Claude: []string{"--settings=/tmp/s.json"}},
		{Claude: []string{"--permission-mode", "bypassPermissions"}},
		{Claude: []string{"--permission-mode=bypassPermissions"}},
		{Claude: []string{"--dangerously-skip-permissions"}},
		{Claude: []string{"--allow-dangerously-skip-permissions"}},
		// harness reserved flags
		{Harness: []string{"--hooks", "/tmp/h.json"}},
		{Harness: []string{"--hooks=/tmp/h.json"}},
		{Harness: []string{"-p", "prompt"}},
		{Harness: []string{"--prompt", "prompt"}},
		// codex reserved flag + config keys
		{Codex: []string{"--dangerously-bypass-hook-trust"}},
		{Codex: []string{"--dangerously-bypass-hook-trust=1"}},
		{Codex: []string{"-c", "features.hooks=true"}},
		{Codex: []string{"-c", "hooks.Stop=[]"}},
		{Codex: []string{"-c", "projects.foo.trust_level=trusted"}},
		{Codex: []string{"--config=projects.$PWD.trust_level=trusted"}},
		{Codex: []string{"-c=hooks.Stop=[]"}},
	}
	for _, args := range reject {
		if _, err := NormalizeArgs(args); err == nil {
			t.Fatalf("NormalizeArgs(%+v) err = nil, want rejection", args)
		}
	}

	accept := []Args{
		{Claude: []string{"--model", "sonnet"}},
		{Harness: []string{"--provider", "anthropic", "--model", "claude-sonnet-4-6"}},
		{Codex: []string{"-c", "model_reasoning_effort=high"}},
		{Codex: []string{"-c", "model=gpt-5"}},
		{Codex: []string{"--config=sandbox_mode=workspace-write"}},
	}
	for _, args := range accept {
		if _, err := NormalizeArgs(args); err != nil {
			t.Fatalf("NormalizeArgs(%+v) err = %v, want accept", args, err)
		}
	}
}

// TestNativeHookMappingGolden asserts the HookState wiring on each Definition
// classifies events identically to the ParseNativeHook switch it replaced.
func TestNativeHookMappingGolden(t *testing.T) {
	cases := []struct {
		harness          string
		event            string
		notificationType string
		want             string
	}{
		{Codex, "UserPromptSubmit", "", StateWorking},
		{Codex, "PreToolUse", "", StateWorking},
		{Codex, "PermissionRequest", "", StateWaiting},
		{Codex, "Stop", "", StateWaiting},
		{Codex, "PostToolUse", "", SignalActivity},
		{Claude, "UserPromptSubmit", "", StateWorking},
		{Claude, "PreToolUse", "", StateWorking},
		{Claude, "PermissionRequest", "", StateWaiting},
		{Claude, "Stop", "", StateWaiting},
		{Claude, "StopFailure", "", StateWaiting},
		{Claude, "PostToolUse", "", SignalActivity},
		{Claude, "PostToolUseFailure", "", SignalActivity},
		{Claude, "Notification", "permission_prompt", StateWaiting},
		{Claude, "Notification", "idle_prompt", StateWaiting},
		{Claude, "Notification", "auth_success", SignalActivity},
		{Harness, "SessionStart", "", StateWorking},
		{Harness, "UserPromptSubmit", "", StateWorking},
		{Harness, "PreToolUse", "", StateWorking},
		{Harness, "Stop", "", StateWaiting},
		{Harness, "PostToolUse", "", SignalActivity},
		{Harness, "PreCompact", "", SignalActivity},
		{Harness, "PostCompact", "", SignalActivity},
	}
	for _, test := range cases {
		definition, ok := Lookup(test.harness)
		if !ok || definition.HookState == nil {
			t.Fatalf("definition %q missing HookState", test.harness)
		}
		got := definition.HookState(test.event, test.notificationType)
		if got == "" {
			got = SignalActivity // ParseNativeHook applies the default
		}
		if got != test.want {
			t.Fatalf("%s HookState(%q,%q) = %q, want %q", test.harness, test.event, test.notificationType, got, test.want)
		}
	}
}

// TestHookEventsParityWithHookState ensures every declared HookEvent maps to an
// explicit (non-default) classification, so the future renderer's event table
// stays in lockstep with the runtime classifier.
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
			notificationType := ""
			if strings.EqualFold(event.Name, "Notification") {
				notificationType = firstMatcherAlternative(event.Matcher)
			}
			if mapped := definition.HookState(event.Name, notificationType); mapped == "" {
				t.Fatalf("definition %q HookEvent %q falls through to the default mapping", name, event.Name)
			}
		}
	}
}

func TestHookEventsDataMatchesGenerators(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		format    string
		envVar    string
		matchers  map[string]string
		hasEvents []string
	}{
		{
			name:      Claude,
			count:     8,
			format:    "json",
			envVar:    "FLOW_CLAUDE_HOOK_SETTINGS",
			matchers:  map[string]string{"Notification": "permission_prompt|idle_prompt", "PreToolUse": "*"},
			hasEvents: []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "Notification", "Stop", "StopFailure"},
		},
		{
			name:      Harness,
			count:     7,
			format:    "json",
			envVar:    "FLOW_HARNESS_HOOKS",
			matchers:  map[string]string{"PreToolUse": "*", "PostToolUse": "*"},
			hasEvents: []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PreCompact", "PostCompact", "Stop"},
		},
		{
			name:      Codex,
			count:     8,
			format:    "toml",
			envVar:    "FLOW_CODEX_HOOK_PROFILE",
			matchers:  map[string]string{"PermissionRequest": "*", "PreToolUse": "*", "PostToolUse": "*", "SessionStart": "", "PreCompact": "", "PostCompact": ""},
			hasEvents: []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PreCompact", "PostCompact", "PermissionRequest", "Stop"},
		},
	}
	for _, test := range tests {
		definition, ok := Lookup(test.name)
		if !ok {
			t.Fatalf("Lookup(%q) missing", test.name)
		}
		if len(definition.HookEvents) != test.count {
			t.Fatalf("%s HookEvents count = %d, want %d", test.name, len(definition.HookEvents), test.count)
		}
		if definition.HookFormat != test.format {
			t.Fatalf("%s HookFormat = %q, want %q", test.name, definition.HookFormat, test.format)
		}
		if definition.HookEnvVar != test.envVar {
			t.Fatalf("%s HookEnvVar = %q, want %q", test.name, definition.HookEnvVar, test.envVar)
		}
		byName := map[string]string{}
		for _, event := range definition.HookEvents {
			byName[event.Name] = event.Matcher
		}
		for _, want := range test.hasEvents {
			if _, ok := byName[want]; !ok {
				t.Fatalf("%s HookEvents missing %q: %+v", test.name, want, definition.HookEvents)
			}
		}
		for event, matcher := range test.matchers {
			if byName[event] != matcher {
				t.Fatalf("%s HookEvent %q matcher = %q, want %q", test.name, event, byName[event], matcher)
			}
		}
	}
}

func TestDetectEntrypointHarnessDeterministicTieBreak(t *testing.T) {
	// A single token mentioning multiple harness executables must resolve to a
	// deterministic, sorted-name winner (claude < codex < harness).
	argv := []string{"env codex=1 claude harness run"}
	for i := 0; i < 8; i++ {
		if got := DetectEntrypointHarness(argv); got != Claude {
			t.Fatalf("DetectEntrypointHarness(multi) = %q, want %q (deterministic)", got, Claude)
		}
	}
}

func TestDefaultEntrypointsStampHarness(t *testing.T) {
	author, err := DefaultAuthorEntrypoint(Codex)
	if err != nil {
		t.Fatalf("DefaultAuthorEntrypoint(codex): %v", err)
	}
	if author["harness"] != Codex {
		t.Fatalf("author harness = %#v, want %q", author["harness"], Codex)
	}

	console, err := DefaultConsoleEntrypointWithArgs(Claude, Args{})
	if err != nil {
		t.Fatalf("DefaultConsoleEntrypointWithArgs(claude): %v", err)
	}
	if console["harness"] != Claude {
		t.Fatalf("console harness = %#v, want %q", console["harness"], Claude)
	}

	shell, err := DefaultConsoleEntrypointWithArgs(Shell, Args{})
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

// firstMatcherAlternative returns the first alternative in a "a|b|c" matcher,
// used by the hook-event parity test to feed the Notification classifier a
// value its matcher would accept.
func firstMatcherAlternative(matcher string) string {
	matcher = strings.TrimSpace(matcher)
	if matcher == "" || matcher == "*" {
		return ""
	}
	if before, _, ok := strings.Cut(matcher, "|"); ok {
		return strings.TrimSpace(before)
	}
	return matcher
}
