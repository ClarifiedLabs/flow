package flow_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/config"
)

// hermeticHelper gates the child branch of TestTestEnvironmentIsHermetic. It
// is on testenv's preserve list so it survives the child's own isolation.
const hermeticHelper = "FLOW_TESTENV_HERMETIC_HELPER"

func TestTestEnvironmentIsHermetic(t *testing.T) {
	if os.Getenv(hermeticHelper) == "1" {
		assertHermeticTestEnvironment(t)
		fmt.Printf("HERMETIC_HOME=%s\n", os.Getenv("HOME"))
		return
	}

	outsideHome := t.TempDir()
	outsideConfigHome := filepath.Join(outsideHome, "config")
	outsideFlowConfig := filepath.Join(outsideConfigHome, "flow", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(outsideFlowConfig), 0o700); err != nil {
		t.Fatalf("create outside config directory: %v", err)
	}
	if err := os.WriteFile(outsideFlowConfig, []byte("server_url: http://outside.invalid\ntoken: outside-token\n"), 0o600); err != nil {
		t.Fatalf("write outside Flow config: %v", err)
	}
	outsideGitConfig := filepath.Join(outsideHome, ".gitconfig")
	if err := os.WriteFile(outsideGitConfig, []byte("[user]\n\tname = Outside User\n"), 0o600); err != nil {
		t.Fatalf("write outside Git config: %v", err)
	}
	outsideGoEnv := filepath.Join(outsideHome, "go.env")
	if err := os.WriteFile(outsideGoEnv, []byte("GOFLAGS=-outside-config\n"), 0o600); err != nil {
		t.Fatalf("write outside Go config: %v", err)
	}

	// Strip the isolation marker so the child re-isolates from scratch, as
	// if the polluted environment below were its real inherited one.
	baseline := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "FLOW_TESTENV_ISOLATED=") {
			continue
		}
		baseline = append(baseline, entry)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTestEnvironmentIsHermetic$", "-test.count=1")
	cmd.Env = append(baseline,
		hermeticHelper+"=1",
		"HOME="+outsideHome,
		"XDG_CONFIG_HOME="+outsideConfigHome,
		"FLOW_DATA_DIR="+filepath.Join(outsideHome, "flow-data"),
		"FLOW_WORKER_COORDINATOR_URL=http://outside.invalid",
		"FLOW_WORKER_TOKEN=outside-worker-token",
		"FLOW_SESSION_TOKEN=outside-session-token",
		"FLOW_JOB_ID=outside-job",
		"GOENV="+outsideGoEnv,
		"GOWORK="+filepath.Join(outsideHome, "go.work"),
		"GOFLAGS=-outside-environment",
		"GIT_CONFIG_GLOBAL="+outsideGitConfig,
		"GIT_CONFIG_NOSYSTEM=0",
		"OUTSIDE_ONLY=present",
		"TMPDIR="+filepath.Join(outsideHome, "does-not-exist"),
		"TZ=America/Los_Angeles",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hermetic test helper failed: %v\n%s", err, output)
	}

	isolatedHome := ""
	for line := range strings.SplitSeq(string(output), "\n") {
		if value, ok := strings.CutPrefix(line, "HERMETIC_HOME="); ok {
			isolatedHome = value
		}
	}
	if isolatedHome == "" {
		t.Fatalf("helper output missing HERMETIC_HOME:\n%s", output)
	}
	if _, err := os.Stat(isolatedHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated HOME still exists after helper exit: %q: %v", isolatedHome, err)
	}
}

func assertHermeticTestEnvironment(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"FLOW_DATA_DIR",
		"FLOW_WORKER_COORDINATOR_URL",
		"FLOW_WORKER_TOKEN",
		"FLOW_SESSION_TOKEN",
		"FLOW_JOB_ID",
		"GOFLAGS",
		"OUTSIDE_ONLY",
	} {
		if value, ok := os.LookupEnv(key); ok {
			t.Fatalf("%s leaked into test environment as %q", key, value)
		}
	}

	home := os.Getenv("HOME")
	if !strings.HasPrefix(home, "/tmp/flow-test-") || filepath.Base(home) != "home" {
		t.Fatalf("HOME = %q, want isolated Flow test home", home)
	}
	testRoot := filepath.Dir(home)
	wantPaths := map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(testRoot, "config"),
		"XDG_DATA_HOME":   filepath.Join(testRoot, "data"),
		"XDG_CACHE_HOME":  filepath.Join(testRoot, "cache"),
		"XDG_RUNTIME_DIR": filepath.Join(testRoot, "runtime"),
		"TMPDIR":          filepath.Join(testRoot, "tmp"),
		"TMP":             filepath.Join(testRoot, "tmp"),
		"TEMP":            filepath.Join(testRoot, "tmp"),
	}
	for key, want := range wantPaths {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	wantValues := map[string]string{
		"FLOW_TESTENV_ISOLATED": "1",
		"GOENV":                 "off",
		"GOWORK":                "off",
		"GIT_CONFIG_GLOBAL":     "/dev/null",
		"GIT_CONFIG_NOSYSTEM":   "1",
		"GIT_ATTR_NOSYSTEM":     "1",
		"GIT_TERMINAL_PROMPT":   "0",
		"LANG":                  "C.UTF-8",
		"LC_ALL":                "C.UTF-8",
		"LC_CTYPE":              "C.UTF-8",
		"TZ":                    "UTC",
	}
	for key, want := range wantValues {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	umaskProbe := filepath.Join(t.TempDir(), "umask-probe")
	if err := os.WriteFile(umaskProbe, []byte("probe"), 0o666); err != nil {
		t.Fatalf("write umask probe: %v", err)
	}
	info, err := os.Stat(umaskProbe)
	if err != nil {
		t.Fatalf("stat umask probe: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("umask probe mode = %v, want 0644", info.Mode().Perm())
	}

	cfg, err := config.LoadClient("")
	if err != nil {
		t.Fatalf("load isolated client config: %v", err)
	}
	if cfg != config.DefaultClient() {
		t.Fatalf("isolated client config = %+v, want built-in defaults %+v", cfg, config.DefaultClient())
	}

	gitConfig := exec.Command("git", "config", "--global", "--list")
	output, err := gitConfig.CombinedOutput()
	if err != nil {
		t.Fatalf("read isolated global Git config: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "" {
		t.Fatalf("global Git config leaked into test environment: %s", output)
	}
}
