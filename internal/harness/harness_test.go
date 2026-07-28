package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateForHookMapsHarnessEvents(t *testing.T) {
	tests := []struct {
		tool  string
		event string
		want  string
	}{
		{tool: "harness", event: "stop", want: StateWaiting},
		{tool: "harness", event: "idle", want: StateWaiting},
		{tool: "harness", event: "start", want: StateWorking},
		{tool: "harness", event: "resume", want: StateWorking},
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
	if _, err := StateForHook("harness", "mystery"); err == nil {
		t.Fatal("unknown event was accepted")
	}
}

func TestDefaultAuthorEntrypointUsesHarness(t *testing.T) {
	entrypoint, err := DefaultAuthorEntrypoint(Harness)
	if err != nil {
		t.Fatalf("harness default entrypoint: %v", err)
	}
	argv := entrypoint["argv"].([]string)
	if len(argv) != 1 || !contains(argv[0], `harness --hooks "$FLOW_HARNESS_HOOKS" -i "$prompt"`) || !contains(argv[0], "--harness harness") {
		t.Fatalf("harness argv = %#v", entrypoint["argv"])
	}
	if entrypoint["harness"] != Harness {
		t.Fatalf("author harness = %#v, want %q", entrypoint["harness"], Harness)
	}
}

func TestDefaultConsoleEntrypointsWithoutPrompt(t *testing.T) {
	harnessEntrypoint, err := DefaultConsoleEntrypointWithArgs(Harness, Args{})
	if err != nil {
		t.Fatalf("harness default console entrypoint: %v", err)
	}
	harnessArgv := harnessEntrypoint["argv"].([]string)
	if len(harnessArgv) != 1 || !contains(harnessArgv[0], `harness --hooks "$FLOW_HARNESS_HOOKS"`) {
		t.Fatalf("harness console argv = %#v", harnessEntrypoint["argv"])
	}
	assertNoConsolePrompt(t, harnessArgv[0])
	if harnessEntrypoint["harness"] != Harness {
		t.Fatalf("console harness = %#v, want %q", harnessEntrypoint["harness"], Harness)
	}

	shell, err := DefaultConsoleEntrypointWithArgs(Shell, Args{})
	if err != nil {
		t.Fatalf("shell default console entrypoint: %v", err)
	}
	shellArgv := shell["argv"].([]string)
	if len(shellArgv) != 1 || shellArgv[0] != `exec "${SHELL:-/bin/sh}"` {
		t.Fatalf("shell console argv = %#v", shell["argv"])
	}
	assertNoConsolePrompt(t, shellArgv[0])
	if shell["harness"] != Shell {
		t.Fatalf("shell console harness = %#v, want %q", shell["harness"], Shell)
	}
}

func TestDefaultEntrypointsAppendHarnessArgs(t *testing.T) {
	author, err := DefaultAuthorEntrypointWithArgs(Harness, Args{
		Harness: []string{"--provider anthropic --model claude-sonnet-4-6"},
	})
	if err != nil {
		t.Fatalf("harness author entrypoint with shell-style args: %v", err)
	}
	authorCommand := author["argv"].([]string)[0]
	if !strings.Contains(authorCommand, `'--provider' 'anthropic' '--model' 'claude-sonnet-4-6' -i "$prompt"`) {
		t.Fatalf("harness author command did not split shell-style args:\n%s", authorCommand)
	}

	console, err := DefaultConsoleEntrypointWithArgs(Harness, Args{
		Harness: []string{"--model", "anthropic:claude-sonnet-4-6"},
	})
	if err != nil {
		t.Fatalf("harness console entrypoint with args: %v", err)
	}
	consoleCommand := console["argv"].([]string)[0]
	if !strings.Contains(consoleCommand, `harness --hooks "$FLOW_HARNESS_HOOKS" '--model' 'anthropic:claude-sonnet-4-6'`) {
		t.Fatalf("harness console command did not append args:\n%s", consoleCommand)
	}
	assertNoConsolePrompt(t, consoleCommand)
}

func TestNormalizeArgsRejectsManagedFlags(t *testing.T) {
	tests := []Args{
		{Harness: []string{"--hooks", "/tmp/hooks.json"}},
		{Harness: []string{"--hooks=/tmp/hooks.json"}},
		{Harness: []string{"-p", "prompt"}},
		{Harness: []string{"--prompt", "prompt"}},
		{Harness: []string{"-i", "prompt"}},
		{Harness: []string{"--initial-prompt", "prompt"}},
	}
	for _, test := range tests {
		if _, err := NormalizeArgs(test); err == nil {
			t.Fatalf("NormalizeArgs(%+v) succeeded, want error", test)
		}
	}
	if _, err := NormalizeArgs(Args{Harness: []string{"--profile", "review", "--model", "anthropic:claude-sonnet-4-6"}}); err != nil {
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
	if err := ValidateAgentName(Harness); err != nil {
		t.Fatalf("ValidateAgentName(%q) should not require executable: %v", Harness, err)
	}

	toolDir := t.TempDir()
	writeFakeExecutable(t, filepath.Join(toolDir, Harness))
	t.Setenv("PATH", toolDir)
	agentNames := definitionNames(AvailableAgentDefinitions())
	if !agentNames[Harness] {
		t.Fatalf("available agents = %v, missing %q", agentNames, Harness)
	}
	consoleNames := definitionNames(ConsoleDefinitionsFromAvailableAgents(AvailableAgentDefinitions()))
	for _, want := range []string{Harness, Shell} {
		if !consoleNames[want] {
			t.Fatalf("available consoles = %v, missing %q", consoleNames, want)
		}
	}
}

func TestAvailableDefinitionsRunUsabilityChecks(t *testing.T) {
	toolDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "checks.log")
	t.Setenv("FLOW_FAKE_HARNESS_CHECK_LOG", logPath)

	writeFakeExecutableScript(t, filepath.Join(toolDir, Harness), `#!/bin/sh
printf 'harness %s\n' "$*" >> "$FLOW_FAKE_HARNESS_CHECK_LOG"
if [ "$*" = "--check-model-proxy" ]; then
  exit 1
fi
exit 0
`)
	t.Setenv("PATH", toolDir)

	agentNames := definitionNames(AvailableAgentDefinitions())
	if agentNames[Harness] {
		t.Fatalf("available agents = %v, want none (usability check fails)", agentNames)
	}
	consoleNames := definitionNames(ConsoleDefinitionsFromAvailableAgents(AvailableAgentDefinitions()))
	if consoleNames[Harness] || !consoleNames[Shell] {
		t.Fatalf("available consoles = %v, want shell only", consoleNames)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read check log: %v", err)
	}
	if !strings.Contains(string(logBytes), "harness --check-model-proxy") {
		t.Fatalf("check log missing harness usability check:\n%s", logBytes)
	}
}

func TestAvailableAgentLabelsUseUsableDefinitions(t *testing.T) {
	toolDir := t.TempDir()
	writeFakeExecutable(t, filepath.Join(toolDir, Harness))
	t.Setenv("PATH", toolDir)

	labels := AvailableAgentLabels()
	if labels[AgentHarnessLabel(Harness)] != "true" {
		t.Fatalf("available labels = %#v, want harness", labels)
	}

	defs := AgentDefinitionsFromLabels(labels)
	names := definitionNames(defs)
	if !names[Harness] || len(names) != 1 {
		t.Fatalf("definitions from labels = %v, want harness only", names)
	}
}

func TestDefaultHarnessHookedCommandConfiguresHooks(t *testing.T) {
	command := DefaultHarnessHookedCommandWithArgs(nil)
	for _, want := range []string{
		"flow fetch-prompt --harness harness",
		`[ -n "${FLOW_HARNESS_HOOKS:-}" ]`,
		`harness --hooks "$FLOW_HARNESS_HOOKS" -i "$prompt"`,
		`harness -i "$prompt"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("default harness command missing %q:\n%s", want, command)
		}
	}
}

func TestDefaultHarnessConsoleCommandConfiguresHooksWithoutPrompt(t *testing.T) {
	command := DefaultHarnessConsoleCommandWithArgs(nil)
	for _, want := range []string{
		`[ -n "${FLOW_HARNESS_HOOKS:-}" ]`,
		`harness --hooks "$FLOW_HARNESS_HOOKS"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("default harness console command missing %q:\n%s", want, command)
		}
	}
	assertNoConsolePrompt(t, command)
}

func TestDefaultHarnessPrintCommandIsNonInteractive(t *testing.T) {
	command := DefaultHarnessPrintCommandWithArgs(nil)
	for _, want := range []string{
		"flow fetch-prompt --harness harness",
		`harness -p "$prompt"`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("print command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "FLOW_HARNESS_HOOKS") {
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
		{argv: nil, want: Harness},
		{argv: []string{`harness --hooks "$FLOW_HARNESS_HOOKS" -i "$prompt"`}, want: Harness},
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
