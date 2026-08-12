package web

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func readModule(t *testing.T, name string) string {
	t.Helper()
	source, err := os.ReadFile("src/" + name)
	if err != nil {
		t.Fatalf("read css module %s: %v", name, err)
	}
	return string(source)
}

// The index's ?v= cache-buster must reflect modules in subdirectories —
// assets/elements/ holds most of the UI — but ignore pinned vendor files.
// Regression: computeAssetVersion used a non-recursive assets/*.js glob, so
// edits under assets/elements/ never bumped the version.
func TestComputeAssetVersionWalksSubdirectories(t *testing.T) {
	css := func() ([]byte, error) { return []byte("css"), nil }
	js := func(data string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(data)} }

	base := fstest.MapFS{
		"assets/app.js":           js("app"),
		"assets/elements/card.js": js("card-v1"),
		"assets/vendor/lib.js":    js("vendor-v1"),
	}

	elementEdited := fstest.MapFS{
		"assets/app.js":           js("app"),
		"assets/elements/card.js": js("card-v2"),
		"assets/vendor/lib.js":    js("vendor-v1"),
	}

	vendorEdited := fstest.MapFS{
		"assets/app.js":           js("app"),
		"assets/elements/card.js": js("card-v1"),
		"assets/vendor/lib.js":    js("vendor-v2"),
	}

	baseVersion := computeAssetVersionFrom(base, css)
	if got := computeAssetVersionFrom(elementEdited, css); got == baseVersion {
		t.Fatal("version did not change when an assets/elements/ module changed")
	}
	if got := computeAssetVersionFrom(vendorEdited, css); got != baseVersion {
		t.Fatal("version changed when only an assets/vendor/ module changed")
	}
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

// TestTopbarSticksToTop keeps the navigation bar pinned above scrolling
// content: the bar must be sticky with an opaque background and a bottom
// border, and the shell must be a single column (no permanent nav column).
func TestTopbarSticksToTop(t *testing.T) {
	css := readModule(t, "chrome.module.css")
	for _, want := range []string{
		".topbar {",
		"position: sticky;",
		"top: 0;",
		"background: var(--panel);",
		"border-bottom: 1px solid var(--line);",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("topbar css missing %q (nav bar would scroll away or show content through)", want)
		}
	}
	if strings.Contains(css, ".sidebar {") {
		t.Fatal("chrome css still styles the removed sidebar")
	}
}

// The nav dropdown panel once painted with var(--surface, #111) and
// var(--border, #333); neither token exists, so the dark fallbacks stayed
// active in light mode while the nav foregrounds switched to the light
// palette. The panel must use the same defined chrome tokens as the top bar.
func TestNavPanelUsesDefinedThemeTokens(t *testing.T) {
	css := readModule(t, "nav.module.css")
	start := strings.Index(css, ".nav-panel {")
	if start < 0 {
		t.Fatal("nav css is missing the .nav-panel rule")
	}
	rule := css[start:]
	end := strings.Index(rule, "\n}")
	if end < 0 {
		t.Fatal("nav css .nav-panel rule is not closed")
	}
	rule = rule[:end]
	for _, want := range []string{"background: var(--panel);", "border: 1px solid var(--line);"} {
		if !strings.Contains(rule, want) {
			t.Fatalf(".nav-panel missing %q; the panel must follow the active theme", want)
		}
	}
	for _, undefined := range []string{"--surface", "--border"} {
		if strings.Contains(rule, undefined) {
			t.Fatalf(".nav-panel references undefined token %s; its dark fallback stays active in light mode", undefined)
		}
	}
}

// The project picker had the same bug as the nav panel: var(--surface, #111),
// var(--border, #333), and var(--surface-raised, rgba(255, 255, 255, 0.06))
// reference tokens that do not exist, so the dark fallbacks stayed active in
// light mode while the picker foregrounds switched to the light palette. The
// panel, item hover, and card badge must use defined chrome tokens.
func TestProjectPickerUsesDefinedThemeTokens(t *testing.T) {
	rules := []struct {
		module    string
		selector  string
		want      []string
		undefined []string
	}{
		{"nav.module.css", ".project-picker-menu {", []string{"background: var(--panel);", "border: 1px solid var(--line);"}, []string{"--surface", "--border"}},
		{"nav.module.css", ".project-picker-item:hover {", []string{"background: var(--panel-2);"}, []string{"--surface-raised"}},
		{"shared.module.css", ".card-project-badge {", []string{"border: 1px solid var(--line);"}, []string{"--border"}},
	}
	for _, check := range rules {
		css := readModule(t, check.module)
		start := strings.Index(css, check.selector)
		if start < 0 {
			t.Fatalf("%s is missing the %s rule", check.module, check.selector)
		}
		rule := css[start:]
		end := strings.Index(rule, "\n}")
		if end < 0 {
			t.Fatalf("%s %s rule is not closed", check.module, check.selector)
		}
		rule = rule[:end]
		for _, want := range check.want {
			if !strings.Contains(rule, want) {
				t.Fatalf("%s missing %q; the project picker must follow the active theme", check.selector, want)
			}
		}
		for _, undefined := range check.undefined {
			if strings.Contains(rule, undefined) {
				t.Fatalf("%s references undefined token %s; its dark fallback stays active in light mode", check.selector, undefined)
			}
		}
	}
}

// The chrome tokens the top bar, nav panel, and project picker share must be
// defined in every theme block — default dark, explicit light, and system
// light — or one mode silently loses the panel background, border, or hover.
func TestChromeTokensDefinedInEveryTheme(t *testing.T) {
	tokens := readModule(t, "tokens.module.css")
	for _, block := range []string{":root {", ":root[data-theme=\"light\"] {", ":root:not([data-theme=\"dark\"]) {"} {
		start := strings.Index(tokens, block)
		if start < 0 {
			t.Fatalf("token sheet is missing the %s block", block)
		}
		section := tokens[start:]
		end := strings.Index(section, "\n}")
		if end < 0 {
			t.Fatalf("token sheet %s block is not closed", block)
		}
		section = section[:end]
		for _, token := range []string{"--panel:", "--panel-2:", "--line:"} {
			if !strings.Contains(section, token) {
				t.Fatalf("token sheet %s block does not define %s", block, token)
			}
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
