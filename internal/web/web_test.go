package web

import (
	"os"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/web/webassetbuild"
)

func TestGeneratedCSSMatchesModuleSource(t *testing.T) {
	source, err := os.ReadFile("src/app.module.css")
	if err != nil {
		t.Fatalf("read css module source: %v", err)
	}
	actual, contentType, ok := Asset("app.css")
	if !ok {
		t.Fatal("app.css asset was not served")
	}
	if contentType != "text/css; charset=utf-8" {
		t.Fatalf("content type = %q, want text/css", contentType)
	}
	expected := webassetbuild.BuildCSS(source)
	if string(actual) != string(expected) {
		t.Fatal("served CSS does not match module source")
	}
}

func TestCSSFontSizesUseRootScale(t *testing.T) {
	source, err := os.ReadFile("src/app.module.css")
	if err != nil {
		t.Fatalf("read css module source: %v", err)
	}
	css := string(source)
	if !strings.Contains(css, "font-size: 110%;") {
		t.Fatal("CSS is missing the root font scale")
	}
	for i, line := range strings.Split(css, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "font-size:") && strings.Contains(trimmed, "px") {
			t.Fatalf("font-size on line %d bypasses the root font scale: %s", i+1, trimmed)
		}
	}
}

// TestSidebarStaysWithinViewport keeps the light/dark theme switcher visible
// without scrolling when the main view pane is taller than the browser window.
// The sidebar must be viewport-bound (sticky, 100vh) rather than stretching to
// match the main pane's content height.
func TestSidebarStaysWithinViewport(t *testing.T) {
	source, err := os.ReadFile("src/app.module.css")
	if err != nil {
		t.Fatalf("read css module source: %v", err)
	}
	css := string(source)
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

func TestCSSModuleBuildScopesSelectors(t *testing.T) {
	source, err := os.ReadFile("src/app.module.css")
	if err != nil {
		t.Fatalf("read css module source: %v", err)
	}
	css := string(webassetbuild.BuildCSS(source))
	for _, want := range []string{
		"flow-app .shell {",
		"flow-app .nav a {",
		"flow-app .pill.warn {",
		"flow-app .summary-grid div,",
		"flow-app .check-row {",
		"flow-app .issue-detail-grid {",
		"flow-app .issue-detail-lifecycle {",
		"flow-app .issue-detail-column .issue-form {",
		"flow-app .issue-form .wide,",
		"flow-app .form-actions {",
		"flow-app .console-actions {",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("generated css missing scoped selector %q", want)
		}
	}
	for _, want := range []string{":root {", ":root[data-theme=\"light\"] {", ":root:not([data-theme=\"dark\"]) {", "body {"} {
		if !strings.Contains(css, want) {
			t.Fatalf("generated css missing global selector %q", want)
		}
	}
	for _, forbidden := range []string{"\n.shell {", "\n.nav a {", "\n.pill.warn {", "flow-app :root"} {
		if strings.Contains(css, forbidden) {
			t.Fatalf("generated css contains unscoped selector %q", forbidden)
		}
	}
}
