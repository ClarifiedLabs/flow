package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// eventsResponse is the poll shape for the project event log. next_since is
// the cursor for the following page: the last returned seq, or the caller's
// own cursor when the page is empty. reset_required reserves purge-cursor
// semantics: flow never deletes events today, so it is always
// false; a consumer that sees true must resync from seq 0.
type eventsResponse struct {
	Events        []coordinator.Event `json:"events"`
	NextSince     int64               `json:"next_since"`
	ResetRequired bool                `json:"reset_required"`
}

// eventStreamPollInterval bounds how often the SSE handler re-queries the
// log when idle. Keepalives ride the same cadence.
const eventStreamPollInterval = time.Second

func parseEventSinceLimit(r *http.Request) (int64, int, error) {
	since := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, fmt.Errorf("invalid since %q: want a non-negative integer cursor", raw)
		}
		since = parsed
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return 0, 0, fmt.Errorf("invalid limit %q: want a positive integer", raw)
		}
		limit = parsed
	}
	return since, limit, nil
}

// parseEventFilter reads the optional kind/task/actor filters. Blank means no
// filter; a set-but-empty value is rejected so a typoed `?kind=` is loud.
func parseEventFilter(r *http.Request) (coordinator.EventFilter, error) {
	var filter coordinator.EventFilter
	query := r.URL.Query()
	for _, param := range []struct {
		name string
		dst  *string
	}{{"kind", &filter.Kind}, {"task", &filter.TaskID}, {"actor", &filter.Actor}} {
		values, present := query[param.name]
		if present && len(values) == 1 && strings.TrimSpace(values[0]) == "" {
			return filter, fmt.Errorf("invalid %s: filter must be non-empty when present", param.name)
		}
		if present && len(values) > 0 {
			*param.dst = values[0]
		}
	}
	return filter, nil
}

// handleEvents serves one page of the project event log:
// GET /v2/projects/{project}/events?since=N&limit=N&kind=K&task=T&actor=A.
func (ps *projectServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	since, limit, err := parseEventSinceLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	filter, err := parseEventFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	events, err := ps.eventLog.ListFiltered(r.Context(), since, limit, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "event_log_unavailable", err.Error())
		return
	}
	next := since
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	writeJSON(w, http.StatusOK, eventsResponse{Events: events, NextSince: next, ResetRequired: false})
}

// handleEventStream streams the project event log as server-sent events:
// GET /v2/projects/{project}/events/stream?since=N. Each frame's data is one
// event JSON document; the frame id is the event's seq so clients can resume
// with Last-Event-ID semantics or their own cursor.
func (ps *projectServer) handleEventStream(w http.ResponseWriter, r *http.Request) {
	since, _, err := parseEventSinceLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "response writer cannot stream")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: %d\n\n", eventStreamPollInterval/time.Millisecond*2)
	flusher.Flush()

	ctx := r.Context()
	for {
		events, err := ps.eventLog.List(ctx, since, coordinator.EventLogMaxLimit)
		if err != nil {
			// The context canceled path ends the stream quietly; a real query
			// failure is surfaced as an SSE comment so clients can log it.
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(w, ": event log error: %s\n\n", err.Error())
			flusher.Flush()
			return
		}
		for _, event := range events {
			encoded, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Seq, encoded)
			since = event.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			continue
		}
		fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()
		select {
		case <-ctx.Done():
			return
		case <-time.After(eventStreamPollInterval):
		}
	}
}
