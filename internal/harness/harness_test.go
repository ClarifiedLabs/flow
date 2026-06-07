package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateForHookMapsCodexAndClaudeEvents(t *testing.T) {
	tests := []struct {
		tool  string
		event string
		want  string
	}{
		{tool: "codex", event: "stop", want: StateWaiting},
		{tool: "codex", event: "resume", want: StateWorking},
		{tool: "claude", event: "notification", want: StateWaiting},
		{tool: "claude", event: "start", want: StateWorking},
		{tool: "harness", event: "stop", want: StateWaiting},
		{tool: "harness", event: "start", want: StateWorking},
	}
	for _, test := range tests {
		got, err := StateForHook(test.tool, test.event)
		if err != nil {
			t.Fatalf("StateForHook(%q, %q): %v", test.tool, test.event, err)
		}
		if got != test.want {
			t.Fatalf("StateForHook(%q, %q) = %q, want %q", test.tool, test.event, got, test.want)
		}
	}
}

func TestStateForHookRejectsUnknownInputs(t *testing.T) {
	if _, err := StateForHook("opencode", "stop"); err == nil {
		t.Fatal("unknown tool was accepted")
	}
	if _, err := StateForHook("codex", "mystery"); err == nil {
		t.Fatal("unknown event was accepted")
	}
}

func TestDefaultAuthorEntrypointUsesSelectedHarness(t *testing.T) {
	codex, err := DefaultAuthorEntrypoint(Codex)
	if err != nil {
		t.Fatalf("codex default entrypoint: %v", err)
	}
	codexArgv := codex["argv"].([]string)
	if len(codexArgv) != 1 || !contains(codexArgv[0], "codex -c") || !contains(codexArgv[0], "--harness codex") {
		t.Fatalf("codex argv = %#v", codex["argv"])
	}

	claude, err := DefaultAuthorEntrypoint(Claude)
	if err != nil {
		t.Fatalf("claude default entrypoint: %v", err)
	}
	claudeArgv := claude["argv"].([]string)
	if len(claudeArgv) != 1 || !contains(claudeArgv[0], `claude --dangerously-skip-permissions --permission-mode bypassPermissions "$prompt"`) || !contains(claudeArgv[0], "--harness claude") {
		t.Fatalf("claude argv = %#v", claude["argv"])
	}

	harness, err := DefaultAuthorEntrypoint(Harness)
	if err != nil {
		t.Fatalf("harness default entrypoint: %v", err)
	}
	harnessArgv := harness["argv"].([]string)
	if len(harnessArgv) != 1 || !contains(harnessArgv[0], `harness --hooks "$FLOW_HARNESS_HOOKS" -p "$prompt"`) || !contains(harnessArgv[0], "--harness harness") {
		t.Fatalf("harness argv = %#v", harness["argv"])
	}
}

func TestDefaultConsoleEntrypointUsesSelectedHarnessWithoutPrompt(t *testing.T) {
	codex, err := DefaultConsoleEntrypointWithArgs(Codex, Args{})
	if err != nil {
		t.Fatalf("codex default console entrypoint: %v", err)
	}
	codexArgv := codex["argv"].([]string)
	if len(codexArgv) != 1 || !contains(codexArgv[0], "codex -c") || !contains(codexArgv[0], `-c "projects.$PWD.trust_level=trusted"`) {
		t.Fatalf("codex console argv = %#v", codex["argv"])
	}
	assertNoConsolePrompt(t, codexArgv[0])

	claude, err := DefaultConsoleEntrypointWithArgs(Claude, Args{})
	if err != nil {
		t.Fatalf("claude default console entrypoint: %v", err)
	}
	claudeArgv := claude["argv"].([]string)
	if len(claudeArgv) != 1 || !contains(claudeArgv[0], `claude --settings "$FLOW_CLAUDE_HOOK_SETTINGS" --dangerously-skip-permissions --permission-mode bypassPermissions`) {
		t.Fatalf("claude console argv = %#v", claude["argv"])
	}
	assertNoConsolePrompt(t, claudeArgv[0])

	shell, err := DefaultConsoleEntrypointWithArgs(Shell, Args{})
	if err != nil {
		t.Fatalf("shell default console entrypoint: %v", err)
	}
	shellArgv := shell["argv"].([]string)
	if len(shellArgv) != 1 || shellArgv[0] != `exec "${SHELL:-/bin/sh}"` {
		t.Fatalf("shell console argv = %#v", shell["argv"])
	}
	assertNoConsolePrompt(t, shellArgv[0])

	harness, err := DefaultConsoleEntrypointWithArgs(Harness, Args{})
	if err != nil {
		t.Fatalf("harness default console entrypoint: %v", err)
	}
	harnessArgv := harness["argv"].([]string)
	if len(harnessArgv) != 1 || !contains(harnessArgv[0], `harness --hooks "$FLOW_HARNESS_HOOKS"`) {
		t.Fatalf("harness console argv = %#v", harness["argv"])
	}
	assertNoConsolePrompt(t, harnessArgv[0])
}

func TestDefaultEntrypointsAppendHarnessArgs(t *testing.T) {
	author, err := DefaultAuthorEntrypointWithArgs(Codex, Args{
		Codex: []string{"--model", "gpt-5", "-c", "model_reasoning_effort=high"},
	})
	if err != nil {
		t.Fatalf("codex author entrypoint with args: %v", err)
	}
	authorCommand := author["argv"].([]string)[0]
	for _, want := range []string{
		`--dangerously-bypass-hook-trust -c "projects.$PWD.trust_level=trusted" '--model' 'gpt-5' -c 'model_reasoning_effort=high' "$prompt"`,
		"flow hook codex ingest",
	} {
		if !strings.Contains(authorCommand, want) {
			t.Fatalf("codex author command missing %q:\n%s", want, authorCommand)
		}
	}

	console, err := DefaultConsoleEntrypointWithArgs(Claude, Args{
		Claude: []string{"--model", "sonnet"},
	})
	if err != nil {
		t.Fatalf("claude console entrypoint with args: %v", err)
	}
	consoleCommand := console["argv"].([]string)[0]
	if !strings.Contains(consoleCommand, `--permission-mode bypassPermissions '--model' 'sonnet'`) {
		t.Fatalf("claude console command did not append args:\n%s", consoleCommand)
	}
	assertNoConsolePrompt(t, consoleCommand)

	harnessAuthor, err := DefaultAuthorEntrypointWithArgs(Harness, Args{
		Harness: []string{"--provider anthropic --model claude-sonnet-4-6"},
	})
	if err != nil {
		t.Fatalf("harness author entrypoint with shell-style args: %v", err)
	}
	harnessCommand := harnessAuthor["argv"].([]string)[0]
	if !strings.Contains(harnessCommand, `'--provider' 'anthropic' '--model' 'claude-sonnet-4-6' -p "$prompt"`) {
		t.Fatalf("harness author command did not split shell-style args:\n%s", harnessCommand)
	}
}

func TestNormalizeArgsRejectsManagedFlags(t *testing.T) {
	tests := []Args{
		{Codex: []string{"--dangerously-bypass-hook-trust"}},
		{Codex: []string{"-c", "hooks.Stop=[]"}},
		{Codex: []string{"--config=projects.$PWD.trust_level=trusted"}},
		{Codex: []string{"--profile", "flow"}},
		{Codex: []string{"-p", "flow"}},
		{Claude: []string{"--settings", "/tmp/settings.json"}},
		{Claude: []string{"--permission-mode=bypassPermissions"}},
		{Harness: []string{"--hooks=/tmp/hooks.json"}},
		{Harness: []string{"-p", "prompt"}},
	}
	for _, test := range tests {
		if _, err := NormalizeArgs(test); err == nil {
			t.Fatalf("NormalizeArgs(%+v) succeeded, want error", test)
		}
	}
	if _, err := NormalizeArgs(Args{Codex: []string{"-c", "model=gpt-5"}, Claude: []string{"--model", "sonnet"}, Harness: []string{"--profile", "review"}}); err != nil {
		t.Fatalf("NormalizeArgs accepted safe flags with error: %v", err)
	}
}

func TestNormalizeArgsSplitsShellStyleStrings(t *testing.T) {
	normalized, err := NormalizeArgs(Args{
		Harness: []string{`--provider anthropic --model "claude sonnet" --profile=review`},
	})
	if err != nil {
		t.Fatalf("NormalizeArgs shell-style strings: %v", err)
	}
	want := []string{"--provider", "anthropic", "--model", "claude sonnet", "--profile=review"}
	if got := normalized.Harness; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized harness args = %#v, want %#v", got, want)
	}

	if _, err := NormalizeArgs(Args{Harness: []string{`--model "unterminated`}}); err == nil {
		t.Fatal("NormalizeArgs accepted unmatched quote")
	}
}

func TestShellIsConsoleOnlyHarness(t *testing.T) {
	if err := ValidateConsoleName(Shell); err != nil {
		t.Fatalf("ValidateConsoleName(%q): %v", Shell, err)
	}
	if err := ValidateAgentName(Shell); err == nil {
		t.Fatalf("ValidateAgentName(%q) succeeded, want error", Shell)
	}
}

func TestAvailableDefinitionsRequireExecutablesOnPath(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	if got := AvailableAgentDefinitions(); len(got) != 0 {
		t.Fatalf("available agent definitions with empty PATH = %+v, want none", got)
	}
	consoles := ConsoleDefinitionsFromAvailableAgents(AvailableAgentDefinitions())
	if len(consoles) != 1 || consoles[0].Name != Shell {
		t.Fatalf("available console definitions with empty PATH = %+v, want shell only", consoles)
	}
	if err := ValidateAgentName(Codex); err != nil {
		t.Fatalf("ValidateAgentName(%q) should not require executable: %v", Codex, err)
	}

	toolDir := t.TempDir()
	for _, name := range []string{Codex, Claude, Harness} {
		writeFakeExecutable(t, filepath.Join(toolDir, name))
	}
	t.Setenv("PATH", toolDir)
	agentNames := definitionNames(AvailableAgentDefinitions())
	for _, want := range []string{Claude, Codex, Harness} {
		if !agentNames[want] {
			t.Fatalf("available agents = %v, missing %q", agentNames, want)
		}
	}
	consoleNames := definitionNames(ConsoleDefinitionsFromAvailableAgents(AvailableAgentDefinitions()))
	for _, want := range []string{Claude, Codex, Harness, Shell} {
		if !consoleNames[want] {
			t.Fatalf("available consoles = %v, missing %q", consoleNames, want)
		}
	}
}

func TestAvailableDefinitionsRunUsabilityChecks(t *testing.T) {
	toolDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "checks.log")
	t.Setenv("FLOW_FAKE_HARNESS_CHECK_LOG", logPath)

	writeFakeExecutableScript(t, filepath.Join(toolDir, Codex), `#!/bin/sh
printf 'codex %s\n' "$*" >> "$FLOW_FAKE_HARNESS_CHECK_LOG"
if [ "$*" = "login status" ]; then
  exit 1
fi
exit 0
`)
	writeFakeExecutableScript(t, filepath.Join(toolDir, Claude), `#!/bin/sh
printf 'claude %s\n' "$*" >> "$FLOW_FAKE_HARNESS_CHECK_LOG"
if [ "$*" = "auth status" ]; then
  exit 0
fi
exit 1
`)
	writeFakeExecutableScript(t, filepath.Join(toolDir, Harness), `#!/bin/sh
printf 'harness %s\n' "$*" >> "$FLOW_FAKE_HARNESS_CHECK_LOG"
if [ "$*" = "--check-model-proxy" ]; then
  exit 1
fi
exit 0
`)
	t.Setenv("PATH", toolDir)

	agentNames := definitionNames(AvailableAgentDefinitions())
	if agentNames[Codex] || !agentNames[Claude] || agentNames[Harness] {
		t.Fatalf("available agents = %v, want only claude", agentNames)
	}
	consoleNames := definitionNames(ConsoleDefinitionsFromAvailableAgents(AvailableAgentDefinitions()))
	if consoleNames[Codex] || !consoleNames[Claude] || consoleNames[Harness] || !consoleNames[Shell] {
		t.Fatalf("available consoles = %v, want claude and shell", consoleNames)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read check log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{
		"codex login status",
		"claude auth status",
		"harness --check-model-proxy",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("check log missing %q:\n%s", want, log)
		}
	}
}

func TestAvailableAgentLabelsUseUsableDefinitions(t *testing.T) {
	toolDir := t.TempDir()
	writeFakeExecutable(t, filepath.Join(toolDir, Codex))
	writeFakeExecutable(t, filepath.Join(toolDir, Harness))
	t.Setenv("PATH", toolDir)

	labels := AvailableAgentLabels()
	if labels[AgentHarnessLabel(Codex)] != "true" || labels[AgentHarnessLabel(Harness)] != "true" {
		t.Fatalf("available labels = %#v, want codex and harness", labels)
	}
	if labels[AgentHarnessLabel(Claude)] == "true" {
		t.Fatalf("available labels = %#v, did not expect claude", labels)
	}

	defs := AgentDefinitionsFromLabels(labels)
	names := definitionNames(defs)
	if !names[Codex] || !names[Harness] || names[Claude] {
		t.Fatalf("definitions from labels = %v, want codex and harness", names)
	}
}

func TestDefaultCodexHookedCommandConfiguresNativeHooks(t *testing.T) {
	command := DefaultCodexHookedCommandWithArgs(nil)
	for _, want := range []string{
		"flow hook codex start",
		"flow hook codex stop",
		"--dangerously-bypass-hook-trust",
		// Managed profile branch (used when the worker exports the profile name).
		`[ -n "${FLOW_CODEX_HOOK_PROFILE:-}" ]`,
		`codex --profile "$FLOW_CODEX_HOOK_PROFILE" --dangerously-bypass-hook-trust -c "projects.$PWD.trust_level=trusted" "$prompt"`,
		// Inline `-c` fallback branch (used when no profile is exported).
		"features.hooks=true",
		"hooks.SessionStart",
		"hooks.UserPromptSubmit",
		"hooks.PreCompact",
		"hooks.PostCompact",
		"hooks.Stop",
		"hooks.PermissionRequest",
		"hooks.PreToolUse",
		"hooks.PostToolUse",
		"flow hook codex ingest",
		`-c "projects.$PWD.trust_level=trusted"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("default codex command missing %q:\n%s", want, command)
		}
	}
}

func TestDefaultCodexConsoleCommandConfiguresHooksAndTrustWithoutPrompt(t *testing.T) {
	command := DefaultCodexConsoleCommandWithArgs(nil)
	for _, want := range []string{
		"flow hook codex start",
		"flow hook codex stop",
		"--dangerously-bypass-hook-trust",
		`[ -n "${FLOW_CODEX_HOOK_PROFILE:-}" ]`,
		`codex --profile "$FLOW_CODEX_HOOK_PROFILE" --dangerously-bypass-hook-trust -c "projects.$PWD.trust_level=trusted"`,
		"features.hooks=true",
		"flow hook codex ingest",
		`-c "projects.$PWD.trust_level=trusted"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("default codex console command missing %q:\n%s", want, command)
		}
	}
	assertNoConsolePrompt(t, command)
}

func TestDefaultCodexExecCommandDoesNotConfigureNativeHooks(t *testing.T) {
	command := DefaultCodexExecCommandWithArgs(nil)
	for _, unexpected := range []string{
		"features.hooks=true",
		"flow hook codex ingest",
		"--dangerously-bypass-hook-trust",
	} {
		if strings.Contains(command, unexpected) {
			t.Fatalf("codex exec command includes author native hook config %q:\n%s", unexpected, command)
		}
	}
}

func TestDefaultClaudeHookedCommandUsesSettingsWhenPresent(t *testing.T) {
	command := DefaultClaudeHookedCommandWithArgs(nil)
	for _, want := range []string{
		`[ -n "${FLOW_CLAUDE_HOOK_SETTINGS:-}" ]`,
		`claude --settings "$FLOW_CLAUDE_HOOK_SETTINGS" --dangerously-skip-permissions --permission-mode bypassPermissions "$prompt"`,
		`claude --dangerously-skip-permissions --permission-mode bypassPermissions "$prompt"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("default claude command missing %q:\n%s", want, command)
		}
	}
}

func TestDefaultClaudeConsoleCommandUsesSettingsWithoutPrompt(t *testing.T) {
	command := DefaultClaudeConsoleCommandWithArgs(nil)
	for _, want := range []string{
		"flow hook claude start",
		"flow hook claude stop",
		`[ -n "${FLOW_CLAUDE_HOOK_SETTINGS:-}" ]`,
		`claude --settings "$FLOW_CLAUDE_HOOK_SETTINGS" --dangerously-skip-permissions --permission-mode bypassPermissions`,
		`claude --dangerously-skip-permissions --permission-mode bypassPermissions`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("default claude console command missing %q:\n%s", want, command)
		}
	}
	assertNoConsolePrompt(t, command)
}

func TestDefaultClaudePrintCommandIsNonInteractive(t *testing.T) {
	command := DefaultClaudePrintCommandWithArgs(nil)
	for _, want := range []string{
		"flow fetch-prompt --harness claude",
		`claude --dangerously-skip-permissions --permission-mode bypassPermissions -p "$prompt"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("print command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "flow hook claude") {
		t.Fatalf("print command includes session hooks:\n%s", command)
	}
}

func TestDefaultAgentCheckCommandUsesSelectedHarness(t *testing.T) {
	command, err := DefaultAgentCheckCommandWithArgs(Harness, []string{"--model", "fast"})
	if err != nil {
		t.Fatalf("default harness check command: %v", err)
	}
	for _, want := range []string{"flow fetch-prompt --harness harness", "harness '--model' 'fast' -p \"$prompt\""} {
		if !strings.Contains(command, want) {
			t.Fatalf("harness check command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "FLOW_HARNESS_HOOKS") {
		t.Fatalf("harness check command should not configure session hooks:\n%s", command)
	}
}

func TestDetectEntrypointHarnessUsesRegistry(t *testing.T) {
	tests := []struct {
		argv []string
		want string
	}{
		{argv: nil, want: Codex},
		{argv: []string{"/usr/local/bin/codex", "prompt"}, want: Codex},
		{argv: []string{`claude "$(flow fetch-prompt)"`}, want: Claude},
		{argv: []string{`harness --hooks "$FLOW_HARNESS_HOOKS" -p "$prompt"`}, want: Harness},
		{argv: []string{"custom-agent"}, want: Agents},
	}
	for _, test := range tests {
		if got := DetectEntrypointHarness(test.argv); got != test.want {
			t.Fatalf("DetectEntrypointHarness(%v) = %q, want %q", test.argv, got, test.want)
		}
	}
}

func writeFakeExecutable(t *testing.T, path string) {
	t.Helper()
	writeFakeExecutableScript(t, path, "#!/bin/sh\nexit 0\n")
}

func writeFakeExecutableScript(t *testing.T, path string, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake executable %s: %v", path, err)
	}
}

func definitionNames(definitions []Definition) map[string]bool {
	names := map[string]bool{}
	for _, definition := range definitions {
		names[definition.Name] = true
	}
	return names
}

func contains(value string, snippet string) bool {
	return strings.Contains(value, snippet)
}

func assertNoConsolePrompt(t *testing.T, command string) {
	t.Helper()
	for _, unexpected := range []string{"flow fetch-prompt", `"$prompt"`, "flow-console"} {
		if strings.Contains(command, unexpected) {
			t.Fatalf("console command includes prompt setup %q:\n%s", unexpected, command)
		}
	}
}
