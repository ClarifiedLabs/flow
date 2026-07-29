package terminalbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ClarifiedLabs/flow/internal/terminal"
	"github.com/coder/websocket"
)

const (
	controlReconnectMin = 250 * time.Millisecond
	controlReconnectMax = 30 * time.Second
)

// JobRegistry reports which jobs are active on this worker.
type JobRegistry interface {
	Register(jobID string)
	Unregister(jobID string)
	Has(jobID string) bool
}

// ControlClient maintains the coordinator's worker control WebSocket and opens
// terminal streams requested over it.
type ControlClient struct {
	coordinatorURL string
	workerID       string
	token          string
	registry       JobRegistry
	logger         *slog.Logger

	connMu  sync.Mutex
	conn    *websocket.Conn
	closed  bool
	writeMu sync.Mutex

	reconnectMin time.Duration
	reconnectMax time.Duration
}

// NewControlClient creates a worker control client.
func NewControlClient(coordinatorURL, workerID, token string, registry JobRegistry) *ControlClient {
	return &ControlClient{
		coordinatorURL: strings.TrimSpace(coordinatorURL),
		workerID:       strings.TrimSpace(workerID),
		token:          strings.TrimSpace(token),
		registry:       registry,
		logger:         slog.Default(),
		reconnectMin:   controlReconnectMin,
		reconnectMax:   controlReconnectMax,
	}
}

type controlMessage struct {
	Type           string `json:"type"`
	StreamID       string `json:"stream_id"`
	JobID          string `json:"job_id"`
	TmuxSocketPath string `json:"tmux_socket_path"`
	Cols           uint16 `json:"cols"`
	Rows           uint16 `json:"rows"`
}

type terminalError struct {
	Type     string `json:"type"`
	StreamID string `json:"stream_id"`
	Error    string `json:"error"`
}

// Run connects to the coordinator and reconnects after unexpected disconnects
// until ctx is canceled or Close is called.
func (c *ControlClient) Run(ctx context.Context) error {
	controlURL, err := c.websocketURL("control", "")
	if err != nil {
		return fmt.Errorf("build worker control URL: %w", err)
	}

	backoff := c.reconnectMin
	for {
		if c.isClosed() {
			return nil
		}

		conn, _, err := websocket.Dial(ctx, controlURL, c.dialOptions())
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if c.isClosed() {
				return nil
			}
			c.logger.WarnContext(ctx, "worker control WebSocket dial failed", "url", controlURL, "error", err, "retry_in", backoff)
			if err := c.waitForReconnect(ctx, backoff); err != nil {
				return err
			}
			backoff = nextBackoff(backoff, c.reconnectMax)
			continue
		}

		if !c.installConnection(conn) {
			_ = conn.Close(websocket.StatusNormalClosure, "control client closed")
			return nil
		}
		backoff = c.reconnectMin
		c.logger.InfoContext(ctx, "worker control WebSocket connected", "url", controlURL)

		err = c.readControl(ctx, conn)
		c.clearConnection(conn)
		conn.CloseNow()

		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if c.isClosed() {
			return nil
		}
		c.logger.WarnContext(ctx, "worker control WebSocket disconnected", "error", err, "retry_in", backoff)
		if err := c.waitForReconnect(ctx, backoff); err != nil {
			return err
		}
		backoff = nextBackoff(backoff, c.reconnectMax)
	}
}

func (c *ControlClient) readControl(ctx context.Context, conn *websocket.Conn) error {
	conn.SetReadLimit(defaultReadLimit)
	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			continue
		}

		var message controlMessage
		if err := json.Unmarshal(data, &message); err != nil {
			continue
		}
		if message.Type != "terminal-open" {
			continue
		}

		message.StreamID = strings.TrimSpace(message.StreamID)
		message.JobID = strings.TrimSpace(message.JobID)
		message.TmuxSocketPath = strings.TrimSpace(message.TmuxSocketPath)
		if c.registry == nil || !c.registry.Has(message.JobID) {
			if err := c.writeTerminalError(ctx, conn, message.StreamID, "job not active"); err != nil {
				c.logger.WarnContext(ctx, "write terminal error", "stream_id", message.StreamID, "job_id", message.JobID, "error", err)
			}
			continue
		}
		if message.TmuxSocketPath == "" {
			if err := c.writeTerminalError(ctx, conn, message.StreamID, "terminal socket path unavailable"); err != nil {
				c.logger.WarnContext(ctx, "write terminal error", "stream_id", message.StreamID, "job_id", message.JobID, "error", err)
			}
			continue
		}

		go c.openTerminal(ctx, conn, message)
	}
}

func (c *ControlClient) openTerminal(ctx context.Context, controlConn *websocket.Conn, message controlMessage) {
	logger := c.logger.With("stream_id", message.StreamID, "job_id", message.JobID)
	streamURL, err := c.websocketURL("terminal-stream", message.StreamID)
	if err != nil {
		logger.ErrorContext(ctx, "build terminal stream URL", "error", err)
		_ = c.writeTerminalError(ctx, controlConn, message.StreamID, "terminal stream unavailable")
		return
	}

	streamConn, _, err := websocket.Dial(ctx, streamURL, c.dialOptions())
	if err != nil {
		logger.ErrorContext(ctx, "dial terminal stream", "error", err)
		_ = c.writeTerminalError(ctx, controlConn, message.StreamID, "terminal stream unavailable")
		return
	}

	command := terminal.TmuxAttachCommand(terminal.TmuxSessionNameForJob(message.JobID), message.TmuxSocketPath)
	cmd := exec.Command(command[0], command[1:]...)
	// TODO: Replace this local filtering with terminal.TmuxClientEnv once that
	// helper is exported from internal/terminal.
	cmd.Env = append(tmuxClientEnvironment(os.Environ()), "TERM=xterm-256color")
	master, err := StartWithSize(cmd, message.Rows, message.Cols)
	if err != nil {
		_ = streamConn.Close(websocket.StatusInternalError, "terminal command failed")
		logger.ErrorContext(ctx, "start tmux terminal", "error", err)
		_ = c.writeTerminalError(ctx, controlConn, message.StreamID, "terminal command failed")
		return
	}

	if err := NewBridge(master, cmd, streamConn).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.WarnContext(ctx, "terminal stream ended", "error", err)
	}
}

func tmuxClientEnvironment(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, value := range env {
		if strings.HasPrefix(value, "TMUX=") || strings.HasPrefix(value, "TMUX_PANE=") {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func (c *ControlClient) writeTerminalError(ctx context.Context, conn *websocket.Conn, streamID, message string) error {
	payload, err := json.Marshal(terminalError{
		Type:     "terminal-error",
		StreamID: streamID,
		Error:    message,
	})
	if err != nil {
		return fmt.Errorf("marshal terminal error: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("write control WebSocket: %w", err)
	}
	return nil
}

func (c *ControlClient) websocketURL(endpoint, streamID string) (string, error) {
	parsed, err := url.Parse(c.coordinatorURL)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported coordinator URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("coordinator URL has no host")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v2/workers/" + url.PathEscape(c.workerID) + "/" + endpoint
	parsed.RawPath = ""
	query := parsed.Query()
	if streamID != "" {
		query.Set("stream_id", streamID)
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (c *ControlClient) dialOptions() *websocket.DialOptions {
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+c.token)
	return &websocket.DialOptions{HTTPHeader: header}
}

func (c *ControlClient) installConnection(conn *websocket.Conn) bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.closed {
		return false
	}
	if previous := c.conn; previous != nil && previous != conn {
		previous.CloseNow()
	}
	c.conn = conn
	return true
}

func (c *ControlClient) clearConnection(conn *websocket.Conn) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == conn {
		c.conn = nil
	}
}

func (c *ControlClient) isClosed() bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.closed
}

func (c *ControlClient) waitForReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		if c.isClosed() {
			return nil
		}
		return nil
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

// Close stops reconnection and closes the active control WebSocket.
func (c *ControlClient) Close() error {
	c.connMu.Lock()
	if c.closed {
		c.connMu.Unlock()
		return nil
	}
	c.closed = true
	conn := c.conn
	c.conn = nil
	c.connMu.Unlock()

	if conn == nil {
		return nil
	}
	if err := conn.CloseNow(); err != nil && websocket.CloseStatus(err) == -1 {
		return fmt.Errorf("close worker control WebSocket: %w", err)
	}
	return nil
}
