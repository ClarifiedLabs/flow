package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// hookCommand is one native-hook handler invocation. TimeoutSeconds is omitted
// when 0 so claude (no timeout) and harness (timeout 5) share a single struct
// and stay byte-identical to the pre-unification per-harness encoders.
type hookCommand struct {
	Type           string `json:"type"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type hookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

type hookSettings struct {
	Hooks map[string][]hookMatcher `json:"hooks"`
}

// hookIngestCommand is the flow subcommand a native hook event invokes to report
// the harness's liveness signal (e.g. "flow hook claude ingest").
func (d Definition) hookIngestCommand() string {
	return "flow hook " + d.Name + " ingest"
}

func (d Definition) hookCommand() hookCommand {
	return hookCommand{Type: "command", Command: d.hookIngestCommand(), TimeoutSeconds: d.HookTimeoutSeconds}
}

// RenderHookConfig renders the native-hook configuration file content for a
// harness, driven entirely by its HookEvents/HookFormat/HookTimeoutSeconds. It
// is the single source of truth for the per-job hook files: "json" produces the
// claude/harness settings file and "toml" produces codex's managed profile. The
// "inline" format (codex's `-c` fallback) is produced by CodexInlineHookArgs and
// returned here joined for completeness.
func RenderHookConfig(def Definition) ([]byte, error) {
	switch def.HookFormat {
	case "json":
		return renderHookJSON(def)
	case "toml":
		return renderHookTOML(def), nil
	case "inline":
		return []byte(strings.Join(CodexInlineHookArgs(def), " ")), nil
	default:
		return nil, fmt.Errorf("harness %q has unsupported hook format %q", def.Name, def.HookFormat)
	}
}

func renderHookJSON(def Definition) ([]byte, error) {
	command := def.hookCommand()
	hooks := make(map[string][]hookMatcher, len(def.HookEvents))
	for _, event := range def.HookEvents {
		hooks[event.Name] = []hookMatcher{{Matcher: event.Matcher, Hooks: []hookCommand{command}}}
	}
	data, err := json.MarshalIndent(hookSettings{Hooks: hooks}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s hook settings: %w", def.Name, err)
	}
	return append(data, '\n'), nil
}

// renderHookTOML renders codex's managed profile (loaded via `codex --profile`).
// It enables the hooks feature and lists every HookEvent under [hooks]. The
// per-event value reuses codexHookValue so the managed file and the inline `-c`
// fallback can never drift.
func renderHookTOML(def Definition) []byte {
	var b strings.Builder
	b.WriteString("features.hooks = true\n\n[hooks]\n")
	for _, event := range def.HookEvents {
		b.WriteString(event.Name)
		b.WriteString(" = ")
		b.WriteString(codexHookValue(def, event))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// CodexInlineHookArgs renders codex's native-hook config as inline `-c key=value`
// argv tokens — the fallback path used when no managed profile env var is set.
// It is driven by the same HookEvents table as RenderHookConfig so the inline
// and managed forms always agree.
func CodexInlineHookArgs(def Definition) []string {
	args := make([]string, 0, 2+2*len(def.HookEvents))
	args = append(args, "-c", "features.hooks=true")
	for _, event := range def.HookEvents {
		args = append(args, "-c", "hooks."+event.Name+"="+codexHookValue(def, event))
	}
	return args
}

// codexHookValue renders the TOML array-of-tables value codex expects for one
// hook event. The compact, space-free form is what codex's TOML parser accepts
// both as a `-c` override value and inside the managed profile file.
func codexHookValue(def Definition, event HookEvent) string {
	var b strings.Builder
	b.WriteString("[{")
	if event.Matcher != "" {
		b.WriteString(`matcher="`)
		b.WriteString(event.Matcher)
		b.WriteString(`",`)
	}
	b.WriteString(`hooks=[{type="command",command="`)
	b.WriteString(def.hookIngestCommand())
	b.WriteString(`"`)
	if def.HookTimeoutSeconds > 0 {
		fmt.Fprintf(&b, ",timeout=%d", def.HookTimeoutSeconds)
	}
	b.WriteString("}]}]")
	return b.String()
}
