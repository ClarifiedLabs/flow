package flow_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/config"
)

const testEnvironmentHelper = "FLOW_TEST_ENV_HELPER"

func TestTestEnvironmentIsHermetic(t *testing.T) {
	if os.Getenv(testEnvironmentHelper) == "1" {
		assertHermeticTestEnvironment(t)
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

	cmd := exec.Command(
		testEnvironmentWrapper(t),
		"/usr/bin/env",
		testEnvironmentHelper+"=1",
		os.Args[0],
		"-test.run=^TestTestEnvironmentIsHermetic$",
		"-test.count=1",
	)
	cmd.Env = append(os.Environ(),
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
		"TMPDIR="+filepath.Join(outsideHome, "tmp"),
		"TZ=America/Los_Angeles",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hermetic test helper failed: %v\n%s", err, output)
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
	if !strings.HasPrefix(home, "/tmp/flow-test.") || filepath.Base(home) != "home" {
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
		"GOENV":               "off",
		"GOWORK":              "off",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_ATTR_NOSYSTEM":   "1",
		"GIT_TERMINAL_PROMPT": "0",
		"LANG":                "C.UTF-8",
		"LC_ALL":              "C.UTF-8",
		"LC_CTYPE":            "C.UTF-8",
		"TZ":                  "UTC",
	}
	for key, want := range wantValues {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	for _, key := range []string{"GOCACHE", "GOMODCACHE"} {
		value := filepath.Clean(os.Getenv(key))
		if !strings.Contains(value, filepath.Join("bin", ".test-cache")) {
			t.Fatalf("%s = %q, want repository test cache", key, value)
		}
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

func TestTestEnvironmentSupportsExplicitVariables(t *testing.T) {
	cmd := exec.Command(
		testEnvironmentWrapper(t),
		"/usr/bin/env",
		"FLOW_BROWSER_BIN=/explicit/browser",
		"/bin/sh",
		"-c",
		`test "$FLOW_BROWSER_BIN" = /explicit/browser && test "$(umask)" = 0022`,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("explicit test variable failed: %v\n%s", err, output)
	}
}

func TestTestEnvironmentPreservesExitStatusAndCleansUp(t *testing.T) {
	wrapper := testEnvironmentWrapper(t)
	failing := exec.Command(wrapper, "/bin/sh", "-c", "exit 23")
	err := failing.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("wrapper exit error = %v, want exit status 23", err)
	}

	printHome := exec.Command(wrapper, "/bin/sh", "-c", `printf '%s' "$HOME"`)
	output, err := printHome.Output()
	if err != nil {
		t.Fatalf("print isolated HOME: %v", err)
	}
	isolatedHome := string(output)
	if _, err := os.Stat(isolatedHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated HOME still exists after wrapper exit: %q: %v", isolatedHome, err)
	}
}

func TestCanonicalTestEntrypointsUseHermeticEnvironment(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makeText := string(makefile)
	for _, want := range []string{
		"TEST_ENV := ./scripts/test-env.sh",
		"test:\n\t$(TEST_ENV) go test -p $(GO_TEST_P) ./...",
		"js-test:\n\t$(TEST_ENV) node --test internal/web/assets/app.test.mjs",
		"lifecycle-test:\n\t$(TEST_ENV) go test ./tests/lifecycle -count=1",
		"\t$(MAKE) test",
		"web-smoke:\n\t$(TEST_ENV) /usr/bin/env FLOW_BROWSER_BIN=",
	} {
		if !strings.Contains(makeText, want) {
			t.Fatalf("Makefile does not route canonical test command through hermetic environment: missing %q", want)
		}
	}

	workflow, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	if !strings.Contains(string(workflow), "run: make test") {
		t.Fatal("release workflow does not use the hermetic Make test target")
	}
}

func testEnvironmentWrapper(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("scripts", "test-env.sh"))
	if err != nil {
		t.Fatalf("resolve test environment wrapper: %v", err)
	}
	return path
}
