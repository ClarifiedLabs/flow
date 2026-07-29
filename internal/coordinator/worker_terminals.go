package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const defaultWorkerTerminalTimeout = 15 * time.Second

// WorkerTerminalService coordinates browser terminal connections with worker
// dial-back streams. Terminal frames are relayed opaquely after a stream is
// paired.
type WorkerTerminalService struct {
	mu       sync.Mutex
	controls map[string]*workerControlConn
	pending  map[string]*pendingStream
	timeout  time.Duration
	closed   bool
}

type workerControlConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

type pendingStream struct {
	browserConn *websocket.Conn
	workerConn  *websocket.Conn
	workerID    string
	done        chan struct{}
	err         error
}

type terminalOpenMessage struct {
	Type           string `json:"type"`
	StreamID       string `json:"stream_id"`
	JobID          string `json:"job_id"`
	TmuxSocketPath string `json:"tmux_socket_path"`
	Cols           uint16 `json:"cols"`
	Rows           uint16 `json:"rows"`
}

func NewWorkerTerminalService() *WorkerTerminalService {
	return &WorkerTerminalService{
		controls: make(map[string]*workerControlConn),
		pending:  make(map[string]*pendingStream),
		timeout:  defaultWorkerTerminalTimeout,
	}
}

// NewTerminalStreamID returns a random identifier suitable for one terminal
// dial-back request.
func NewTerminalStreamID() (string, error) {
	return randomPrefixedID("st")
}

// RegisterControl installs the persistent control connection for workerID. The
// caller retains ownership of the connection read loop.
func (svc *WorkerTerminalService) RegisterControl(workerID string, conn *websocket.Conn) error {
	if workerID == "" {
		return errors.New("worker id is required")
	}
	if conn == nil {
		return errors.New("worker control connection is required")
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.closed {
		return errors.New("worker terminal service is closed")
	}
	if _, exists := svc.controls[workerID]; exists {
		return fmt.Errorf("worker %s already has a control connection", workerID)
	}
	svc.controls[workerID] = &workerControlConn{conn: conn}
	return nil
}

func (svc *WorkerTerminalService) UnregisterControl(workerID string) {
	svc.mu.Lock()
	delete(svc.controls, workerID)
	svc.mu.Unlock()
}

func (svc *WorkerTerminalService) ControlConn(workerID string) (*workerControlConn, bool) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	control, ok := svc.controls[workerID]
	return control, ok
}

// OpenStream asks a worker to dial back, then waits until CompleteStream pairs
// the worker connection with browserConn. CompleteStream owns the relay after
// the pair is formed.
func (svc *WorkerTerminalService) OpenStream(streamID, workerID string, browserConn *websocket.Conn, jobID, tmuxSocketPath string, cols, rows uint16) error {
	if streamID == "" {
		return errors.New("stream id is required")
	}
	if browserConn == nil {
		return errors.New("browser terminal connection is required")
	}
	tmuxSocketPath = strings.TrimSpace(tmuxSocketPath)
	if tmuxSocketPath == "" {
		return errors.New("tmux socket path is required")
	}

	pending := &pendingStream{
		browserConn: browserConn,
		workerID:    workerID,
		done:        make(chan struct{}),
	}

	svc.mu.Lock()
	if svc.closed {
		svc.mu.Unlock()
		return errors.New("worker terminal service is closed")
	}
	control, ok := svc.controls[workerID]
	if !ok {
		svc.mu.Unlock()
		return fmt.Errorf("worker %s has no control connection", workerID)
	}
	if _, exists := svc.pending[streamID]; exists {
		svc.mu.Unlock()
		return fmt.Errorf("terminal stream %s already exists", streamID)
	}
	svc.pending[streamID] = pending
	timeout := svc.timeout
	svc.mu.Unlock()

	payload, err := json.Marshal(terminalOpenMessage{
		Type:           "terminal-open",
		StreamID:       streamID,
		JobID:          jobID,
		TmuxSocketPath: tmuxSocketPath,
		Cols:           cols,
		Rows:           rows,
	})
	if err != nil {
		svc.CancelStream(streamID, fmt.Errorf("marshal terminal open: %w", err))
		return err
	}

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), timeout)
	control.writeMu.Lock()
	err = control.conn.Write(writeCtx, websocket.MessageText, payload)
	control.writeMu.Unlock()
	cancelWrite()
	if err != nil {
		err = fmt.Errorf("send terminal open to worker %s: %w", workerID, err)
		svc.CancelStream(streamID, err)
		return err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-pending.done:
		return pending.err
	case <-timer.C:
		timeoutErr := errors.New("worker did not open stream")
		svc.mu.Lock()
		if current, exists := svc.pending[streamID]; exists && current == pending {
			delete(svc.pending, streamID)
			pending.err = timeoutErr
			close(pending.done)
			svc.mu.Unlock()
			_ = browserConn.Close(websocket.StatusPolicyViolation, timeoutErr.Error())
			return timeoutErr
		}
		svc.mu.Unlock()
		<-pending.done
		return pending.err
	}
}

// CompleteStream pairs a worker dial-back with its pending browser connection
// and relays complete WebSocket frames in both directions until either side
// closes.
func (svc *WorkerTerminalService) CompleteStream(streamID string, workerConn *websocket.Conn) error {
	if workerConn == nil {
		return errors.New("worker terminal connection is required")
	}

	svc.mu.Lock()
	pending, ok := svc.pending[streamID]
	if !ok {
		svc.mu.Unlock()
		return fmt.Errorf("terminal stream %s is not pending", streamID)
	}
	delete(svc.pending, streamID)
	pending.workerConn = workerConn
	close(pending.done)
	svc.mu.Unlock()

	return spliceTerminalConnections(pending.browserConn, workerConn)
}

func (svc *WorkerTerminalService) CancelStream(streamID string, err error) {
	if err == nil {
		err = errors.New("terminal stream canceled")
	}
	svc.mu.Lock()
	pending, ok := svc.pending[streamID]
	if ok {
		delete(svc.pending, streamID)
		pending.err = err
		close(pending.done)
	}
	svc.mu.Unlock()
}

// FailStream cancels a pending stream only when it belongs to workerID. It is
// used for terminal-error messages received on that worker's control channel.
func (svc *WorkerTerminalService) FailStream(streamID, workerID string, err error) bool {
	if err == nil {
		err = errors.New("worker terminal open failed")
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	pending, ok := svc.pending[streamID]
	if !ok || pending.workerID != workerID {
		return false
	}
	delete(svc.pending, streamID)
	pending.err = err
	close(pending.done)
	return true
}

func (svc *WorkerTerminalService) Close() {
	svc.mu.Lock()
	if svc.closed {
		svc.mu.Unlock()
		return
	}
	svc.closed = true
	controls := make([]*workerControlConn, 0, len(svc.controls))
	for _, control := range svc.controls {
		controls = append(controls, control)
	}
	pending := make([]*pendingStream, 0, len(svc.pending))
	for _, stream := range svc.pending {
		stream.err = errors.New("worker terminal service is closed")
		close(stream.done)
		pending = append(pending, stream)
	}
	clear(svc.controls)
	clear(svc.pending)
	svc.mu.Unlock()

	for _, control := range controls {
		_ = control.conn.Close(websocket.StatusGoingAway, "coordinator shutting down")
	}
	for _, stream := range pending {
		_ = stream.browserConn.Close(websocket.StatusGoingAway, "coordinator shutting down")
		if stream.workerConn != nil {
			_ = stream.workerConn.Close(websocket.StatusGoingAway, "coordinator shutting down")
		}
	}
}

func spliceTerminalConnections(browserConn, workerConn *websocket.Conn) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan error, 2)
	go copyTerminalFrames(ctx, workerConn, browserConn, errs)
	go copyTerminalFrames(ctx, browserConn, workerConn, errs)

	firstErr := <-errs
	cancel()
	closed := make(chan struct{}, 2)
	go func() {
		_ = browserConn.Close(websocket.StatusNormalClosure, "terminal stream closed")
		closed <- struct{}{}
	}()
	go func() {
		_ = workerConn.Close(websocket.StatusNormalClosure, "terminal stream closed")
		closed <- struct{}{}
	}()
	<-closed
	<-closed
	<-errs

	if firstErr == nil || errors.Is(firstErr, context.Canceled) || errors.Is(firstErr, io.EOF) || websocket.CloseStatus(firstErr) != -1 {
		return nil
	}
	return firstErr
}

func copyTerminalFrames(ctx context.Context, dst, src *websocket.Conn, errs chan<- error) {
	for {
		messageType, data, err := src.Read(ctx)
		if err != nil {
			errs <- err
			return
		}
		if err := dst.Write(ctx, messageType, data); err != nil {
			errs <- err
			return
		}
	}
}
