package terminal

import (
	"reflect"
	"strings"
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
	if info.ProxyPath != "/v2/sessions/s-1/terminal" {
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
	if info.ProxyPath != "/v2/jobs/j:reviewer%2F1/terminal" {
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

func TestTmuxClientEnvStripsTmuxAndDefaultsUTF8Locale(t *testing.T) {
	env := envSliceMap(TmuxClientEnv([]string{
		"PATH=/usr/bin",
		"TMUX=/tmp/tmux.sock",
		"TMUX_PANE=%1",
		"LANG=POSIX",
		"LC_CTYPE=en_US.UTF-8",
	}))
	if _, ok := env["TMUX"]; ok {
		t.Fatalf("TMUX was not stripped: %+v", env)
	}
	if _, ok := env["TMUX_PANE"]; ok {
		t.Fatalf("TMUX_PANE was not stripped: %+v", env)
	}
	if env["LANG"] != defaultUTF8Locale {
		t.Fatalf("LANG = %q, want %q", env["LANG"], defaultUTF8Locale)
	}
	if env["LC_ALL"] != defaultUTF8Locale {
		t.Fatalf("LC_ALL = %q, want %q", env["LC_ALL"], defaultUTF8Locale)
	}
	if env["LC_CTYPE"] != "en_US.UTF-8" {
		t.Fatalf("LC_CTYPE = %q, want explicit locale", env["LC_CTYPE"])
	}
}

func envSliceMap(env []string) map[string]string {
	values := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	return values
}

func TestWatchdogSuppressesWaitingWhenChildProcessIsBusy(t *testing.T) {
	decision := ClassifyWatchdog(WatchdogObservation{
		TmuxSession:       "flow-j-1",
		SilentFor:         10 * time.Minute,
		SilenceThreshold:  time.Minute,
		ForegroundProcess: "harness",
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
		ForegroundProcess: "harness",
	})
	if decision != WatchdogWaiting {
		t.Fatalf("decision = %q, want waiting", decision)
	}
}
