package flow_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseWorkflowUsesSecretForHomebrewTapAppClientID(t *testing.T) {
	workflow, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)

	const want = "client-id: ${{ secrets.HOMEBREW_TAP_APP_CLIENT_ID }}"
	if !strings.Contains(text, want) {
		t.Fatalf("release workflow should use %q", want)
	}

	for _, forbidden := range []string{
		"vars.HOMEBREW_TAP_APP_CLIENT_ID",
		"app-id: ${{ secrets.HOMEBREW_TAP_APP_CLIENT_ID }}",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("release workflow should not contain %q", forbidden)
		}
	}
}

func TestChecksumsScriptDoesNotIncludeTemporaryOutput(t *testing.T) {
	distDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(distDir, "flow_v0.0.0_linux_amd64.tar.gz"), []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	checksumsFile := filepath.Join(distDir, "checksums.txt")
	cmd := exec.Command("bash", "scripts/release/checksums.sh")
	cmd.Env = append(os.Environ(),
		"DIST_DIR="+distDir,
		"CHECKSUMS_FILE="+checksumsFile,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("checksums script failed: %v\n%s", err, output)
	}

	contents, err := os.ReadFile(checksumsFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	if strings.Contains(text, "checksums.txt.tmp") {
		t.Fatalf("checksums included temporary output file:\n%s", text)
	}
	if !strings.Contains(text, "flow_v0.0.0_linux_amd64.tar.gz") {
		t.Fatalf("checksums missing package file:\n%s", text)
	}
}
