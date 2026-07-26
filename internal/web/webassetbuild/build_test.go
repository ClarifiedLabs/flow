package webassetbuild

import (
	"strings"
	"testing"
)

func TestBuildCSSScopesSelectors(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		want      []string
		forbidden []string
	}{
		{
			name:   "style rule",
			source: ".card {\n  color: red;\n}",
			want:   []string{"flow-app .card {"},
		},
		{
			name: "conditional group",
			source: strings.Join([]string{
				"@media (max-width: 760px) {",
				"  .shell {",
				"    grid-template-columns: 1fr;",
				"  }",
				"}",
			}, "\n"),
			want: []string{"flow-app .shell {"},
		},
		{
			name: "raw at-rules",
			source: strings.Join([]string{
				"@keyframes rise {",
				"  0% { opacity: 0; }",
				"  to { opacity: 1; }",
				"}",
				"@font-face {",
				"  font-family: \"Example\";",
				"}",
				".card { animation: rise 0.2s ease both; }",
			}, "\n"),
			want:      []string{"@keyframes rise {", "  0% {", "  to {", "@font-face {", "flow-app .card {"},
			forbidden: []string{"flow-app 0%", "flow-app to", "flow-app font-family"},
		},
		{
			name:      "globals",
			source:    ":root {\n  --bg: black;\n}\nbody {\n  margin: 0;\n}",
			want:      []string{":root {", "body {"},
			forbidden: []string{"flow-app :root", "flow-app body"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			css := string(BuildCSS([]byte(tt.source)))
			for _, want := range tt.want {
				if !strings.Contains(css, want) {
					t.Fatalf("generated css missing %q:\n%s", want, css)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(css, forbidden) {
					t.Fatalf("generated css contains %q:\n%s", forbidden, css)
				}
			}
		})
	}
}

// A comment above a rule must not be swept into that rule's selector list: the
// scope prefix would turn it into an invalid selector and browsers drop the
// whole rule, which is invisible until something silently stops being styled.
func TestBuildModulesLeavesCommentsAlone(t *testing.T) {
	source := strings.Join([]string{
		"/* The lane grid reflows on its own; 260px is the width below",
		"   which a card's title stops being readable. */",
		".lanes {",
		"  display: grid;",
		"}",
		"",
		"/* single line */",
		".other {",
		"  color: red;",
		"}",
	}, "\n")

	css := string(BuildModules([]Module{{Name: "board.module.css", Scope: "flow-board", Source: []byte(source)}}))
	for _, want := range []string{"flow-board .lanes {", "flow-board .other {"} {
		if !strings.Contains(css, want) {
			t.Fatalf("generated css missing %q:\n%s", want, css)
		}
	}
	if strings.Contains(css, "flow-board /*") || strings.Contains(css, "flow-board    which") {
		t.Fatalf("comment lines were scoped as selectors:\n%s", css)
	}
}
