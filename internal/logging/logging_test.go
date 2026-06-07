package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestStripLogLevelFlag(t *testing.T) {
	args, level, ok, err := StripLogLevelFlag([]string{"issue", "list", "--log-level", "debug", "--server", "http://example.test"})
	if err != nil {
		t.Fatalf("StripLogLevelFlag returned error: %v", err)
	}
	if !ok || level != "debug" {
		t.Fatalf("level = %q ok = %v, want debug true", level, ok)
	}
	if got := strings.Join(args, " "); got != "issue list --server http://example.test" {
		t.Fatalf("args = %q", got)
	}

	args, level, ok, err = StripLogLevelFlag([]string{"--log-level=warn", "doctor"})
	if err != nil {
		t.Fatalf("StripLogLevelFlag returned error: %v", err)
	}
	if !ok || level != "warn" {
		t.Fatalf("level = %q ok = %v, want warn true", level, ok)
	}
	if got := strings.Join(args, " "); got != "doctor" {
		t.Fatalf("args = %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	level, err := ParseLevel("debug")
	if err != nil {
		t.Fatalf("ParseLevel returned error: %v", err)
	}
	if got := level.Level(); got != slog.LevelDebug {
		t.Fatalf("level = %v, want debug", got)
	}

	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("ParseLevel accepted invalid level")
	}
}

func TestConfigureUsesFlagOverEnvironment(t *testing.T) {
	var stderr bytes.Buffer
	args, restore, err := Configure([]string{"doctor", "--log-level", "debug"}, &stderr, func(key string) string {
		if key == EnvVar {
			return "error"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Configure returned error: %v", err)
	}
	defer restore()
	if got := strings.Join(args, " "); got != "doctor" {
		t.Fatalf("args = %q", got)
	}

	slog.Debug("debug message")
	if !strings.Contains(stderr.String(), "debug message") {
		t.Fatalf("stderr missing debug message: %q", stderr.String())
	}
}
