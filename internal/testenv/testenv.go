// Package testenv makes Flow test processes hermetic. Flow workers launch
// coding agents with FLOW_* variables and worker configuration set; without
// isolation those leak into any test run started from such an environment,
// as do the developer's HOME, XDG directories, and global Git configuration.
//
// Every test package's TestMain must route through Main (or Isolate for
// custom TestMains); a meta-test at the repository root enforces this. With
// that in place, a plain `go test ./...` is hermetic without any wrapper.
//
// The FLOW_TESTENV_* namespace is reserved for this package.
package testenv

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// isolatedMarker marks an already-isolated process. Tests that re-exec the
// test binary (helper processes, fake agents, git hooks) inherit it, so the
// child's TestMain does not scrub the deliberate environment the parent test
// built on top of the hermetic baseline.
const isolatedMarker = "FLOW_TESTENV_ISOLATED"

// preservedKeys survive isolation with their outer values:
//
//	PATH                         - tests need git, tmux, ttyd, node, sh
//	FLOW_BROWSER_BIN             - explicit browser for browser smoke tests
//	GOCOVERDIR                   - coverage-instrumented runs write here
//	FLOW_TESTENV_HERMETIC_HELPER - gate for the hermeticity regression test
var preservedKeys = []string{"PATH", "FLOW_BROWSER_BIN", "GOCOVERDIR", "FLOW_TESTENV_HERMETIC_HELPER"}

// Main is the canonical TestMain body:
//
//	func TestMain(m *testing.M) { testenv.Main(m) }
func Main(m *testing.M) {
	cleanup := Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// Isolate replaces the process environment with a hermetic allowlist rooted
// at a fresh temporary directory and returns a cleanup func that removes it.
// It must run before any test executes (i.e. from TestMain). It is a no-op
// in processes that already run inside an isolated environment.
func Isolate() func() {
	if os.Getenv(isolatedMarker) == "1" {
		return func() {}
	}
	saved := map[string]string{}
	for _, key := range preservedKeys {
		if value, ok := os.LookupEnv(key); ok {
			saved[key] = value
		}
	}
	if saved["PATH"] == "" {
		saved["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	os.Clearenv()
	// Test-created files should have stable conventional permissions
	// regardless of the developer's umask.
	syscall.Umask(0o022)

	// TMPDIR is now unset, so this creates /tmp/flow-test-* on the
	// platforms Flow targets.
	root, err := os.MkdirTemp("", "flow-test-")
	if err != nil {
		fatalf("testenv: create isolation root: %v", err)
	}
	for _, sub := range []string{"home", "config", "data", "cache", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, sub), 0o755); err != nil {
			fatalf("testenv: create %s: %v", sub, err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0o700); err != nil {
		fatalf("testenv: create runtime: %v", err)
	}

	env := map[string]string{
		"HOME":                filepath.Join(root, "home"),
		"XDG_CONFIG_HOME":     filepath.Join(root, "config"),
		"XDG_DATA_HOME":       filepath.Join(root, "data"),
		"XDG_CACHE_HOME":      filepath.Join(root, "cache"),
		"XDG_RUNTIME_DIR":     filepath.Join(root, "runtime"),
		"TMPDIR":              filepath.Join(root, "tmp"),
		"TMP":                 filepath.Join(root, "tmp"),
		"TEMP":                filepath.Join(root, "tmp"),
		"GOENV":               "off",
		"GOWORK":              "off",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_ATTR_NOSYSTEM":   "1",
		"GIT_TERMINAL_PROMPT": "0",
		"LANG":                "C.UTF-8",
		"LC_ALL":              "C.UTF-8",
		"LC_CTYPE":            "C.UTF-8",
		// Effective in-process: time.Local lazy-loads on first use,
		// which is after TestMain has started.
		"TZ":           "UTC",
		isolatedMarker: "1",
	}
	maps.Copy(env, saved)
	for key, value := range env {
		if err := os.Setenv(key, value); err != nil {
			fatalf("testenv: set %s: %v", key, err)
		}
	}
	return func() { _ = os.RemoveAll(root) }
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
