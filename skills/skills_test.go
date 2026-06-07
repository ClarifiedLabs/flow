package flowskills

import (
	"strings"
	"testing"
)

func TestInstructionsReturnsBundledMarkdown(t *testing.T) {
	for _, name := range Names {
		contents, err := Instructions(name)
		if err != nil {
			t.Fatalf("instructions %s: %v", name, err)
		}
		if !strings.Contains(contents, "# Flow ") || !strings.Contains(contents, "## Workflow") {
			t.Fatalf("instructions %s missing role workflow:\n%s", name, contents)
		}
		if strings.Contains(contents, "---\nname:") {
			t.Fatalf("instructions %s still include skill frontmatter:\n%s", name, contents)
		}
	}
}

func TestInstructionsRejectsUnsupportedSkill(t *testing.T) {
	if _, err := Instructions("flow-ci"); err == nil {
		t.Fatal("Instructions accepted unsupported skill")
	}
}
