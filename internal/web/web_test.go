package web

import (
	"os"
	"strings"
	"testing"
)

func readModule(t *testing.T, name string) string {
	t.Helper()
	source, err := os.ReadFile("src/" + name)
	if err != nil {
		t.Fatalf("read css module %s: %v", name, err)
	}
	return string(source)
}

func TestGeneratedCSSServesEveryModule(t *testing.T) {
	actual, contentType, ok := Asset("app.css")
	if !ok {
		t.Fatal("app.css asset was not served")
	}
	if contentType != "text/css; charset=utf-8" {
		t.Fatalf("content type = %q, want text/css", contentType)
	}
	css := string(actual)
	for _, module := range cssModules {
		if !strings.Contains(css, "/* Generated from internal/web/src/"+module.Name+". */") {
			t.Fatalf("served CSS is missing module %s", module.Name)
		}
	}
}

// The whole scale is rem-based off one root percentage, so a px font-size
// anywhere silently opts that element out of the user's zoom.
func TestCSSFontSizesUseRootScale(t *testing.T) {
	if !strings.Contains(readModule(t, "tokens.module.css"), "font-size: 110%;") {
		t.Fatal("token sheet is missing the root font scale")
	}
	for _, module := range cssModules {
		css := readModule(t, module.Name)
		for i, line := range strings.Split(css, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "font-size:") && strings.Contains(trimmed, "px") {
				t.Fatalf("%s line %d bypasses the root font scale: %s", module.Name, i+1, trimmed)
			}
		}
	}
}

// Sizes and tracking are named by role. A component reaching for a raw rem
// value instead of a token is how a design system drifts.
func TestTypographyTokensAreDeclared(t *testing.T) {
	tokens := readModule(t, "tokens.module.css")
	for _, want := range []string{
		"--text-nano: 0.625rem;",
		"--text-meta: 0.6875rem;",
		"--text-quiet: 0.71875rem;",
		"--text-prose: 0.78125rem;",
		"--text-body: 0.8125rem;",
		"--text-title: 0.9375rem;",
		"--track-caps: 0.1em;",
		"--track-section: 0.11em;",
	} {
		if !strings.Contains(tokens, want) {
			t.Fatalf("token sheet missing %q", want)
		}
	}
}

// The lane grid reflows on its own; 260px is the width below which a card's
// title stops being readable at two lines.
func TestBoardLanesKeepTheirMinimumWidth(t *testing.T) {
	board := readModule(t, "board.module.css")
	if !strings.Contains(board, "grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));") {
		t.Fatal("board CSS is missing the 260px minimum lane width")
	}
}

// The dense table scrolls horizontally rather than wrapping: a wrapped table
// at this density is unreadable.
func TestBoardTableScrollsRatherThanWraps(t *testing.T) {
	table := readModule(t, "board-table.module.css")
	for _, want := range []string{"overflow-x: auto;", "min-width: 680px;"} {
		if !strings.Contains(table, want) {
			t.Fatalf("board table CSS missing %q", want)
		}
	}
}

// TestSidebarStaysWithinViewport keeps the light/dark theme switcher visible
// without scrolling when the main view pane is taller than the browser window.
// The sidebar must be viewport-bound (sticky, 100vh) rather than stretching to
// match the main pane's content height.
func TestSidebarStaysWithinViewport(t *testing.T) {
	css := readModule(t, "base.module.css")
	for _, want := range []string{
		".sidebar {",
		"position: sticky;",
		"top: 0;",
		"height: 100vh;",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("sidebar css missing %q (theme switcher would be hidden behind tall main content)", want)
		}
	}
}

// Each component sheet is scoped to its own element, so a rule written for one
// component cannot reach another. Tokens stay global.
func TestCSSModulesScopeToTheirOwnElement(t *testing.T) {
	css, _, ok := Asset("app.css")
	if !ok {
		t.Fatal("app.css asset was not served")
	}
	generated := string(css)
	for _, want := range []string{
		"flow-app .shell {",
		"flow-app .nav a {",
		"flow-board .surface.lanes {",
		"flow-task-card .title {",
		"flow-task-card:hover {",
		"flow-attention-strip .row {",
		"flow-board-table table {",
		"flow-epic .member {",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated css missing scoped selector %q", want)
		}
	}
	for _, want := range []string{":root {", ":root[data-theme=\"light\"] {", ":root:not([data-theme=\"dark\"]) {", "body {"} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated css missing global selector %q", want)
		}
	}
	for _, forbidden := range []string{"\n.shell {", "\n.nav a {", "\n.title {", "flow-app :root", "\n:host"} {
		if strings.Contains(generated, forbidden) {
			t.Fatalf("generated css contains unscoped selector %q", forbidden)
		}
	}
}

// A custom element is display: inline until told otherwise, which collapses
// every layout built on it.
func TestComponentSheetsDeclareTheirOwnDisplay(t *testing.T) {
	for _, module := range cssModules {
		if module.Scope == "" || module.Scope == "flow-app" {
			continue
		}
		css := readModule(t, module.Name)
		if strings.TrimSpace(css) == "" {
			continue
		}
		if !strings.Contains(css, ":host {") {
			t.Fatalf("%s does not style its own element box; custom elements are display: inline by default", module.Name)
		}
	}
}
