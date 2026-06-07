package handoff

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var RequiredSections = []string{
	"Current Goal",
	"Completed Work",
	"Remaining Work",
	"Tests Run and Results",
	"Failed Approaches",
	"Important Files and Commands",
	"Next Recommended Action",
}

type TemplateInput struct {
	IssueID               string
	ChangeID              string
	SessionID             string
	Branch                string
	Base                  string
	UpdatedAt             time.Time
	CurrentGoal           string
	CompletedWork         string
	RemainingWork         string
	TestsRun              string
	FailedApproaches      string
	ImportantFiles        string
	NextRecommendedAction string
}

func RenderTemplate(input TemplateInput) string {
	updatedAt := input.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}

	var builder strings.Builder
	builder.WriteString("# Flow Handoff\n\n")
	writeMetadata(&builder, "Issue", input.IssueID)
	writeMetadata(&builder, "Change", input.ChangeID)
	writeMetadata(&builder, "Session", input.SessionID)
	writeMetadata(&builder, "Branch", input.Branch)
	writeMetadata(&builder, "Base", input.Base)
	writeMetadata(&builder, "Updated", updatedAt.Format(time.RFC3339))
	builder.WriteString("\n")

	writeSection(&builder, "Current Goal", input.CurrentGoal, "Describe the goal for the current authoring session.")
	writeSection(&builder, "Completed Work", input.CompletedWork, "List the work already completed.")
	writeSection(&builder, "Remaining Work", input.RemainingWork, "List the work that still needs to be done.")
	writeSection(&builder, "Tests Run and Results", input.TestsRun, "List commands run and whether they passed or failed.")
	writeSection(&builder, "Failed Approaches", input.FailedApproaches, "Record approaches that were rejected and why.")
	writeSection(&builder, "Important Files and Commands", input.ImportantFiles, "List files and commands the next session should inspect first.")
	writeSection(&builder, "Next Recommended Action", input.NextRecommendedAction, "State the next concrete action.")

	return strings.TrimRight(builder.String(), "\n") + "\n"
}

func Validate(contents string) error {
	if strings.TrimSpace(contents) == "" {
		return errors.New("handoff is empty")
	}
	for _, section := range RequiredSections {
		if !hasSection(contents, section) {
			return fmt.Errorf("handoff is missing %q section", section)
		}
		if !hasActionableSectionContent(contents, section) {
			return fmt.Errorf("handoff section %q requires non-placeholder content", section)
		}
	}

	return nil
}

func Summary(contents string) string {
	for _, line := range linesInSection(contents, "Current Goal") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "TODO:") {
			continue
		}
		return trimmed
	}

	for _, line := range strings.Split(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "TODO:") {
			continue
		}
		return trimmed
	}

	return ""
}

func writeMetadata(builder *strings.Builder, key string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "unknown"
	}
	fmt.Fprintf(builder, "- %s: %s\n", key, value)
}

func writeSection(builder *strings.Builder, title string, value string, placeholder string) {
	builder.WriteString("## ")
	builder.WriteString(title)
	builder.WriteString("\n\n")
	value = strings.TrimSpace(value)
	if value == "" {
		value = "TODO: " + placeholder
	}
	builder.WriteString(value)
	builder.WriteString("\n\n")
}

func hasSection(contents string, title string) bool {
	needle := "## " + strings.TrimSpace(title)
	for _, line := range strings.Split(contents, "\n") {
		if strings.TrimSpace(line) == needle {
			return true
		}
	}

	return false
}

func hasActionableSectionContent(contents string, title string) bool {
	for _, line := range linesInSection(contents, title) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.EqualFold(trimmed, "todo") || strings.HasPrefix(strings.ToUpper(trimmed), "TODO:") {
			continue
		}
		return true
	}

	return false
}

func linesInSection(contents string, title string) []string {
	needle := "## " + strings.TrimSpace(title)
	var lines []string
	inSection := false
	for _, line := range strings.Split(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if trimmed == needle {
				inSection = true
				continue
			}
			if inSection {
				break
			}
		}
		if inSection {
			lines = append(lines, line)
		}
	}

	return lines
}
