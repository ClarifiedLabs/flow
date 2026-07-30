// Package orchestrator implements the flow-orchestrator scaling controller:
// it polls the coordinator's queue depth and scales the worker Kubernetes
// Deployment, maintaining a standby pool of ephemeral workers.
//
// Scaling policy: target = clamp(max(desired_replicas, queued), min_replicas,
// max_replicas). Scale up immediately; scale down only after the queue has
// been empty for scaledown_idle. The controller never scales while blind: if
// the coordinator or the Kubernetes API is unreachable, the cycle is skipped
// and a poll-error metric is counted.
package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ClarifiedLabs/flow/internal/metrics"
)

// StatsSource reports the coordinator's aggregate job queue depth.
type StatsSource interface {
	QueueStats(ctx context.Context) (QueueStats, error)
}

// QueueStats is the queue-depth signal the controller scales on.
type QueueStats struct {
	Queued           int
	ClaimedOrRunning int
}

// Scaler reads and sets the replica count of the worker Deployment via the
// Kubernetes deployments/scale subresource.
type Scaler interface {
	GetScale(ctx context.Context) (int, error)
	SetScale(ctx context.Context, replicas int) error
}

// Metrics holds the orchestrator's exported metric handles. Any handle may be
// nil, in which case updates for it are skipped.
type Metrics struct {
	QueueDepth            *metrics.Gauge
	WorkerReplicasDesired *metrics.Gauge
	ScaleOperations       *metrics.Counter
	PollErrors            *metrics.Counter
}

// Options configures a Controller.
type Options struct {
	Stats  StatsSource
	Scaler Scaler

	MinReplicas     int
	MaxReplicas     int
	DesiredReplicas int

	PollInterval  time.Duration
	ScaleDownIdle time.Duration

	Metrics Metrics
	// Now overrides the clock; nil uses time.Now. Tests inject it to control
	// the scale-down idle window.
	Now func() time.Time
}

// Controller runs the orchestrator's poll-and-scale loop.
type Controller struct {
	stats  StatsSource
	scaler Scaler

	min     int
	max     int
	desired int

	pollInterval  time.Duration
	scaleDownIdle time.Duration

	metrics Metrics
	now     func() time.Time

	ready     atomic.Bool
	idleSince *time.Time
}

// NewController validates options and returns a Controller.
func NewController(opts Options) *Controller {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Controller{
		stats:         opts.Stats,
		scaler:        opts.Scaler,
		min:           opts.MinReplicas,
		max:           opts.MaxReplicas,
		desired:       opts.DesiredReplicas,
		pollInterval:  opts.PollInterval,
		scaleDownIdle: opts.ScaleDownIdle,
		metrics:       opts.Metrics,
		now:           now,
	}
}

// SetScaler wires the Deployment scaler after construction; the in-cluster
// Kubernetes client can fail to initialize, so it is created and injected
// separately from the other options.
func (c *Controller) SetScaler(s Scaler) { c.scaler = s }

// Ready reports whether the controller has completed at least one successful
// poll of both the coordinator and the Kubernetes API.
func (c *Controller) Ready() bool { return c.ready.Load() }

// Run polls and scales until ctx is canceled. The first cycle runs
// immediately; cycle failures are logged and counted, never fatal.
func (c *Controller) Run(ctx context.Context) error {
	if c.scaler == nil {
		return errors.New("orchestrator scaler is not configured")
	}
	c.runCycle(ctx)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.runCycle(ctx)
		}
	}
}

// target computes the desired replica count for a queue depth.
func (c *Controller) target(queued int) int {
	target := c.desired
	if queued > target {
		target = queued
	}
	if target < c.min {
		target = c.min
	}
	if target > c.max {
		target = c.max
	}
	return target
}

func (c *Controller) runCycle(ctx context.Context) {
	stats, err := c.stats.QueueStats(ctx)
	if err != nil {
		slog.Warn("orchestrator coordinator poll failed; skipping cycle", "error", err)
		c.pollError("coordinator")
		return
	}
	current, err := c.scaler.GetScale(ctx)
	if err != nil {
		slog.Warn("orchestrator kubernetes poll failed; skipping cycle", "error", err)
		c.pollError("k8s")
		return
	}
	c.ready.Store(true)

	c.setGauge(c.metrics.QueueDepth, float64(stats.Queued), nil)
	c.setGauge(c.metrics.WorkerReplicasDesired, float64(current), nil)

	target := c.target(stats.Queued)

	// Track the queue-empty window for scale-down delay.
	if stats.Queued > 0 {
		c.idleSince = nil
	} else if c.idleSince == nil {
		since := c.now()
		c.idleSince = &since
	}

	switch {
	case target > current:
		// Scale up immediately: queued work or a standby-pool deficit waits
		// for no idle window.
		c.scale(ctx, target, "up")
	case target < current:
		canScaleDown := stats.Queued == 0 &&
			c.idleSince != nil &&
			c.now().Sub(*c.idleSince) >= c.scaleDownIdle
		if canScaleDown {
			c.scale(ctx, target, "down")
		}
	}
}

func (c *Controller) scale(ctx context.Context, replicas int, direction string) {
	if err := c.scaler.SetScale(ctx, replicas); err != nil {
		slog.Warn("orchestrator scale failed", "replicas", replicas, "direction", direction, "error", err)
		c.pollError("k8s")
		return
	}
	slog.Info("orchestrator scaled worker deployment", "replicas", replicas, "direction", direction)
	if c.metrics.ScaleOperations != nil {
		c.metrics.ScaleOperations.Inc(map[string]string{"direction": direction})
	}
	c.setGauge(c.metrics.WorkerReplicasDesired, float64(replicas), nil)
}

func (c *Controller) pollError(source string) {
	if c.metrics.PollErrors != nil {
		c.metrics.PollErrors.Inc(map[string]string{"source": source})
	}
}

func (c *Controller) setGauge(gauge *metrics.Gauge, value float64, labels map[string]string) {
	if gauge != nil {
		gauge.Set(value, labels)
	}
}
