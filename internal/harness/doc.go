package harness

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Kind is the typed identity of an agent/console harness. It replaces the loose
// string switches that used to re-derive a harness from argv. The string consts
// below remain for callers that still pass plain strings (map keys, payload
// fields), but new code should prefer Kind.
type Kind string

const (
	KindCodex   Kind = "codex"
	KindClaude  Kind = "claude"
	KindHarness Kind = "harness"
	KindAgents  Kind = "agents"
	KindShell   Kind = "shell"
)

const (
	Codex   = string(KindCodex)
	Claude  = string(KindClaude)
	Harness = string(KindHarness)
	Agents  = string(KindAgents)
	Shell   = string(KindShell)

	AgentHarnessLabelPrefix = "agent.harness."

	StateWorking = "working"
	StateWaiting = "waiting"
)

const availabilityCheckTimeout = 5 * time.Second

// codexNativeHookEvents is codex's native-hook event set. It is a standalone var
// (rather than an inline literal in the definitions table) so the inline `-c`
// renderer can reference it without forming an initialization cycle with the
// definitions map that the codex command builders close over.
//
// The set is broadened toward parity with claude/harness using ONLY events the
// installed codex actually emits (verified against codex's HookEvent enum:
// PreToolUse, PermissionRequest, PostToolUse, PreCompact, PostCompact,
// SessionStart, UserPromptSubmit, SubagentStart, SubagentStop, Stop). Codex has
// no Notification/StopFailure/PostToolUseFailure events, so those stay
// claude-only. Matchers are applied only to tool/permission events, matching the
// pre-existing codex convention and codex's own examples; session and compaction
// events carry no matcher.
var codexNativeHookEvents = []HookEvent{
	{Name: "SessionStart"},
	{Name: "UserPromptSubmit"},
	{Name: "PreToolUse", Matcher: "*"},
	{Name: "PostToolUse", Matcher: "*"},
	{Name: "PreCompact"},
	{Name: "PostCompact"},
	{Name: "PermissionRequest", Matcher: "*"},
	{Name: "Stop"},
}

const codexNativeHookTimeoutSeconds = 5

// envCodexHookProfile names the env var that carries codex's managed hook profile
// name. The worker writes $CODEX_HOME/<name>.config.toml and exports this var;
// the codex command builders pass it to `codex --profile` when it is set.
const envCodexHookProfile = "FLOW_CODEX_HOOK_PROFILE"

// CodexHookProfileName is the profile name Flow writes and loads for codex hooks
// (the file is $CODEX_HOME/flow.config.toml).
const CodexHookProfileName = "flow"

// HookEvent describes a single native-hook event the harness emits. Matcher is
// the harness-native matcher string ("*" for all tools, or a notification-type
// alternation like "permission_prompt|idle_prompt"); empty means no matcher.
type HookEvent struct {
	Name    string
	Matcher string
}

type Definition struct {
	Name              string
	Kind              Kind
	DisplayName       string
	Executable        string
	RequireExecutable bool
	UsabilityCheck    []string
	// CheckCommand builds the non-interactive check/print command (used by
	// reviewer/verifier jobs) for the given additive argv tokens.
	CheckCommand func([]string) string
	// HookState classifies a native-hook event (and, for harnesses that use it,
	// a notification type) into a session state signal. It returns "" for events
	// it does not explicitly recognize; callers apply the activity default.
	HookState func(event, notificationType string) string
	// ManagedFlags / ManagedConfigKeys are the argv flags and (for codex) config
	// keys Flow reserves; user-supplied harness args may not override them. A
	// config key ending in "." matches by prefix, otherwise by exact equality.
	ManagedFlags      []string
	ManagedConfigKeys []string
	// HookEvents, HookFormat and HookEnvVar describe how this harness's native
	// hooks are wired, as the data source of truth for the hook-config renderer.
	// HookFormat is one of "json", "toml" or "inline". HookEnvVar is the env var
	// that points the harness at the generated config (empty when inline).
	// HookTimeoutSeconds is the per-hook command timeout written into the config
	// (0 omits the field; claude omits it, harness/codex use 5).
	HookEvents         []HookEvent
	HookFormat         string
	HookEnvVar         string
	HookTimeoutSeconds int
	// Models returns this harness's selectable model catalog (dynamic for the
	// harness CLI, curated static lists for claude/codex). Nil means the harness
	// exposes no Flow-selectable models. AvailableModels wraps it to stamp each
	// model with this harness's name.
	Models func() ([]Model, error)
	// Trust-prompt scraping. The interactive worker drives codex/claude through a
	// TTY (tmux), where their directory/workspace-trust dialog appears and must be
	// dismissed before the initial prompt is pasted. These data fields make the
	// matcher tolerant of TUI copy drift (a copy tweak is a one-line data edit,
	// not a code change) and table-testable, while preserving the anti-injection
	// invariant via TrustPromptSubmitMarker. Empty TrustPromptMarkers means the
	// harness shows no scraped trust prompt: the harness CLI uses a clean -p, and
	// codex's directory prompt is normally suppressed by
	// projects.<cwd>.trust_level=trusted — but that suppression silently fails
	// when the worktree path contains a symlink (so $PWD != codex's canonical
	// path), and claude's workspace-trust dialog is never suppressed by a flag in
	// interactive/TTY mode (only by -p / non-TTY), so the scrapers remain as a
	// safety net. See TrustPromptVisible / TrustPromptForegroundAllowed.
	//
	// TrustPromptMarkers are substrings that must ALL appear (case-insensitively)
	// anywhere in the captured pane. TrustPromptSubmitMarker is the prompt's
	// submit/confirm instruction; it must appear on the LAST non-empty pane line,
	// which both confirms the prompt is live and rejects pane content that merely
	// quotes the dialog. TrustPromptSubmitKey is the tmux send-keys key that
	// approves it. TrustPromptForeground lists the foreground process base-names
	// (wrapper shell, node wrapper, harness binary) under which the prompt may
	// legitimately be shown, so the approver never types into an unrelated program.
	TrustPromptMarkers      []string
	TrustPromptSubmitMarker string
	TrustPromptSubmitKey    string
	TrustPromptForeground   []string
}

// AvailableModels returns the harness's model catalog with every entry stamped
// with this harness's name and normalized (lowercased harness/sorted/deduped,
// qualified IDs filled). It returns nil when the harness exposes no models.
func (d Definition) AvailableModels() ([]Model, error) {
	if d.Models == nil {
		return nil, nil
	}
	models, err := d.Models()
	if err != nil {
		return nil, err
	}
	stamped := CloneModels(models)
	for i := range stamped {
		stamped[i].Harness = d.Name
	}
	return NormalizeModels(stamped)
}

// flagManaged reports whether arg overrides one of the harness's reserved flags,
// matching both the bare "--flag" form and the "--flag=value" form.
func (d Definition) flagManaged(arg string) bool {
	for _, flag := range d.ManagedFlags {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

// configKeyManaged reports whether a "-c key=value" style config token targets a
// Flow-managed key. Entries ending in "." match by prefix; others are exact.
func (d Definition) configKeyManaged(value string) bool {
	key := strings.TrimSpace(value)
	if before, _, ok := strings.Cut(key, "="); ok {
		key = strings.TrimSpace(before)
	}
	for _, managed := range d.ManagedConfigKeys {
		if strings.HasSuffix(managed, ".") {
			if strings.HasPrefix(key, managed) {
				return true
			}
			continue
		}
		if key == managed {
			return true
		}
	}
	return false
}

var definitions = map[string]Definition{
	Codex: {
		Name:              Codex,
		Kind:              KindCodex,
		DisplayName:       "Codex",
		Executable:        "codex",
		RequireExecutable: true,
		UsabilityCheck:    []string{"login", "status"},
		CheckCommand:      DefaultCodexExecCommandWithArgs,
		HookState:         func(event, _ string) string { return mapCodexNativeHook(event) },
		// --profile / -p are reserved because Flow injects its managed hook
		// profile via `codex --profile`; the config-key guards stay table data so
		// user args still cannot clobber the hook/trust config codex loads from it.
		ManagedFlags:       []string{"--dangerously-bypass-hook-trust", "--profile", "-p"},
		ManagedConfigKeys:  []string{"features.hooks", "hooks.", "projects."},
		HookEvents:         codexNativeHookEvents,
		HookFormat:         "toml",
		HookEnvVar:         envCodexHookProfile,
		HookTimeoutSeconds: codexNativeHookTimeoutSeconds,
		Models:             CuratedCodexModels,
		// Codex "Do you trust the contents of this directory?" directory prompt.
		TrustPromptMarkers:      []string{"do you trust the contents", "yes, continue", "no, quit"},
		TrustPromptSubmitMarker: "enter to continue",
		TrustPromptSubmitKey:    "Enter",
		TrustPromptForeground:   []string{"bash", "codex", "fish", "node", "sh", "zsh"},
	},
	Claude: {
		Name:              Claude,
		Kind:              KindClaude,
		DisplayName:       "Claude Code",
		Executable:        "claude",
		RequireExecutable: true,
		UsabilityCheck:    []string{"auth", "status"},
		CheckCommand:      DefaultClaudePrintCommandWithArgs,
		HookState:         mapClaudeNativeHook,
		ManagedFlags: []string{
			"--settings",
			"--permission-mode",
			"--dangerously-skip-permissions",
			"--allow-dangerously-skip-permissions",
		},
		HookEvents: []HookEvent{
			{Name: "UserPromptSubmit"},
			{Name: "PreToolUse", Matcher: "*"},
			{Name: "PostToolUse", Matcher: "*"},
			{Name: "PostToolUseFailure", Matcher: "*"},
			{Name: "PermissionRequest", Matcher: "*"},
			{Name: "Notification", Matcher: "permission_prompt|idle_prompt"},
			{Name: "Stop"},
			{Name: "StopFailure"},
		},
		HookFormat: "json",
		HookEnvVar: "FLOW_CLAUDE_HOOK_SETTINGS",
		Models:     CuratedClaudeModels,
		// Claude Code "Quick safety check" workspace-trust prompt. Per `claude
		// --help` this dialog is only skipped in non-interactive mode (-p / no TTY);
		// the managed --dangerously-skip-permissions / --permission-mode flags do
		// NOT suppress it in the interactive author/console sessions, so it must be
		// scraped and dismissed.
		TrustPromptMarkers:      []string{"quick safety check", "yes, i trust this folder", "no, exit"},
		TrustPromptSubmitMarker: "enter to confirm",
		TrustPromptSubmitKey:    "Enter",
		TrustPromptForeground:   []string{"bash", "claude", "fish", "node", "sh", "zsh"},
	},
	Harness: {
		Name:              Harness,
		Kind:              KindHarness,
		DisplayName:       "Harness",
		Executable:        "harness",
		RequireExecutable: true,
		UsabilityCheck:    []string{"--check-model-proxy"},
		CheckCommand:      DefaultHarnessPrintCommandWithArgs,
		HookState:         func(event, _ string) string { return mapHarnessNativeHook(event) },
		ManagedFlags:      []string{"--hooks", "-p", "--prompt"},
		HookEvents: []HookEvent{
			{Name: "SessionStart"},
			{Name: "UserPromptSubmit"},
			{Name: "PreToolUse", Matcher: "*"},
			{Name: "PostToolUse", Matcher: "*"},
			{Name: "PreCompact", Matcher: "*"},
			{Name: "PostCompact", Matcher: "*"},
			{Name: "Stop"},
		},
		HookFormat:         "json",
		HookEnvVar:         "FLOW_HARNESS_HOOKS",
		HookTimeoutSeconds: 5,
		Models:             AvailableHarnessModels,
	},
}

var promptHarnesses = map[string]struct{}{
	Codex:   {},
	Claude:  {},
	Agents:  {},
	Harness: {},
}

func DefaultAgentName() string {
	return Codex
}

func DefaultConsoleName() string {
	return Claude
}

func DefaultPromptConventionName() string {
	return Codex
}

func Lookup(name string) (Definition, bool) {
	definition, ok := definitions[NormalizeName(name)]
	return definition, ok
}

func (d Definition) Available() bool {
	if !d.RequireExecutable {
		return true
	}
	executable, err := exec.LookPath(d.Executable)
	if err != nil {
		return false
	}
	if len(d.UsabilityCheck) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), availabilityCheckTimeout)
	defer cancel()
	return exec.CommandContext(ctx, executable, d.UsabilityCheck...).Run() == nil
}

func AgentNames() []string {
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ValidateAgentName(name string) error {
	normalized := NormalizeName(name)
	if normalized == "" {
		return nil
	}
	if _, ok := Lookup(normalized); !ok {
		return fmt.Errorf("unsupported agent harness %q", name)
	}
	return nil
}

func ValidateConsoleName(name string) error {
	normalized := NormalizeName(name)
	if normalized == "" {
		return nil
	}
	if normalized == Shell {
		return nil
	}
	_, ok := Lookup(normalized)
	if !ok {
		return fmt.Errorf("unsupported console harness %q", name)
	}
	return nil
}

func AvailableAgentDefinitions() []Definition {
	return availableDefinitions()
}

func AvailableAgentLabels() map[string]string {
	labels := map[string]string{}
	for _, definition := range AvailableAgentDefinitions() {
		labels[AgentHarnessLabel(definition.Name)] = "true"
	}
	return labels
}

func AgentDefinitionsFromLabels(labels map[string]string) []Definition {
	defs := make([]Definition, 0, len(definitions))
	for _, name := range AgentNames() {
		if labels[AgentHarnessLabel(name)] != "true" {
			continue
		}
		definition, ok := Lookup(name)
		if ok {
			defs = append(defs, definition)
		}
	}
	return defs
}

func AgentHarnessLabel(name string) string {
	return AgentHarnessLabelPrefix + NormalizeName(name)
}

func ConsoleDefinitionsFromAvailableAgents(agentDefinitions []Definition) []Definition {
	defs := make([]Definition, 0, len(agentDefinitions)+1)
	defs = append(defs, agentDefinitions...)
	defs = append(defs, shellDefinition())
	return defs
}

func availableDefinitions() []Definition {
	defs := make([]Definition, 0, len(definitions))
	for _, name := range AgentNames() {
		definition, ok := Lookup(name)
		if ok && definition.Available() {
			defs = append(defs, definition)
		}
	}
	return defs
}

func shellDefinition() Definition {
	return Definition{
		Name:        Shell,
		Kind:        KindShell,
		DisplayName: "Shell",
		Executable:  "",
	}
}

func ValidatePromptConventionName(name string) error {
	if _, ok := promptHarnesses[NormalizeName(name)]; !ok {
		return fmt.Errorf("unsupported prompt harness %q", name)
	}
	return nil
}

func DefaultAuthorEntrypoint(name string) (map[string]any, error) {
	return DefaultAuthorEntrypointWithArgs(name, Args{})
}

func DefaultAuthorEntrypointWithArgs(name string, args Args) (map[string]any, error) {
	if err := ValidateAgentName(name); err != nil {
		return nil, err
	}
	definition, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unsupported agent harness %q", name)
	}
	normalizedArgs, err := NormalizeArgs(args)
	if err != nil {
		return nil, err
	}
	command, err := defaultAuthorCommandWithArgs(definition.Name, normalizedArgs.For(definition.Name))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"argv":    []string{command},
		"cwd":     ".",
		"env":     map[string]string{},
		"shell":   true,
		"harness": definition.Name,
	}, nil
}

func DefaultConsoleEntrypointWithArgs(name string, args Args) (map[string]any, error) {
	normalized := NormalizeName(name)
	if normalized == "" {
		normalized = DefaultConsoleName()
	}
	if normalized == Shell {
		return defaultConsoleEntrypoint(Shell, DefaultShellConsoleCommand()), nil
	}
	if err := ValidateConsoleName(normalized); err != nil {
		return nil, err
	}
	definition, ok := Lookup(normalized)
	if !ok {
		return nil, fmt.Errorf("unsupported console harness %q", name)
	}
	normalizedArgs, err := NormalizeArgs(args)
	if err != nil {
		return nil, err
	}
	command, err := defaultConsoleCommandWithArgs(definition.Name, normalizedArgs.For(definition.Name))
	if err != nil {
		return nil, err
	}
	return defaultConsoleEntrypoint(definition.Name, command), nil
}

func defaultConsoleEntrypoint(harnessName string, command string) map[string]any {
	return map[string]any{
		"argv":    []string{command},
		"cwd":     ".",
		"env":     map[string]string{},
		"shell":   true,
		"harness": harnessName,
	}
}

func defaultAuthorCommandWithArgs(name string, args []string) (string, error) {
	switch NormalizeName(name) {
	case Codex:
		return DefaultCodexHookedCommandWithArgs(args), nil
	case Claude:
		return DefaultClaudeHookedCommandWithArgs(args), nil
	case Harness:
		return DefaultHarnessHookedCommandWithArgs(args), nil
	default:
		return "", fmt.Errorf("unsupported agent harness %q", name)
	}
}

func defaultConsoleCommandWithArgs(name string, args []string) (string, error) {
	switch NormalizeName(name) {
	case Codex:
		return DefaultCodexConsoleCommandWithArgs(args), nil
	case Claude:
		return DefaultClaudeConsoleCommandWithArgs(args), nil
	case Harness:
		return DefaultHarnessConsoleCommandWithArgs(args), nil
	default:
		return "", fmt.Errorf("unsupported console harness %q", name)
	}
}

func DefaultAgentCheckCommandWithArgs(name string, args []string) (string, error) {
	definition, ok := Lookup(name)
	if !ok || definition.CheckCommand == nil {
		return "", fmt.Errorf("unsupported agent harness %q", name)
	}
	return definition.CheckCommand(args), nil
}

func DefaultCodexHookedCommandWithArgs(args []string) string {
	return `flow hook codex start >/dev/null 2>&1 || true
prompt="$(flow fetch-prompt --harness codex)"
code=$?
if [ "$code" -eq 0 ]; then
  if [ -n "${` + envCodexHookProfile + `:-}" ]; then
    codex --profile "$` + envCodexHookProfile + `" --dangerously-bypass-hook-trust -c "projects.$PWD.trust_level=trusted"` + renderOptionalShellArgs(args) + ` "$prompt"
  else
    codex ` + renderShellArgs(DefaultCodexNativeHookArgs()) + ` --dangerously-bypass-hook-trust -c "projects.$PWD.trust_level=trusted"` + renderOptionalShellArgs(args) + ` "$prompt"
  fi
  code=$?
fi
flow hook codex stop >/dev/null 2>&1 || true
exit "$code"`
}

func DefaultCodexConsoleCommandWithArgs(args []string) string {
	return `flow hook codex start >/dev/null 2>&1 || true
if [ -n "${` + envCodexHookProfile + `:-}" ]; then
  codex --profile "$` + envCodexHookProfile + `" --dangerously-bypass-hook-trust -c "projects.$PWD.trust_level=trusted"` + renderOptionalShellArgs(args) + `
else
  codex ` + renderShellArgs(DefaultCodexNativeHookArgs()) + ` --dangerously-bypass-hook-trust -c "projects.$PWD.trust_level=trusted"` + renderOptionalShellArgs(args) + `
fi
code=$?
flow hook codex stop >/dev/null 2>&1 || true
exit "$code"`
}

// DefaultCodexNativeHookArgs renders codex's inline `-c` native-hook overrides
// from the table-driven renderer (the fallback path when no managed hook profile
// is exported). It delegates to CodexInlineHookArgs so the inline and managed
// forms share a single source of truth.
func DefaultCodexNativeHookArgs() []string {
	return CodexInlineHookArgs(Definition{
		Name:               Codex,
		HookEvents:         codexNativeHookEvents,
		HookTimeoutSeconds: codexNativeHookTimeoutSeconds,
	})
}

func renderShellArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "-c" {
			quoted = append(quoted, arg)
			continue
		}
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func renderOptionalShellArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return " " + renderShellArgs(args)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func DefaultCodexExecCommandWithArgs(args []string) string {
	return `prompt="$(flow fetch-prompt --harness codex)"
code=$?
if [ "$code" -eq 0 ]; then
  codex exec -c "projects.$PWD.trust_level=trusted"` + renderOptionalShellArgs(args) + ` "$prompt"
  code=$?
fi
exit "$code"`
}

func DefaultClaudeHookedCommandWithArgs(args []string) string {
	return `flow hook claude start >/dev/null 2>&1 || true
prompt="$(flow fetch-prompt --harness claude)"
code=$?
if [ "$code" -eq 0 ]; then
  if [ -n "${FLOW_CLAUDE_HOOK_SETTINGS:-}" ]; then
    claude --settings "$FLOW_CLAUDE_HOOK_SETTINGS" --dangerously-skip-permissions --permission-mode bypassPermissions` + renderOptionalShellArgs(args) + ` "$prompt"
  else
    claude --dangerously-skip-permissions --permission-mode bypassPermissions` + renderOptionalShellArgs(args) + ` "$prompt"
  fi
  code=$?
fi
flow hook claude stop >/dev/null 2>&1 || true
exit "$code"`
}

func DefaultClaudeConsoleCommandWithArgs(args []string) string {
	return `flow hook claude start >/dev/null 2>&1 || true
if [ -n "${FLOW_CLAUDE_HOOK_SETTINGS:-}" ]; then
  claude --settings "$FLOW_CLAUDE_HOOK_SETTINGS" --dangerously-skip-permissions --permission-mode bypassPermissions` + renderOptionalShellArgs(args) + `
else
  claude --dangerously-skip-permissions --permission-mode bypassPermissions` + renderOptionalShellArgs(args) + `
fi
code=$?
flow hook claude stop >/dev/null 2>&1 || true
exit "$code"`
}

func DefaultHarnessHookedCommandWithArgs(args []string) string {
	return `prompt="$(flow fetch-prompt --harness harness)"
code=$?
if [ "$code" -eq 0 ]; then
  if [ -n "${FLOW_HARNESS_HOOKS:-}" ]; then
    harness --hooks "$FLOW_HARNESS_HOOKS"` + renderOptionalShellArgs(args) + ` -p "$prompt"
  else
    harness` + renderOptionalShellArgs(args) + ` -p "$prompt"
  fi
  code=$?
fi
exit "$code"`
}

func DefaultHarnessConsoleCommandWithArgs(args []string) string {
	return `if [ -n "${FLOW_HARNESS_HOOKS:-}" ]; then
  harness --hooks "$FLOW_HARNESS_HOOKS"` + renderOptionalShellArgs(args) + `
else
  harness` + renderOptionalShellArgs(args) + `
fi
code=$?
exit "$code"`
}

func DefaultHarnessPrintCommandWithArgs(args []string) string {
	return `prompt="$(flow fetch-prompt --harness harness)"
code=$?
if [ "$code" -eq 0 ]; then
  harness` + renderOptionalShellArgs(args) + ` -p "$prompt"
  code=$?
fi
exit "$code"`
}

func DefaultShellConsoleCommand() string {
	return `exec "${SHELL:-/bin/sh}"`
}

func DefaultClaudePrintCommandWithArgs(args []string) string {
	return `prompt="$(flow fetch-prompt --harness claude)"
code=$?
if [ "$code" -eq 0 ]; then
  claude --dangerously-skip-permissions --permission-mode bypassPermissions` + renderOptionalShellArgs(args) + ` -p "$prompt"
  code=$?
fi
exit "$code"`
}

func StateForHook(tool string, event string) (string, error) {
	normalizedTool := NormalizeName(tool)
	if _, ok := Lookup(normalizedTool); !ok {
		return "", fmt.Errorf("unsupported harness hook tool %q", tool)
	}

	switch strings.ToLower(strings.TrimSpace(event)) {
	case "start", "started", "resume", "resumed", "working":
		return StateWorking, nil
	case "idle", "notification", "stop", "stopped", "waiting":
		return StateWaiting, nil
	default:
		return "", fmt.Errorf("unsupported %s hook event %q", normalizedTool, event)
	}
}

func DetectEntrypointHarness(argv []string) string {
	// Iterate definitions in sorted name order so a token mentioning more than
	// one harness resolves deterministically (alphabetical tie-break) instead of
	// depending on Go's randomized map iteration.
	names := AgentNames()
	for _, arg := range argv {
		lowered := strings.ToLower(arg)
		for _, name := range names {
			definition := definitions[name]
			if commandMentionsHarness(lowered, definition.Executable) {
				return definition.Name
			}
		}
	}
	if len(argv) == 0 {
		return DefaultAgentName()
	}
	return Agents
}

func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func commandMentionsHarness(command string, executable string) bool {
	fields := strings.FieldsFunc(command, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z'))
	})
	for _, field := range fields {
		if filepath.Base(field) == executable {
			return true
		}
	}
	return false
}
