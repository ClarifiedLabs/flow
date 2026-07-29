package terminal

import (
	"net/url"
	"strings"
	"time"
)

type AttachInfo struct {
	SessionID      string   `json:"session_id"`
	JobID          string   `json:"job_id"`
	TmuxSession    string   `json:"tmux_session"`
	TmuxSocketPath string   `json:"tmux_socket_path,omitempty"`
	Command        []string `json:"command"`
	ProxyPath      string   `json:"proxy_path"`
}

func AttachInfoForSession(sessionID string, jobID string, tmuxSocketPath string) AttachInfo {
	tmuxSession := TmuxSessionNameForJob(jobID)
	return AttachInfo{
		SessionID:      strings.TrimSpace(sessionID),
		JobID:          strings.TrimSpace(jobID),
		TmuxSession:    tmuxSession,
		TmuxSocketPath: strings.TrimSpace(tmuxSocketPath),
		Command:        TmuxAttachCommand(tmuxSession, tmuxSocketPath),
		ProxyPath:      TerminalProxyPath(sessionID),
	}
}

func AttachInfoForJob(jobID string, tmuxSocketPath string) AttachInfo {
	tmuxSession := TmuxSessionNameForJob(jobID)
	trimmedJobID := strings.TrimSpace(jobID)
	return AttachInfo{
		JobID:          trimmedJobID,
		TmuxSession:    tmuxSession,
		TmuxSocketPath: strings.TrimSpace(tmuxSocketPath),
		Command:        TmuxAttachCommand(tmuxSession, tmuxSocketPath),
		ProxyPath:      JobTerminalProxyPath(trimmedJobID),
	}
}

func TmuxAttachCommand(sessionName string, tmuxSocketPath string) []string {
	command := []string{"tmux"}
	if socketPath := strings.TrimSpace(tmuxSocketPath); socketPath != "" {
		command = append(command, "-S", socketPath)
	}
	return append(command, "attach-session", "-t", strings.TrimSpace(sessionName))
}

func TerminalProxyPath(sessionID string) string {
	return "/v2/sessions/" + url.PathEscape(strings.TrimSpace(sessionID)) + "/terminal"
}

func JobTerminalProxyPath(jobID string) string {
	return "/v2/jobs/" + url.PathEscape(strings.TrimSpace(jobID)) + "/terminal"
}

// TmuxClientEnv returns a copy of env with TMUX/TMUX_PANE stripped and a
// default UTF-8 locale applied. It is suitable for tmux client commands that
// must not inherit the caller's tmux session state.
func TmuxClientEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, value := range env {
		if strings.HasPrefix(value, "TMUX=") || strings.HasPrefix(value, "TMUX_PANE=") {
			continue
		}
		filtered = append(filtered, value)
	}
	return withDefaultUTF8Locale(filtered)
}

func withDefaultUTF8Locale(env []string) []string {
	result := append([]string(nil), env...)
	present := map[string]bool{}
	for i, value := range result {
		key, rawValue, ok := strings.Cut(value, "=")
		if !ok || !isUTF8LocaleKey(key) {
			continue
		}
		present[key] = true
		if !isUTF8Locale(rawValue) {
			result[i] = key + "=" + defaultUTF8Locale
		}
	}
	for _, key := range utf8LocaleEnvKeys {
		if !present[key] {
			result = append(result, key+"="+defaultUTF8Locale)
		}
	}
	return result
}

func isUTF8LocaleKey(key string) bool {
	for _, candidate := range utf8LocaleEnvKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

func isUTF8Locale(value string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
	return strings.Contains(normalized, "UTF8")
}

const defaultUTF8Locale = "C.UTF-8"

var utf8LocaleEnvKeys = []string{"LANG", "LC_ALL", "LC_CTYPE"}

func TmuxSessionNameForJob(jobID string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.TrimSpace(jobID))
	if safe == "" {
		safe = "job"
	}

	return "flow-" + safe
}

type WatchdogObservation struct {
	TmuxSession       string
	SilentFor         time.Duration
	SilenceThreshold  time.Duration
	ForegroundProcess string
	BusyChildProcess  bool
}

type WatchdogDecision string

const (
	WatchdogNoChange WatchdogDecision = "no_change"
	WatchdogWorking  WatchdogDecision = "working"
	WatchdogWaiting  WatchdogDecision = "waiting"
)

func ClassifyWatchdog(observation WatchdogObservation) WatchdogDecision {
	if observation.BusyChildProcess {
		return WatchdogWorking
	}
	if observation.SilenceThreshold <= 0 || observation.SilentFor < observation.SilenceThreshold {
		return WatchdogNoChange
	}
	if strings.TrimSpace(observation.ForegroundProcess) == "" {
		return WatchdogNoChange
	}

	return WatchdogWaiting
}
