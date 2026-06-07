package harness

import (
	"path/filepath"
	"strings"
)

// TrustPromptVisible reports whether the captured tmux pane is showing this
// harness's directory/workspace-trust dialog.
//
// Matching is data-driven and deliberately tolerant of TUI copy drift: every
// TrustPromptMarker must appear somewhere in the pane (case-insensitively), and
// the prompt's submit/confirm instruction (TrustPromptSubmitMarker) must be the
// LAST non-empty line on screen. Requiring the submit instruction to be the last
// line preserves the anti-injection invariant — pane content that merely quotes
// the dialog (e.g. an issue body) pushes the submit line off the end and so is
// not mistaken for a live prompt. A harness with no markers (clean -p, no
// scraped prompt) always returns false.
func (d Definition) TrustPromptVisible(pane string) bool {
	if len(d.TrustPromptMarkers) == 0 || d.TrustPromptSubmitMarker == "" {
		return false
	}
	lines := nonEmptyTrimmedLines(pane)
	if len(lines) == 0 {
		return false
	}
	if !containsFold(lines[len(lines)-1], d.TrustPromptSubmitMarker) {
		return false
	}
	for _, marker := range d.TrustPromptMarkers {
		if !containsFold(pane, marker) {
			return false
		}
	}
	return true
}

// TrustPromptForegroundAllowed reports whether a tmux foreground process name is
// one under which this harness's trust prompt may legitimately be shown (the
// wrapper shell, the node wrapper, or the harness binary). The approver consults
// this so it never sends the submit key into an unrelated foreground program.
func (d Definition) TrustPromptForegroundAllowed(foreground string) bool {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(foreground)))
	if name == "" {
		return false
	}
	for _, allowed := range d.TrustPromptForeground {
		if name == allowed {
			return true
		}
	}
	return false
}

// TrustPromptDefinitions returns the harness definitions whose interactive TUI
// shows a scraped trust prompt (those with TrustPromptMarkers set), in
// deterministic name-sorted order so callers iterate predictably.
func TrustPromptDefinitions() []Definition {
	defs := make([]Definition, 0, len(definitions))
	for _, name := range AgentNames() {
		def, ok := Lookup(name)
		if ok && len(def.TrustPromptMarkers) > 0 {
			defs = append(defs, def)
		}
	}
	return defs
}

// nonEmptyTrimmedLines splits a captured pane into its non-blank lines, each
// trimmed of surrounding whitespace. tmux pads panes with trailing blank rows;
// trimming them lets the matcher treat the prompt's instruction line as the last
// line regardless of pane height.
func nonEmptyTrimmedLines(value string) []string {
	rawLines := strings.Split(value, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// containsFold reports whether substr is within s, case-insensitively. It keeps
// the trust matcher robust to capitalization tweaks in TUI copy.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
