package harness

import (
	"encoding/json"
	"fmt"
)

// hookCommand is one native-hook handler invocation. TimeoutSeconds is omitted
// when 0 so harnesses with no per-hook timeout share a single struct.
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
// the harness's liveness signal (e.g. "flow hook harness ingest").
func (d Definition) hookIngestCommand() string {
	return "flow hook " + d.Name + " ingest"
}

func (d Definition) hookCommand() hookCommand {
	return hookCommand{Type: "command", Command: d.hookIngestCommand(), TimeoutSeconds: d.HookTimeoutSeconds}
}

// RenderHookConfig renders the native-hook configuration file content for a
// harness, driven entirely by its HookEvents/HookTimeoutSeconds. It is the
// single source of truth for the per-job hook files; "json" is the only
// supported format.
func RenderHookConfig(def Definition) ([]byte, error) {
	switch def.HookFormat {
	case "json":
		return renderHookJSON(def)
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
