package terminal

import (
	"net"
	"net/url"
	"strconv"
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
	return "/v1/sessions/" + url.PathEscape(strings.TrimSpace(sessionID)) + "/terminal"
}

func JobTerminalProxyPath(jobID string) string {
	return "/v1/jobs/" + url.PathEscape(strings.TrimSpace(jobID)) + "/terminal"
}

func TTYDServeCommand(sessionName string, bindAddress string, port int) []string {
	return []string{
		"ttyd",
		"-W",
		"-i", strings.TrimSpace(bindAddress),
		"-p", strconv.Itoa(port),
		"tmux", "attach-session", "-t", strings.TrimSpace(sessionName),
	}
}

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

func NormalizeProxyTargetURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errInvalidTargetURL
	}
	if parsed.User != nil || parsed.Fragment != "" || strings.TrimSpace(parsed.Host) == "" {
		return "", errInvalidTargetURL
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return parsed.String(), nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !allowedTerminalTargetIP(ip) {
		return "", errInvalidTargetURL
	}

	return parsed.String(), nil
}

func allowedTerminalTargetIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	ip = ip.To4()
	if ip == nil {
		return false
	}

	return ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
}

type invalidTargetURLError struct{}

func (invalidTargetURLError) Error() string {
	return "terminal target URL must be an HTTP loopback, private, or tailnet URL"
}

var errInvalidTargetURL invalidTargetURLError
