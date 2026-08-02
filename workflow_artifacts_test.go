package flow_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRootWorkflowSummaryIsIgnoredAndUntracked(t *testing.T) {
	ignore, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"/.flow/session/\n", "/SUMMARY.md\n"} {
		if !strings.Contains(string(ignore), pattern) {
			t.Fatalf(".gitignore must contain reserved workflow pattern %q", strings.TrimSpace(pattern))
		}
	}
	if _, err := os.Stat(".git"); os.IsNotExist(err) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("git", "ls-files", "-z", "--", "SUMMARY.md", ".flow/session").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect tracked workflow artifacts: %v\n%s", err, output)
	}
	if len(output) != 0 {
		t.Fatalf("workflow artifacts must not be tracked: %q", strings.ReplaceAll(string(output), "\x00", ", "))
	}
}
