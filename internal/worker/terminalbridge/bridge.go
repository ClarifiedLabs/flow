package terminalbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const defaultReadLimit int64 = 4 << 20

// ptyDrainTimeout bounds how long Run waits for the pump to forward output a
// process wrote right before exiting. The pump normally ends on its own once
// the last slave descriptor closes; a descendant keeping the slave open would
// block it, so the drain is bounded before the master is forced closed.
const ptyDrainTimeout = 500 * time.Millisecond

// Bridge joins a process's PTY to a WebSocket terminal stream.
type Bridge struct {
	master    *os.File
	cmd       *exec.Cmd
	conn      *websocket.Conn
	readLimit int64
	writeMu   sync.Mutex
}

// NewBridge creates a bridge for an already-started command and its PTY.
func NewBridge(master *os.File, cmd *exec.Cmd, conn *websocket.Conn) *Bridge {
	return &Bridge{
		master:    master,
		cmd:       cmd,
		conn:      conn,
		readLimit: defaultReadLimit,
	}
}

// SetReadLimit sets the maximum size of an incoming WebSocket message.
func (b *Bridge) SetReadLimit(limit int64) {
	b.readLimit = limit
}

type bridgeControl struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type bridgeExit struct {
	Type string `json:"type"`
	Code int    `json:"code"`
}

// Run copies terminal traffic until the process exits, the WebSocket closes,
// or ctx is canceled. Run is the sole owner of cmd.Wait. When the process
// exits, Run first forwards any output still buffered in the terminal so the
// tail of the session is not lost, then reports the exit status in-band and
// closes the connection.
func (b *Bridge) Run(ctx context.Context) error {
	if b.master == nil {
		return fmt.Errorf("run terminal bridge: nil PTY master")
	}
	if b.cmd == nil || b.cmd.Process == nil {
		return fmt.Errorf("run terminal bridge: command is not started")
	}
	if b.conn == nil {
		return fmt.Errorf("run terminal bridge: nil WebSocket connection")
	}

	b.conn.SetReadLimit(b.readLimit)

	processExited := make(chan struct{})
	processFinalized := make(chan struct{})
	pumpDone := make(chan struct{})
	go func() {
		err := b.cmd.Wait()
		close(processExited)

		// Deliver output the process wrote right before exiting — its final
		// prompt or command result — before the exit message and the close.
		// Closing the master here instead would discard whatever the pump
		// has not forwarded yet. A descendant keeping the slave open blocks
		// the pump past ptyDrainTimeout, so the close below also unsticks it.
		select {
		case <-pumpDone:
		case <-time.After(ptyDrainTimeout):
		}
		_ = b.master.Close()
		<-pumpDone

		code := b.cmd.ProcessState.ExitCode()
		payload, _ := json.Marshal(bridgeExit{Type: "exit", Code: code})
		writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = b.write(writeCtx, websocket.MessageText, payload)
		cancel()
		_ = b.close(websocket.StatusNormalClosure, "process exited")
		_ = err // The exit status is delivered in-band, including non-zero exits.
		close(processFinalized)
	}()

	go func() {
		b.copyPTYToWebSocket(ctx)
		close(pumpDone)
	}()

	for {
		messageType, data, err := b.conn.Read(ctx)
		if err != nil {
			select {
			case <-processExited:
				<-processFinalized
				return nil
			default:
			}

			killErr := terminateProcessGroup(b.cmd)
			<-processExited
			_ = b.master.Close()
			<-processFinalized
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if killErr != nil {
				return fmt.Errorf("terminal WebSocket closed and terminate process group: %w", killErr)
			}
			if websocket.CloseStatus(err) != -1 || errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read terminal WebSocket: %w", err)
		}

		switch messageType {
		case websocket.MessageBinary:
			if err := writeAll(b.master, data); err != nil {
				_ = terminateProcessGroup(b.cmd)
				<-processExited
				<-processFinalized
				return fmt.Errorf("write terminal input: %w", err)
			}
		case websocket.MessageText:
			var control bridgeControl
			if err := json.Unmarshal(data, &control); err != nil {
				continue
			}
			if control.Type == "resize" {
				if err := SetWinsize(b.master, control.Rows, control.Cols); err != nil {
					return err
				}
			}
		}
	}
}

func (b *Bridge) copyPTYToWebSocket(ctx context.Context) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := b.master.Read(buffer)
		if n > 0 {
			if writeErr := b.write(ctx, websocket.MessageBinary, buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (b *Bridge) write(ctx context.Context, messageType websocket.MessageType, data []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.conn.Write(ctx, messageType, data)
}

func (b *Bridge) close(code websocket.StatusCode, reason string) error {
	_ = code
	_ = reason
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.conn.CloseNow()
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
