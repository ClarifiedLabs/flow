package terminal

import (
	"reflect"
	"testing"
	"time"
)

func TestAttachInfoForSessionUsesStableTmuxName(t *testing.T) {
	info := AttachInfoForSession("s-1", "j:author/1", "")
	if info.TmuxSession != "flow-j-author-1" {
		t.Fatalf("TmuxSession = %q, want sanitized name", info.TmuxSession)
	}
	wantCommand := []string{"tmux", "attach-session", "-t", "flow-j-author-1"}
	if !reflect.DeepEqual(info.Command, wantCommand) {
		t.Fatalf("Command = %#v, want %#v", info.Command, wantCommand)
	}
	if info.ProxyPath != "/v1/sessions/s-1/terminal" {
		t.Fatalf("ProxyPath = %q, want session terminal path", info.ProxyPath)
	}
}

func TestAttachInfoForJobUsesStableTmuxName(t *testing.T) {
	info := AttachInfoForJob("j:reviewer/1", "")
	if info.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty for job attach", info.SessionID)
	}
	if info.JobID != "j:reviewer/1" || info.TmuxSession != "flow-j-reviewer-1" {
		t.Fatalf("attach info = %+v", info)
	}
	wantCommand := []string{"tmux", "attach-session", "-t", "flow-j-reviewer-1"}
	if !reflect.DeepEqual(info.Command, wantCommand) {
		t.Fatalf("Command = %#v, want %#v", info.Command, wantCommand)
	}
	if info.ProxyPath != "/v1/jobs/j:reviewer%2F1/terminal" {
		t.Fatalf("ProxyPath = %q, want job terminal path", info.ProxyPath)
	}
}

func TestAttachInfoIncludesConfiguredTmuxSocket(t *testing.T) {
	info := AttachInfoForJob("j:reviewer/1", "/tmp/flow-job.sock")
	wantCommand := []string{"tmux", "-S", "/tmp/flow-job.sock", "attach-session", "-t", "flow-j-reviewer-1"}
	if !reflect.DeepEqual(info.Command, wantCommand) {
		t.Fatalf("Command = %#v, want %#v", info.Command, wantCommand)
	}
	if info.TmuxSocketPath != "/tmp/flow-job.sock" {
		t.Fatalf("TmuxSocketPath = %q", info.TmuxSocketPath)
	}
}

func TestTTYDServeCommandBindsToLoopback(t *testing.T) {
	command := TTYDServeCommand("flow-j-author-1", "127.0.0.1", 9123)
	want := []string{"ttyd", "-W", "-i", "127.0.0.1", "-p", "9123", "tmux", "attach-session", "-t", "flow-j-author-1"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %#v, want %#v", command, want)
	}
}

func TestNormalizeProxyTargetURLRequiresLoopbackHTTP(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1:7681", "https://localhost/terminal", "http://10.0.0.4:7681", "http://100.64.1.2:7681"} {
		normalized, err := NormalizeProxyTargetURL(target)
		if err != nil {
			t.Fatalf("NormalizeProxyTargetURL(%q): %v", target, err)
		}
		if normalized != target {
			t.Fatalf("normalized target = %q, want %q", normalized, target)
		}
	}
	for _, target := range []string{"http://example.com", "http://8.8.8.8:7681", "file:///tmp/socket", "http://user@127.0.0.1:7681"} {
		if _, err := NormalizeProxyTargetURL(target); err == nil {
			t.Fatalf("NormalizeProxyTargetURL(%q) succeeded, want error", target)
		}
	}
}

func TestWatchdogSuppressesWaitingWhenChildProcessIsBusy(t *testing.T) {
	decision := ClassifyWatchdog(WatchdogObservation{
		TmuxSession:       "flow-j-1",
		SilentFor:         10 * time.Minute,
		SilenceThreshold:  time.Minute,
		ForegroundProcess: "codex",
		BusyChildProcess:  true,
	})
	if decision != WatchdogWorking {
		t.Fatalf("decision = %q, want working for busy child process", decision)
	}
}

func TestWatchdogClassifiesSilentIdleForegroundProcessAsWaiting(t *testing.T) {
	decision := ClassifyWatchdog(WatchdogObservation{
		TmuxSession:       "flow-j-1",
		SilentFor:         10 * time.Minute,
		SilenceThreshold:  time.Minute,
		ForegroundProcess: "codex",
	})
	if decision != WatchdogWaiting {
		t.Fatalf("decision = %q, want waiting", decision)
	}
}
