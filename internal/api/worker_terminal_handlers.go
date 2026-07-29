package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/coder/websocket"
)

type workerTerminalErrorMessage struct {
	Type     string `json:"type"`
	StreamID string `json:"stream_id"`
	Error    string `json:"error"`
}

func (s *Server) dispatchWorkerPath(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/workers/"), "/")
	if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" {
		switch parts[1] {
		case "control":
			s.handleWorkerControl(w, r, principal)
			return
		case "terminal-stream":
			s.handleWorkerTerminalStream(w, r, principal)
			return
		}
	}
	s.handleWorkerPath(w, r, principal)
}

func (s *Server) handleWorkerControl(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	workerID := workerTerminalPathID(r.URL.Path)
	if principal.Scope != coordinator.TokenScopeWorker || principal.Subject != workerID {
		writeError(w, http.StatusForbidden, "forbidden", "matching worker token is required")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.WarnContext(r.Context(), "accept worker control WebSocket", "worker_id", workerID, "error", err)
		return
	}
	if err := s.workerTerminals.RegisterControl(workerID, conn); err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		slog.WarnContext(r.Context(), "register worker control WebSocket", "worker_id", workerID, "error", err)
		return
	}
	defer s.workerTerminals.UnregisterControl(workerID)
	defer conn.Close(websocket.StatusNormalClosure, "worker control disconnected")

	conn.SetReadLimit(64 << 10)
	for {
		messageType, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var message workerTerminalErrorMessage
		if err := json.Unmarshal(data, &message); err != nil || message.Type != "terminal-error" {
			continue
		}
		message.StreamID = strings.TrimSpace(message.StreamID)
		message.Error = strings.TrimSpace(message.Error)
		if message.StreamID == "" {
			continue
		}
		streamErr := errors.New("worker terminal open failed")
		if message.Error != "" {
			streamErr = errors.New(message.Error)
		}
		s.workerTerminals.FailStream(message.StreamID, workerID, streamErr)
		slog.Warn("worker terminal open failed",
			"worker_id", workerID,
			"stream_id", message.StreamID,
			"error", message.Error,
		)
	}
}

func (s *Server) handleWorkerTerminalStream(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	workerID := workerTerminalPathID(r.URL.Path)
	if principal.Scope != coordinator.TokenScopeWorker || principal.Subject != workerID {
		writeError(w, http.StatusForbidden, "forbidden", "matching worker token is required")
		return
	}
	streamID := strings.TrimSpace(r.URL.Query().Get("stream_id"))
	if streamID == "" {
		writeError(w, http.StatusBadRequest, "invalid_stream_id", "stream_id is required")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.WarnContext(r.Context(), "accept worker terminal stream WebSocket", "worker_id", workerID, "stream_id", streamID, "error", err)
		return
	}
	if err := s.workerTerminals.CompleteStream(streamID, conn); err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		if websocket.CloseStatus(err) == -1 {
			slog.Warn("worker terminal stream ended", "worker_id", workerID, "stream_id", streamID, "error", err)
		}
	}
}

func workerTerminalPathID(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/v2/workers/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}
