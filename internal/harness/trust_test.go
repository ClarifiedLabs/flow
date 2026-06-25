package harness

import (
	"strings"
	"testing"
)

// codexTrustPane and claudeTrustPane are real captured trust-prompt panes (the
// strings the worker scrapes out of tmux). They are the table-test fixtures for
// the data-driven matcher.
func codexTrustPane() string {
	return `> You are in /Users/tester/.local/share/flow/projects/p-1/workers/local/jobs/j-1/repo

  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection. Trusting the directory allows project-local config,
  hooks, and exec policies to load.

  1. Yes, continue
  2. No, quit

  Press enter to continue
`
}

func claudeTrustPane() string {
	return `Accessing workspace:

 /Users/tester/.local/share/flow/projects/p-1/workers/local/jobs/j-1/repo

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 > 1. Yes, I trust this folder
   2. No, exit

 Enter to confirm / Esc to cancel
`
}

func mustLookup(t *testing.T, name string) Definition {
	t.Helper()
	def, ok := Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q) returned no definition", name)
	}
	return def
}

func TestTrustPromptVisibleRecognizesActivePrompts(t *testing.T) {
	if !mustLookup(t, Codex).TrustPromptVisible(codexTrustPane()) {
		t.Fatal("codex definition did not recognize its active trust prompt")
	}
	if !mustLookup(t, Claude).TrustPromptVisible(claudeTrustPane()) {
		t.Fatal("claude definition did not recognize its active trust prompt")
	}
}

// TestTrustPromptVisibleToleratesCopyDrift is the core robustness guarantee: a
// pane whose copy has drifted in ways that leave the marker substrings present
// (case changes, reworded surrounding text, a different last-line separator)
// still matches, so a TUI copy tweak does not silently break dismissal.
func TestTrustPromptVisibleToleratesCopyDrift(t *testing.T) {
	codexDrift := `> You are now working in /Users/tester/.local/share/flow/projects/p-1/repo

  DO YOU TRUST THE CONTENTS of this workspace? Untrusted contents carry prompt-injection risk.

  1. Yes, continue and proceed
  2. No, quit now

  Press ENTER to continue
`
	if !mustLookup(t, Codex).TrustPromptVisible(codexDrift) {
		t.Fatal("codex definition failed to match a trust prompt with drifted copy")
	}

	// Real drift observed in claude 2.1.183: the last line uses a middle-dot
	// separator ("·") instead of a slash, and the safety-check sentence is
	// reworded. The marker substrings still appear, so it must still match.
	claudeDrift := `Accessing workspace:

 /Users/tester/.local/share/flow/projects/p-1/repo

 Quick safety check — is this a project you trust?

 Claude Code can read, edit, and execute files here.

 ❯ 1. Yes, I trust this folder
   2. No, exit

 Enter to confirm · Esc to cancel
`
	if !mustLookup(t, Claude).TrustPromptVisible(claudeDrift) {
		t.Fatal("claude definition failed to match a trust prompt with drifted copy")
	}
}

// TestTrustPromptVisibleRejectsEmbeddedPromptText preserves the anti-injection
// invariant: when the trust-prompt copy is merely quoted inside larger pane
// content (e.g. an issue body), the prompt's submit instruction is no longer
// the last line on screen, so it must not be mistaken for a live prompt.
func TestTrustPromptVisibleRejectsEmbeddedPromptText(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  Definition
		pane string
	}{
		{"codex", mustLookup(t, Codex), codexTrustPane()},
		{"claude", mustLookup(t, Claude), claudeTrustPane()},
	} {
		pane := "Flow role instructions (flow-author):\n\n# Flow Author\n\nIssue: i-0005\n\n" +
			tc.pane + "\n\nacceptance_criteria:\n" + tc.name + " no longer gets stuck at the trust prompt\n"
		if tc.def.TrustPromptVisible(pane) {
			t.Fatalf("%s definition matched trust-prompt text embedded in issue content", tc.name)
		}
	}
}

func TestTrustPromptVisibleRejectsNonTrustPanes(t *testing.T) {
	for _, name := range []string{Codex, Claude} {
		def := mustLookup(t, name)
		if def.TrustPromptVisible("> Type your message\n") {
			t.Fatalf("%s definition matched an agent input pane", name)
		}
		if def.TrustPromptVisible("") {
			t.Fatalf("%s definition matched an empty pane", name)
		}
		if def.TrustPromptVisible("starting agent...\n") {
			t.Fatalf("%s definition matched a bootstrapping pane", name)
		}
	}
}

// TestTrustPromptVisibleCrossHarnessExclusive guards that one harness's matcher
// does not fire on another harness's trust prompt (the markers are distinct), so
// the per-harness one-shot latch stays meaningful.
func TestTrustPromptVisibleCrossHarnessExclusive(t *testing.T) {
	if mustLookup(t, Codex).TrustPromptVisible(claudeTrustPane()) {
		t.Fatal("codex definition matched claude's trust prompt")
	}
	if mustLookup(t, Claude).TrustPromptVisible(codexTrustPane()) {
		t.Fatal("claude definition matched codex's trust prompt")
	}
}

// TestTrustPromptVisibleFalseForNonScrapedHarness asserts a harness with no
// scraped trust prompt (the harness CLI uses a clean -i) never reports a prompt.
func TestTrustPromptVisibleFalseForNonScrapedHarness(t *testing.T) {
	def := mustLookup(t, Harness)
	if len(def.TrustPromptMarkers) != 0 {
		t.Fatalf("harness definition unexpectedly carries trust markers: %v", def.TrustPromptMarkers)
	}
	if def.TrustPromptVisible(codexTrustPane()) || def.TrustPromptVisible(claudeTrustPane()) {
		t.Fatal("non-scraped harness definition reported a trust prompt")
	}
}

func TestTrustPromptForegroundAllowed(t *testing.T) {
	codex := mustLookup(t, Codex)
	claude := mustLookup(t, Claude)

	// The wrapper shell, the node wrapper, and the harness binary may legitimately
	// be the foreground while the trust prompt is up.
	for _, fg := range []string{"codex", "node", "bash", "zsh", "sh", "fish", "/opt/homebrew/bin/codex"} {
		if !codex.TrustPromptForegroundAllowed(fg) {
			t.Fatalf("codex denied legitimate foreground %q", fg)
		}
	}
	for _, fg := range []string{"claude", "node", "bash", "zsh", "/opt/claude/bin/claude"} {
		if !claude.TrustPromptForegroundAllowed(fg) {
			t.Fatalf("claude denied legitimate foreground %q", fg)
		}
	}

	// A non-shell, non-agent foreground (a build/tool process) must be denied so
	// the approver never types into an unrelated program; the cross-agent binary
	// is denied too.
	for _, fg := range []string{"go", "python3", "make", "vim", ""} {
		if codex.TrustPromptForegroundAllowed(fg) {
			t.Fatalf("codex approved illegitimate foreground %q", fg)
		}
		if claude.TrustPromptForegroundAllowed(fg) {
			t.Fatalf("claude approved illegitimate foreground %q", fg)
		}
	}
	if codex.TrustPromptForegroundAllowed("claude") {
		t.Fatal("codex approved the claude binary as foreground")
	}
	if claude.TrustPromptForegroundAllowed("codex") {
		t.Fatal("claude approved the codex binary as foreground")
	}
}

func TestTrustPromptDefinitions(t *testing.T) {
	defs := TrustPromptDefinitions()
	if len(defs) != 2 {
		t.Fatalf("TrustPromptDefinitions returned %d definitions, want 2", len(defs))
	}
	// Deterministic (sorted) order: codex before claude.
	if defs[0].Name != Claude || defs[1].Name != Codex {
		t.Fatalf("TrustPromptDefinitions order = [%s %s], want [claude codex]", defs[0].Name, defs[1].Name)
	}
	for _, def := range defs {
		if len(def.TrustPromptMarkers) == 0 {
			t.Fatalf("%s in TrustPromptDefinitions has no markers", def.Name)
		}
		if strings.TrimSpace(def.TrustPromptSubmitMarker) == "" {
			t.Fatalf("%s in TrustPromptDefinitions has no submit marker", def.Name)
		}
		if strings.TrimSpace(def.TrustPromptSubmitKey) == "" {
			t.Fatalf("%s in TrustPromptDefinitions has no submit key", def.Name)
		}
	}
}
