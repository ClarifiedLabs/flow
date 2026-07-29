package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSessionTerminalStreamUsesRegisteredSocketAndPropagatesWorkerError(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Terminal stream socket task")
	const socketPath = "/tmp/flow-session-test.sock"
	if _, err := fixture.Sessions.RegisterTerminal(context.Background(), started.Session.ID, socketPath); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}
	access, err := fixture.Sessions.CreateTerminalAccess(context.Background(), started.Session.ID, defaultTerminalAccessTTL)
	if err != nil {
		t.Fatalf("create terminal access: %v", err)
	}

	httpServer := httptest.NewServer(fixture.Server)
	t.Cleanup(httpServer.Close)
	wsBaseURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	controlHeader := make(http.Header)
	controlHeader.Set("Authorization", "Bearer worker-token")
	controlCtx, cancelControl := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelControl()
	control, _, err := websocket.Dial(controlCtx, wsBaseURL+"/v2/workers/w-local/control", &websocket.DialOptions{
		HTTPHeader: controlHeader,
	})
	if err != nil {
		t.Fatalf("dial worker control WebSocket: %v", err)
	}
	t.Cleanup(func() { _ = control.CloseNow() })
	waitForWorkerTerminalControl(t, fixture.Server, "w-local")

	loginClient := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	loginResponse, err := loginClient.Get(httpServer.URL + access.LoginPath)
	if err != nil {
		t.Fatalf("request terminal login: %v", err)
	}
	defer loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("terminal login status = %d, want 303", loginResponse.StatusCode)
	}
	var terminalCookie *http.Cookie
	for _, cookie := range loginResponse.Cookies() {
		if cookie.Name == terminalAccessCookie {
			terminalCookie = cookie
			break
		}
	}
	if terminalCookie == nil {
		t.Fatalf("terminal login cookies = %+v, missing %s", loginResponse.Cookies(), terminalAccessCookie)
	}

	browserHeader := make(http.Header)
	browserHeader.Set("Cookie", terminalCookie.String())
	browserCtx, cancelBrowser := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelBrowser()
	browser, _, err := websocket.Dial(
		browserCtx,
		wsBaseURL+"/v2/sessions/"+started.Session.ID+"/terminal/stream",
		&websocket.DialOptions{HTTPHeader: browserHeader},
	)
	if err != nil {
		t.Fatalf("dial browser terminal WebSocket: %v", err)
	}
	t.Cleanup(func() { _ = browser.CloseNow() })
	if err := browser.Write(browserCtx, websocket.MessageText, []byte(`{"type":"attach","cols":100,"rows":40}`)); err != nil {
		t.Fatalf("write browser terminal attach: %v", err)
	}

	messageType, data, err := control.Read(controlCtx)
	if err != nil {
		t.Fatalf("read terminal-open message: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("terminal-open message type = %v, want text", messageType)
	}
	var opened struct {
		Type           string `json:"type"`
		StreamID       string `json:"stream_id"`
		JobID          string `json:"job_id"`
		TmuxSocketPath string `json:"tmux_socket_path"`
		Cols           uint16 `json:"cols"`
		Rows           uint16 `json:"rows"`
	}
	if err := json.Unmarshal(data, &opened); err != nil {
		t.Fatalf("decode terminal-open message: %v", err)
	}
	if opened.Type != "terminal-open" || opened.StreamID == "" || opened.JobID != started.Session.JobID {
		t.Fatalf("terminal-open message = %+v", opened)
	}
	if opened.TmuxSocketPath != socketPath {
		t.Fatalf("terminal-open tmux socket = %q, want %q", opened.TmuxSocketPath, socketPath)
	}
	if opened.Cols != 100 || opened.Rows != 40 {
		t.Fatalf("terminal-open size = %dx%d, want 100x40", opened.Cols, opened.Rows)
	}

	terminalError, err := json.Marshal(workerTerminalErrorMessage{
		Type:     "terminal-error",
		StreamID: opened.StreamID,
		Error:    "job not active",
	})
	if err != nil {
		t.Fatalf("marshal terminal error: %v", err)
	}
	if err := control.Write(controlCtx, websocket.MessageText, terminalError); err != nil {
		t.Fatalf("write terminal error: %v", err)
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	defer cancelClose()
	if _, _, err := browser.Read(closeCtx); err == nil {
		t.Fatal("browser terminal remained open after worker terminal error")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("browser terminal waited for the stream timeout after worker terminal error")
	} else if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Fatalf("browser terminal close status = %v, want %v; error: %v", status, websocket.StatusPolicyViolation, err)
	}
}

func waitForWorkerTerminalControl(t *testing.T, server *Server, workerID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := server.workerTerminals.ControlConn(workerID); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s control WebSocket was not registered", workerID)
		}
		time.Sleep(time.Millisecond)
	}
}
