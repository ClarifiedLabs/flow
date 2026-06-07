package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

const EnvVar = "LOG_LEVEL"

// Configure installs a process-wide slog logger and returns args with any
// --log-level flag removed. The returned restore function keeps tests and
// embedded invocations isolated.
func Configure(args []string, stderr io.Writer, getenv func(string) string) ([]string, func(), error) {
	if stderr == nil {
		stderr = io.Discard
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	levelText := strings.TrimSpace(getenv(EnvVar))
	stripped, flagLevel, ok, err := StripLogLevelFlag(args)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		levelText = flagLevel
	}

	level, err := ParseLevel(levelText)
	if err != nil {
		return nil, nil, err
	}

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{
		AddSource: level <= slog.LevelDebug,
		Level:     level,
	})))
	return stripped, func() {
		slog.SetDefault(previous)
	}, nil
}

func StripLogLevelFlag(args []string) ([]string, string, bool, error) {
	stripped := make([]string, 0, len(args))
	var level string
	var found bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			stripped = append(stripped, args[i:]...)
			break
		}
		if arg == "--log-level" || arg == "-log-level" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return nil, "", false, fmt.Errorf("--log-level requires a value")
			}
			level = strings.TrimSpace(args[i+1])
			found = true
			i++
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--log-level="); ok {
			if strings.TrimSpace(value) == "" {
				return nil, "", false, fmt.Errorf("--log-level requires a value")
			}
			level = strings.TrimSpace(value)
			found = true
			continue
		}
		if value, ok := strings.CutPrefix(arg, "-log-level="); ok {
			if strings.TrimSpace(value) == "" {
				return nil, "", false, fmt.Errorf("--log-level requires a value")
			}
			level = strings.TrimSpace(value)
			found = true
			continue
		}
		stripped = append(stripped, arg)
	}

	return stripped, level, found, nil
}

func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	case "off", "none", "disabled":
		return slog.Level(100), nil
	default:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("invalid log level %q: expected debug, info, warn, error, or off", value)
		}
		return slog.Level(parsed), nil
	}
}

func CommandName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
