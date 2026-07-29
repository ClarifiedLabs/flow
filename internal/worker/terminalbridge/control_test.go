package terminalbridge

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type memoryJobRegistry struct {
	mu   sync.Mutex
	jobs map[string]bool
}

func newMemoryJobRegistry() *memoryJobRegistry {
	return &memoryJobRegistry{jobs: make(map[string]bool)}
}

func (r *memoryJobRegistry) Register(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[jobID] = true
}

func (r *memoryJobRegistry) Unregister(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, jobID)
}

func (r *memoryJobRegistry) Has(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[jobID]
}

func TestControlClientUnknownJobReturnsTerminalError(t *testing.T) {
	controlConnections := make(chan *websocket.Conn, 2)
	server := newControlServer(t, controlConnections, nil)
	client := NewControlClient(server.URL, "worker-1", "secret", newMemoryJobRegistry())
	client.reconnectMin = 10 * time.Millisecond
	client.reconnectMax = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()
	control := receiveConnection(t, controlConnections)

	writeJSON(t, control, `{"type":"terminal-open","stream_id":"stream-1","job_id":"missing","cols":80,"rows":24}`)
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	messageType, data, err := control.Read(readCtx)
	if err != nil {
		t.Fatalf("read terminal error: %v", err)
	}
	if messageType != websocket.MessageText || string(data) != `{"type":"terminal-error","stream_id":"stream-1","error":"job not active"}` {
		t.Fatalf("terminal error = type %v %s", messageType, data)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cancel()
	waitControlClient(t, runErr)
}

func TestControlClientRegisteredJobStreamsTerminal(t *testing.T) {
	tmuxArgsPath := installFakeTmux(t)
	controlConnections := make(chan *websocket.Conn, 2)
	streamConnections := make(chan *websocket.Conn, 2)
	server := newControlServer(t, controlConnections, streamConnections)
	registry := newMemoryJobRegistry()
	registry.Register("job-1")
	client := NewControlClient(server.URL, "worker-1", "secret", registry)
	jobSocketPath := filepath.Join(t.TempDir(), "job.sock")

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()
	control := receiveConnection(t, controlConnections)

	writeJSON(t, control, fmt.Sprintf(
		`{"type":"terminal-open","stream_id":"stream-echo","job_id":"job-1","tmux_socket_path":%q,"cols":90,"rows":30}`,
		jobSocketPath,
	))
	stream := receiveConnection(t, streamConnections)
	ioCtx, ioCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ioCancel()
	if err := stream.Write(ioCtx, websocket.MessageBinary, []byte("control-echo\n")); err != nil {
		t.Fatalf("write terminal stream: %v", err)
	}
	readWebSocketUntil(t, ioCtx, stream, websocket.MessageBinary, []byte("control-echo"))
	wantArgs := strings.Join([]string{"-S", jobSocketPath, "attach-session", "-t", "flow-job-1", ""}, "\n")
	if got := readFileEventually(t, tmuxArgsPath); got != wantArgs {
		t.Fatalf("tmux args = %q, want %q", got, wantArgs)
	}

	if err := stream.Close(websocket.StatusNormalClosure, "test complete"); err != nil {
		t.Fatalf("close terminal stream: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cancel()
	waitControlClient(t, runErr)
}

func TestControlClientReconnectReplacesConnection(t *testing.T) {
	controlConnections := make(chan *websocket.Conn, 3)
	server := newControlServer(t, controlConnections, nil)
	client := NewControlClient(server.URL, "worker-1", "secret", newMemoryJobRegistry())
	client.reconnectMin = 10 * time.Millisecond
	client.reconnectMax = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()
	firstServerConnection := receiveConnection(t, controlConnections)
	firstClientConnection := currentControlConnection(t, client)

	if err := firstServerConnection.Close(websocket.StatusInternalError, "force reconnect"); err != nil {
		t.Fatalf("close first connection: %v", err)
	}
	secondServerConnection := receiveConnection(t, controlConnections)
	secondClientConnection := waitForReplacement(t, client, firstClientConnection)
	if secondClientConnection == firstClientConnection {
		t.Fatal("control connection was not replaced")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	secondServerConnection.CloseNow()
	cancel()
	waitControlClient(t, runErr)
}

func newControlServer(t *testing.T, controls chan<- *websocket.Conn, streams chan<- *websocket.Conn) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/workers/worker-1/control":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept control WebSocket: %v", err)
				return
			}
			controls <- conn
		case "/v2/workers/worker-1/terminal-stream":
			if streams == nil {
				http.Error(w, "unexpected terminal stream", http.StatusNotFound)
				return
			}
			if got := r.URL.Query().Get("stream_id"); got != "stream-echo" {
				t.Errorf("stream_id = %q, want stream-echo", got)
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept terminal WebSocket: %v", err)
				return
			}
			streams <- conn
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func installFakeTmux(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "tmux")
	argsPath := filepath.Join(directory, "tmux-args")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FLOW_TEST_TMUX_ARGS\"\nexec cat\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FLOW_TEST_TMUX_ARGS", argsPath)
	return argsPath
}

func readFileEventually(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			return string(contents)
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func receiveConnection(t *testing.T, connections <-chan *websocket.Conn) *websocket.Conn {
	t.Helper()
	select {
	case conn := <-connections:
		t.Cleanup(func() { _ = conn.CloseNow() })
		return conn
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for WebSocket connection")
		return nil
	}
}

func currentControlConnection(t *testing.T, client *ControlClient) *websocket.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client.connMu.Lock()
		conn := client.conn
		client.connMu.Unlock()
		if conn != nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatal("control client did not install connection")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForReplacement(t *testing.T, client *ControlClient, previous *websocket.Conn) *websocket.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client.connMu.Lock()
		conn := client.conn
		client.connMu.Unlock()
		if conn != nil && conn != previous {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatal("control client did not replace connection")
		}
		time.Sleep(time.Millisecond)
	}
}

func writeJSON(t *testing.T, conn *websocket.Conn, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
		t.Fatalf("write JSON: %v", err)
	}
}

func waitControlClient(t *testing.T, runErr <-chan error) {
	t.Helper()
	select {
	case err := <-runErr:
		if err != nil && err != context.Canceled {
			t.Fatalf("ControlClient.Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal(fmt.Sprintf("timed out waiting for control client"))
	}
}

var _ = bytes.Contains
