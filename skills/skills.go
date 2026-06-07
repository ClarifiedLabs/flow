package flowskills

import (
	"embed"
	"fmt"
	"strings"
)

const (
	AuthorSkill   = "flow-author"
	ReviewerSkill = "flow-reviewer"
	VerifierSkill = "flow-verifier"
)

var Names = []string{AuthorSkill, ReviewerSkill, VerifierSkill}

//go:embed flow-author.md flow-reviewer.md flow-verifier.md
var bundled embed.FS

func Instructions(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("skill name is required")
	}
	if !validName(name) {
		return "", fmt.Errorf("unsupported Flow skill %q", name)
	}
	contents, err := bundled.ReadFile(name + ".md")
	if err != nil {
		return "", fmt.Errorf("read bundled skill %s: %w", name, err)
	}
	return strings.TrimSpace(string(contents)), nil
}

func validName(name string) bool {
	for _, candidate := range Names {
		if name == candidate {
			return true
		}
	}
	return false
}
