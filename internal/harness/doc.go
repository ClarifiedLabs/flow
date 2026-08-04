package harness

import (
	"context"
	"errors"
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
	KindHarness Kind = "harness"
	KindAgents  Kind = "agents"
	KindShell   Kind = "shell"
)

const (
	Harness = string(KindHarness)
	Agents  = string(KindAgents)
	Shell   = string(KindShell)

	AgentHarnessLabelPrefix = "agent.harness."

	StateWorking = "working"
	StateWaiting = "waiting"
)

const availabilityCheckTimeout = 5 * time.Second

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
	// HookState classifies a native-hook event into a session state signal. It
	// returns "" for events it does not explicitly recognize; callers apply the
	// activity default.
	HookState func(event string) string
	// ManagedFlags are the argv flags Flow reserves; user-supplied harness args
	// may not override them.
	ManagedFlags []string
	// HookEvents and HookEnvVar describe how this harness's native hooks are
	// wired, as the data source of truth for the hook-config renderer. HookEnvVar
	// is the env var that points the harness at the generated config.
	// HookTimeoutSeconds is the per-hook command timeout written into the config
	// (0 omits the field; harness uses 5).
	HookEvents         []HookEvent
	HookEnvVar         string
	HookTimeoutSeconds int
}

type Availability struct {
	Name        string
	DisplayName string
	Executable  string
	Path        string
	Available   bool
	Reason      string
	Error       string
}

// AvailableModels returns the harness's model catalog with every entry stamped
// with this harness's name and normalized (lowercased harness/sorted/deduped,
// qualified IDs filled). It returns nil for non-agent kinds (shell), which
// expose no Flow-selectable models.
func (d Definition) AvailableModels() ([]Model, error) {
	if d.Kind != KindHarness {
		return nil, nil
	}
	models, err := AvailableHarnessModels()
	if err != nil {
		return nil, err
	}
	stamped := CloneModels(models)
	for i := range stamped {
		stamped[i].Harness = d.Name
	}
	return NormalizeModels(stamped)
}

func (d Definition) Availability() Availability {
	status := Availability{
		Name:        d.Name,
		DisplayName: d.DisplayName,
		Executable:  d.Executable,
	}
	if !d.RequireExecutable {
		status.Available = true
		status.Reason = "not required"
		return status
	}

	executable, err := exec.LookPath(d.Executable)
	if err != nil {
		status.Reason = "executable not found"
		status.Error = err.Error()
		return status
	}
	status.Path = executable
	if len(d.UsabilityCheck) == 0 {
		status.Available = true
		status.Reason = "executable found"
		return status
	}

	ctx, cancel := context.WithTimeout(context.Background(), availabilityCheckTimeout)
	defer cancel()
	err = exec.CommandContext(ctx, executable, d.UsabilityCheck...).Run()
	if err != nil {
		status.Reason = "usability check failed"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status.Error = "timed out"
		} else {
			status.Error = err.Error()
		}
		return status
	}
	status.Available = true
	status.Reason = "usability check passed"
	return status
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

var definitions = map[string]Definition{
	Harness: {
		Name:              Harness,
		Kind:              KindHarness,
		DisplayName:       "Harness",
		Executable:        "harness",
		RequireExecutable: true,
		UsabilityCheck:    []string{"--check-model-proxy"},
		HookState:         mapHarnessNativeHook,
		ManagedFlags:      []string{"--hooks", "--session", "-p", "--prompt", "-i", "--initial-prompt"},
		HookEvents: []HookEvent{
			{Name: "SessionStart"},
			{Name: "UserPromptSubmit"},
			{Name: "PreToolUse", Matcher: "*"},
			{Name: "PostToolUse", Matcher: "*"},
			{Name: "PreCompact", Matcher: "*"},
			{Name: "PostCompact", Matcher: "*"},
			{Name: "Stop"},
		},
		HookEnvVar:         "FLOW_HARNESS_HOOKS",
		HookTimeoutSeconds: 5,
	},
}

var promptHarnesses = map[string]struct{}{
	Agents:  {},
	Harness: {},
}

func DefaultAgentName() string {
	return Harness
}

func DefaultConsoleName() string {
	return Harness
}

func DefaultPromptConventionName() string {
	return Harness
}

func Lookup(name string) (Definition, bool) {
	definition, ok := definitions[NormalizeName(name)]
	return definition, ok
}

func (d Definition) Available() bool {
	return d.Availability().Available
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

// BuildVersion returns the exact native build identifier Harness writes into
// session state. Capture reservation uses this value for build-matched indexing.
func BuildVersion(ctx context.Context, name string) (string, error) {
	definition, ok := Lookup(name)
	if !ok || definition.Executable == "" {
		return "", fmt.Errorf("unsupported harness %q", name)
	}
	output, err := exec.CommandContext(ctx, definition.Executable, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect %s build: %w", definition.Name, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || NormalizeName(fields[0]) != definition.Name || !strings.HasPrefix(fields[1], "v") || len(fields[1]) > 255 {
		return "", fmt.Errorf("inspect %s build: invalid version output %q", definition.Name, strings.TrimSpace(string(output)))
	}
	return fields[1], nil
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
	return DefaultAuthorEntrypointWithArgs(name, nil)
}

func DefaultAuthorEntrypointWithArgs(name string, args []string) (map[string]any, error) {
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
	command, err := defaultAuthorCommandWithArgs(definition.Name, normalizedArgs)
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

func DefaultConsoleEntrypointWithArgs(name string, args []string) (map[string]any, error) {
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
	command, err := defaultConsoleCommandWithArgs(definition.Name, normalizedArgs)
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
	case Harness:
		return DefaultHarnessHookedCommandWithArgs(args), nil
	default:
		return "", fmt.Errorf("unsupported agent harness %q", name)
	}
}

func defaultConsoleCommandWithArgs(name string, args []string) (string, error) {
	switch NormalizeName(name) {
	case Harness:
		return DefaultHarnessConsoleCommandWithArgs(args), nil
	default:
		return "", fmt.Errorf("unsupported console harness %q", name)
	}
}

// DefaultAgentCheckCommandWithArgs builds the interactive command used by
// Flow-owned reviewer/verifier jobs. The worker ends the harness only after a
// valid `flow complete` seal, leaving the terminal available when an agent
// needs to correct its verdict or an operator needs to continue the session.
func DefaultAgentCheckCommandWithArgs(name string, args []string) (string, error) {
	if NormalizeName(name) != Harness {
		return "", fmt.Errorf("unsupported agent harness %q", name)
	}
	return DefaultHarnessInteractiveCheckCommandWithArgs(args), nil
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

func DefaultHarnessHookedCommandWithArgs(args []string) string {
	return `prompt="$(flow fetch-prompt --harness harness)"
code=$?
if [ "$code" -eq 0 ]; then
  if [ -n "${FLOW_HARNESS_HOOKS:-}" ]; then
    harness --session "$FLOW_HARNESS_SESSION" --hooks "$FLOW_HARNESS_HOOKS"` + renderOptionalShellArgs(args) + ` -i "$prompt"
  else
    harness --session "$FLOW_HARNESS_SESSION"` + renderOptionalShellArgs(args) + ` -i "$prompt"
  fi
  code=$?
fi
exit "$code"`
}

func DefaultHarnessConsoleCommandWithArgs(args []string) string {
	return `if [ -n "${FLOW_HARNESS_HOOKS:-}" ]; then
  harness --session "$FLOW_HARNESS_SESSION" --hooks "$FLOW_HARNESS_HOOKS"` + renderOptionalShellArgs(args) + `
else
  harness --session "$FLOW_HARNESS_SESSION"` + renderOptionalShellArgs(args) + `
fi
code=$?
exit "$code"`
}

func DefaultHarnessPrintCommandWithArgs(args []string) string {
	return `prompt="$(flow fetch-prompt --harness harness)"
code=$?
if [ "$code" -eq 0 ]; then
  harness --session "$FLOW_HARNESS_SESSION"` + renderOptionalShellArgs(args) + ` -p "$prompt"
  code=$?
fi
exit "$code"`
}

func DefaultHarnessInteractiveCheckCommandWithArgs(args []string) string {
	return `prompt="$(flow fetch-prompt --harness harness)"
code=$?
if [ "$code" -eq 0 ]; then
  harness --session "$FLOW_HARNESS_SESSION"` + renderOptionalShellArgs(args) + ` -i "$prompt"
  code=$?
fi
exit "$code"`
}

func DefaultShellConsoleCommand() string {
	return `exec "${SHELL:-/bin/sh}"`
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
