// Package hooks implements server-configured post-commit hooks: after an
// event-log entry commits in a project database, matching entries dispatch an
// external executable with the event as JSON on stdin.
//
// Dispatch is fully asynchronous and at-most-once. The dispatcher tails
// committed rows by polling EventLogService.List on the same ~1s cadence the
// SSE handler uses, so hook execution can never block or roll back the
// mutation that produced an event. The trade-off: a crash between commit and
// dispatch loses the hook run, each boot starts its per-project cursor at the
// then-current max seq (events committed while the server was down never
// fire), and a full dispatch queue drops runs (counted, not retried).
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/metrics"
)

const (
	// defaultPollInterval matches the SSE handler's eventStreamPollInterval.
	defaultPollInterval = time.Second
	// defaultRunTimeout bounds one hook execution.
	defaultRunTimeout = 30 * time.Second
	// defaultWorkers bounds concurrent hook executions.
	defaultWorkers = 4
	// defaultQueueSize bounds pending hook runs; a full queue drops runs.
	defaultQueueSize = 256
	// maxPayloadBytes caps the raw event payload inside the envelope.
	maxPayloadBytes = 64 << 10
	// maxEnvelopeBytes caps the whole stdin document.
	maxEnvelopeBytes = 256 << 10
	// maxStderrBytes caps captured hook stderr kept for the server log.
	maxStderrBytes = 4 << 10
)

// ProjectSource is one project's event log the dispatcher tails.
type ProjectSource struct {
	ProjectID string
	EventLog  *coordinator.EventLogService
}

// run is one queued hook execution.
type run struct {
	hook      config.HookConfig
	projectID string
	event     coordinator.Event
}

// Dispatcher tails project event logs and runs matching hooks. The zero
// value is not usable; construct with New. PollInterval, RunTimeout,
// Workers, and QueueSize may be overridden before Run (tests do).
type Dispatcher struct {
	hooks   []config.HookConfig
	sources func() []ProjectSource

	// PollInterval is the tail cadence (default 1s, the SSE cadence).
	PollInterval time.Duration
	// RunTimeout bounds one hook execution (default 30s).
	RunTimeout time.Duration
	// Workers bounds concurrent hook executions (default 4).
	Workers int
	// QueueSize bounds queued runs before drops (default 256).
	QueueSize int

	dropped  *metrics.Counter
	failures *metrics.Counter

	// seedSignals holds a per-project channel closed when that project's
	// cursor is first seeded, so tests can append only after seeding.
	seedMu      sync.Mutex
	seedSignals map[string]chan struct{}
}

// New returns a Dispatcher for hooks over the projects sources returns.
// sources is re-queried every poll cycle so projects created while the
// server runs join the tail (starting at their current max seq). registry
// may be nil, disabling metrics.
func New(hooks []config.HookConfig, sources func() []ProjectSource, registry *metrics.Registry) *Dispatcher {
	d := &Dispatcher{
		hooks:        hooks,
		sources:      sources,
		PollInterval: defaultPollInterval,
		RunTimeout:   defaultRunTimeout,
		Workers:      defaultWorkers,
		QueueSize:    defaultQueueSize,
	}
	if registry != nil {
		d.dropped = registry.Counter("flow_hook_runs_dropped_total", "Post-commit hook runs dropped because the dispatch queue was full.")
		d.failures = registry.Counter("flow_hook_run_failures_total", "Post-commit hook runs that failed (start failure, non-zero exit, or timeout).")
	}
	return d
}

// Seeded returns a channel closed when the given project's cursor has been
// seeded (its first poll completed). Tests use it to append events only after
// seeding so they are never skipped. Production callers need not wait.
func (d *Dispatcher) Seeded(projectID string) <-chan struct{} {
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	if d.seedSignals == nil {
		d.seedSignals = map[string]chan struct{}{}
	}
	ch, ok := d.seedSignals[projectID]
	if !ok {
		ch = make(chan struct{})
		d.seedSignals[projectID] = ch
	}
	return ch
}

// markSeeded closes the project's seed signal, if any test registered one.
func (d *Dispatcher) markSeeded(projectID string) {
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	if ch, ok := d.seedSignals[projectID]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// Run tails every source until ctx is done, then waits for in-flight and
// queued runs to finish (each still bounded by RunTimeout). It is
// synchronous; callers run it in a goroutine tied to server shutdown.
func (d *Dispatcher) Run(ctx context.Context) {
	queue := make(chan run, d.QueueSize)
	var wg sync.WaitGroup
	for i := 0; i < d.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				d.execute(ctx, job)
			}
		}()
	}
	defer func() {
		close(queue)
		wg.Wait()
	}()

	// cursors tracks the last dispatched seq per project. A project first
	// seen by the poll loop starts at its current max seq: events that
	// committed while the dispatcher was down never fire (at-most-once).
	cursors := map[string]int64{}
	ticker := time.NewTicker(d.PollInterval)
	defer ticker.Stop()
	for {
		d.poll(ctx, cursors, queue)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// poll refreshes the source list, seeds cursors for newly seen projects at
// their tail, and enqueues hook runs for every event past each cursor.
// Query failures keep the cursor and are retried next cycle.
func (d *Dispatcher) poll(ctx context.Context, cursors map[string]int64, queue chan<- run) {
	sources := d.sources()
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		if source.EventLog == nil || source.ProjectID == "" {
			continue
		}
		seen[source.ProjectID] = true
		cursor, tracked := cursors[source.ProjectID]
		if !tracked {
			latest, err := source.EventLog.LatestSeq(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("hook dispatcher: read latest seq failed", "project_id", source.ProjectID, "error", err)
				}
				continue
			}
			cursor = latest
			cursors[source.ProjectID] = cursor
			d.markSeeded(source.ProjectID)
		}
		// Drain full pages so a burst is dispatched without waiting for extra
		// poll cycles.
		for {
			events, err := source.EventLog.List(ctx, cursor, coordinator.EventLogMaxLimit)
			slog.Debug("hook dispatcher poll", "project_id", source.ProjectID, "cursor", cursor, "events", len(events), "err", err)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("hook dispatcher: list events failed", "project_id", source.ProjectID, "error", err)
				}
				break
			}
			for _, event := range events {
				cursor = event.Seq
				for _, hook := range d.hooks {
					if matchAny(hook.Events, event.Kind) {
						d.enqueue(queue, hook, source.ProjectID, event)
					}
				}
			}
			if len(events) < coordinator.EventLogMaxLimit {
				break
			}
		}
		cursors[source.ProjectID] = cursor
	}
	// Forget projects that left the registry so a re-added project starts at
	// its tail again.
	for projectID := range cursors {
		if !seen[projectID] {
			delete(cursors, projectID)
		}
	}
}

// enqueue offers a run to the worker pool without blocking: a full queue
// drops the run (at-most-once), counting and logging the loss.
func (d *Dispatcher) enqueue(queue chan<- run, hook config.HookConfig, projectID string, event coordinator.Event) {
	select {
	case queue <- run{hook: hook, projectID: projectID, event: event}:
	default:
		inc(d.dropped)
		slog.Warn("hook run dropped: dispatch queue full",
			"project_id", projectID, "kind", event.Kind, "seq", event.Seq, "command", hook.Command[0])
	}
}

// execute runs one hook: the event envelope on stdin, FLOW_EVENT_* in the
// environment, no shell, bounded by RunTimeout. Failures are counted and
// logged, never propagated — a hook must not affect the server.
func (d *Dispatcher) execute(ctx context.Context, job run) {
	runCtx, cancel := context.WithTimeout(ctx, d.RunTimeout)
	defer cancel()
	if err := d.runCommand(runCtx, job); err != nil {
		inc(d.failures)
		slog.Warn("hook run failed",
			"project_id", job.projectID, "kind", job.event.Kind, "seq", job.event.Seq,
			"command", job.hook.Command[0], "error", err)
	}
}

func (d *Dispatcher) runCommand(ctx context.Context, job run) error {
	cmd := exec.CommandContext(ctx, job.hook.Command[0], job.hook.Command[1:]...)
	cmd.Stdin = bytes.NewReader(buildEnvelope(job.projectID, job.event))
	cmd.Env = append(os.Environ(),
		"FLOW_EVENT_KIND="+job.event.Kind,
		"FLOW_EVENT_SEQ="+strconv.FormatInt(job.event.Seq, 10),
		"FLOW_PROJECT_ID="+job.projectID,
		"FLOW_TASK_ID="+job.event.TaskID,
	)
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if len(stderr.buf) > 0 {
		slog.Debug("hook run stderr",
			"project_id", job.projectID, "kind", job.event.Kind, "seq", job.event.Seq,
			"command", job.hook.Command[0], "stderr", string(stderr.buf))
	}
	return err
}

// matchAny reports whether kind matches any pattern: an exact kind, or a
// single trailing ".*" glob prefix ("task.*" matches "task.done", not
// "task" or "tasks.done").
func matchAny(patterns []string, kind string) bool {
	for _, pattern := range patterns {
		if pattern == kind {
			return true
		}
		if prefix, ok := strings.CutSuffix(pattern, ".*"); ok && strings.HasPrefix(kind, prefix+".") {
			return true
		}
	}
	return false
}

// envelope is the stdin document handed to a hook.
type envelope struct {
	Seq        int64           `json:"seq"`
	Kind       string          `json:"kind"`
	ProjectID  string          `json:"project_id"`
	TaskID     string          `json:"task_id,omitempty"`
	Actor      string          `json:"actor,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

// envelopeHeader carries every envelope field but the payload; buildEnvelope
// splices the payload bytes in raw because a payload truncated at the byte
// cap may no longer be valid JSON, which json.RawMessage would refuse to
// marshal (dropping the payload entirely).
type envelopeHeader struct {
	Seq        int64     `json:"seq"`
	Kind       string    `json:"kind"`
	ProjectID  string    `json:"project_id"`
	TaskID     string    `json:"task_id,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// buildEnvelope marshals the event for stdin. The raw payload is byte-capped
// at maxPayloadBytes (truncation may leave it no longer parseable; hooks must
// treat payload as best-effort detail) and the whole document is hard-capped
// at maxEnvelopeBytes.
func buildEnvelope(projectID string, event coordinator.Event) []byte {
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if len(payload) > maxPayloadBytes {
		payload = payload[:maxPayloadBytes]
	}
	head, err := json.Marshal(envelopeHeader{
		Seq:        event.Seq,
		Kind:       event.Kind,
		ProjectID:  projectID,
		TaskID:     event.TaskID,
		Actor:      event.Actor,
		OccurredAt: event.OccurredAt.UTC(),
	})
	if err != nil {
		// Unreachable in practice (scalars, strings, and a DB-parsed time).
		slog.Warn("hook envelope header marshal failed", "kind", event.Kind, "seq", event.Seq, "error", err)
		return []byte(`{"seq":0,"kind":"","project_id":"","payload":{}}`)
	}
	// head ends in '}'; append the payload member.
	var buf bytes.Buffer
	buf.Grow(len(head) + len(payload) + 16)
	buf.Write(head[:len(head)-1])
	buf.WriteString(`,"payload":`)
	buf.Write(payload)
	buf.WriteByte('}')
	encoded := buf.Bytes()
	if len(encoded) > maxEnvelopeBytes {
		encoded = encoded[:maxEnvelopeBytes]
	}
	return encoded
}

// cappedBuffer is a bytes.Buffer that discards writes past maxStderrBytes.
type cappedBuffer struct {
	buf []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := maxStderrBytes - len(b.buf)
	if remaining > 0 {
		if len(p) > remaining {
			b.buf = append(b.buf, p[:remaining]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func inc(counter *metrics.Counter) {
	if counter != nil {
		counter.Inc(nil)
	}
}
