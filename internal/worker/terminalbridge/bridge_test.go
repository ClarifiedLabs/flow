package terminalbridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBridgeWebSocketPTYEchoRoundTrip(t *testing.T) {
	workerConn, peerConn := websocketPair(t)
	cmd := exec.Command("cat")
	master, err := StartWithSize(cmd, 24, 80)
	if err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- NewBridge(master, cmd, workerConn).Run(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := peerConn.Write(ctx, websocket.MessageBinary, []byte("bridge-echo\n")); err != nil {
		t.Fatalf("write WebSocket input: %v", err)
	}
	readWebSocketUntil(t, ctx, peerConn, websocket.MessageBinary, []byte("bridge-echo"))

	if err := peerConn.Close(websocket.StatusNormalClosure, "test complete"); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	waitBridge(t, runErr)
}

func TestBridgeResizeChangesTerminalSize(t *testing.T) {
	workerConn, peerConn := websocketPair(t)
	cmd := exec.Command("sh")
	master, err := StartWithSize(cmd, 24, 80)
	if err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- NewBridge(master, cmd, workerConn).Run(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := peerConn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":91,"rows":37}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := peerConn.Write(ctx, websocket.MessageBinary, []byte("stty size; exit\n")); err != nil {
		t.Fatalf("write stty command: %v", err)
	}
	readWebSocketUntil(t, ctx, peerConn, websocket.MessageBinary, []byte("37 91"))

	for {
		messageType, data, err := peerConn.Read(ctx)
		if err != nil {
			t.Fatalf("read exit message: %v", err)
		}
		if messageType == websocket.MessageText {
			if string(data) != `{"type":"exit","code":0}` {
				t.Fatalf("exit message = %s", data)
			}
			break
		}
	}
	waitBridge(t, runErr)
}

// TestBridgeDeliversFinalOutputBeforeExit is a regression test for a lost
// session tail: the bridge used to close the PTY master and the WebSocket as
// soon as the process exited, racing the pump and dropping output the process
// had just written (TestBridgeResizeChangesTerminalSize flaked on this in CI).
// The hog processes starve the scheduler the way parallel CI compilation
// does, so without the drain the exit message and the close reliably arrive
// before the payload does. The hot burst covers output still streaming at
// exit; the cold burst covers a prompt that prints and immediately exits.
func TestBridgeDeliversFinalOutputBeforeExit(t *testing.T) {
	// Register cleanup right after each Start so that a failure spawning a
	// later hog (t.Fatalf skips the rest of the test body) cannot leak the
	// hogs that are already running.
	for range 2 * runtime.NumCPU() {
		hog := exec.Command("sh", "-c", "while :; do :; done")
		if err := hog.Start(); err != nil {
			t.Fatalf("start CPU hog: %v", err)
		}
		t.Cleanup(func() {
			_ = hog.Process.Kill()
			_, _ = hog.Process.Wait()
		})
	}

	payload := bytes.Repeat([]byte("x"), 32<<10)
	commands := []string{
		`head -c 32768 /dev/zero | tr '\000' x; exit 0`,
		`sleep 0.2; head -c 32768 /dev/zero | tr '\000' x; exit 0`,
	}
	for i := 0; i < 26; i++ {
		workerConn, peerConn := websocketPair(t)
		cmd := exec.Command("sh", "-c", commands[i%len(commands)])
		master, err := StartWithSize(cmd, 24, 80)
		if err != nil {
			t.Fatalf("StartWithSize: %v", err)
		}

		runErr := make(chan error, 1)
		go func() { runErr <- NewBridge(master, cmd, workerConn).Run(context.Background()) }()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var output bytes.Buffer
		for {
			messageType, data, err := peerConn.Read(ctx)
			if err != nil {
				cancel()
				t.Fatalf("iteration %d: read WebSocket before exit message: %v (received %d of %d payload bytes)", i, err, output.Len(), len(payload))
			}
			if messageType == websocket.MessageBinary {
				output.Write(data)
				continue
			}
			if string(data) != `{"type":"exit","code":0}` {
				cancel()
				t.Fatalf("iteration %d: exit message = %s", i, data)
			}
			break
		}
		cancel()

		if !bytes.Contains(output.Bytes(), payload) {
			t.Fatalf("iteration %d: received %d of %d payload bytes before the exit message", i, output.Len(), len(payload))
		}
		waitBridge(t, runErr)
	}
}

func TestBridgeChildExitSendsExitAndCloses(t *testing.T) {
	workerConn, peerConn := websocketPair(t)
	cmd := exec.Command("sh", "-c", "exit 7")
	master, err := StartWithSize(cmd, 24, 80)
	if err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- NewBridge(master, cmd, workerConn).Run(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	messageType, data, err := peerConn.Read(ctx)
	if err != nil {
		t.Fatalf("read exit message: %v", err)
	}
	if messageType != websocket.MessageText || string(data) != `{"type":"exit","code":7}` {
		t.Fatalf("exit message = type %v %s", messageType, data)
	}
	if _, _, err := peerConn.Read(ctx); err == nil {
		t.Fatal("expected WebSocket to close after process exit")
	}
	waitBridge(t, runErr)
}

func TestBridgeWebSocketCloseKillsProcessGroup(t *testing.T) {
	workerConn, peerConn := websocketPair(t)
	cmd := exec.Command("sh", "-c", "sleep 30 & child=$!; echo child:$child; wait $child")
	master, err := StartWithSize(cmd, 24, 80)
	if err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- NewBridge(master, cmd, workerConn).Run(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output := readWebSocketUntil(t, ctx, peerConn, websocket.MessageBinary, []byte("child:"))
	match := regexp.MustCompile(`child:(\d+)`).FindSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("child PID not found in %q", output)
	}
	childPID, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("parse child PID: %v", err)
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child is not running before close: %v", err)
	}

	if err := peerConn.Close(websocket.StatusNormalClosure, "test close"); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	waitBridge(t, runErr)
	if cmd.ProcessState == nil {
		t.Fatal("command was not waited")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d remains after process-group termination (kill error %v)", childPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func websocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	acceptErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	workerConn, _, err := websocket.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial test WebSocket: %v", err)
	}

	var peerConn *websocket.Conn
	select {
	case peerConn = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept test WebSocket: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out accepting test WebSocket")
	}
	t.Cleanup(func() {
		workerConn.CloseNow()
		peerConn.CloseNow()
	})
	return workerConn, peerConn
}

func readWebSocketUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, wantedType websocket.MessageType, wanted []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read WebSocket waiting for %q: %v (output %q)", wanted, err, output.Bytes())
		}
		if messageType == wantedType {
			output.Write(data)
			if bytes.Contains(output.Bytes(), wanted) {
				return output.Bytes()
			}
		}
	}
}

func waitBridge(t *testing.T, runErr <-chan error) {
	t.Helper()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Bridge.Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal(fmt.Sprintf("timed out waiting for bridge"))
	}
}
