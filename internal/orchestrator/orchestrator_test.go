package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/metrics"
)

// fakeStats is a scripted StatsSource.
type fakeStats struct {
	mu      sync.Mutex
	stats   QueueStats
	err     error
	calls   int
	onCalls func()
}

func (f *fakeStats) QueueStats(ctx context.Context) (QueueStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.stats, f.err
}

func (f *fakeStats) set(stats QueueStats, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats = stats
	f.err = err
}

// fakeScaler records scale operations and scripts failures.
type fakeScaler struct {
	mu       sync.Mutex
	current  int
	setErr   error
	getErr   error
	setCalls []int
}

func (f *fakeScaler) GetScale(ctx context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.getErr
}

func (f *fakeScaler) SetScale(ctx context.Context, replicas int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, replicas)
	f.current = replicas
	return nil
}

func (f *fakeScaler) sets() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.setCalls...)
}

func testController(stats StatsSource, scaler Scaler, now func() time.Time) *Controller {
	return NewController(Options{
		Stats:           stats,
		Scaler:          scaler,
		MinReplicas:     0,
		MaxReplicas:     10,
		DesiredReplicas: 1,
		PollInterval:    time.Hour,
		ScaleDownIdle:   2 * time.Minute,
		Now:             now,
	})
}

func TestControllerScaleUpImmediately(t *testing.T) {
	stats := &fakeStats{stats: QueueStats{Queued: 4}}
	scaler := &fakeScaler{current: 1}
	c := testController(stats, scaler, time.Now)

	c.runCycle(context.Background())

	sets := scaler.sets()
	if len(sets) != 1 || sets[0] != 4 {
		t.Fatalf("scale sets = %v, want [4]", sets)
	}
	if !c.Ready() {
		t.Fatal("controller not ready after successful cycle")
	}
}

func TestControllerStandbyPoolFloor(t *testing.T) {
	stats := &fakeStats{stats: QueueStats{Queued: 0}}
	scaler := &fakeScaler{current: 0}
	c := testController(stats, scaler, time.Now)

	// Empty queue but below the desired standby pool: scale up immediately,
	// no idle wait.
	c.runCycle(context.Background())
	if sets := scaler.sets(); len(sets) != 1 || sets[0] != 1 {
		t.Fatalf("scale sets = %v, want [1]", sets)
	}
}

func TestControllerClampsToMinMax(t *testing.T) {
	stats := &fakeStats{stats: QueueStats{Queued: 100}}
	scaler := &fakeScaler{current: 0}
	c := testController(stats, scaler, time.Now)

	c.runCycle(context.Background())
	if sets := scaler.sets(); len(sets) != 1 || sets[0] != 10 {
		t.Fatalf("scale sets = %v, want clamped [10]", sets)
	}
}

func TestControllerScaleDownOnlyAfterIdle(t *testing.T) {
	current := time.Now()
	now := func() time.Time { return current }
	stats := &fakeStats{stats: QueueStats{Queued: 2}}
	scaler := &fakeScaler{current: 2}
	c := testController(stats, scaler, now)

	// Queue drains: the idle window starts, but scaledown_idle has not elapsed.
	stats.set(QueueStats{Queued: 0}, nil)
	c.runCycle(context.Background())
	current = current.Add(time.Minute)
	c.runCycle(context.Background())
	if sets := scaler.sets(); len(sets) != 0 {
		t.Fatalf("scale sets = %v, want none before idle window", sets)
	}

	// After scaledown_idle with an empty queue, scale down to the standby pool.
	current = current.Add(61 * time.Second)
	c.runCycle(context.Background())
	if sets := scaler.sets(); len(sets) != 1 || sets[0] != 1 {
		t.Fatalf("scale sets = %v, want [1]", sets)
	}

	// New work resets the idle window and scales up immediately.
	stats.set(QueueStats{Queued: 3}, nil)
	c.runCycle(context.Background())
	if sets := scaler.sets(); len(sets) != 2 || sets[1] != 3 {
		t.Fatalf("scale sets = %v, want [1 3]", sets)
	}
}

func TestControllerNeverScalesWhileBlind(t *testing.T) {
	stats := &fakeStats{stats: QueueStats{Queued: 5}, err: errors.New("coordinator down")}
	scaler := &fakeScaler{current: 1}
	c := testController(stats, scaler, time.Now)

	c.runCycle(context.Background())
	if sets := scaler.sets(); len(sets) != 0 {
		t.Fatalf("scale sets = %v, want none while coordinator is unreachable", sets)
	}
	if c.Ready() {
		t.Fatal("controller ready without a successful poll")
	}

	stats.set(QueueStats{Queued: 5}, nil)
	scaler.mu.Lock()
	scaler.getErr = errors.New("k8s down")
	scaler.mu.Unlock()
	c.runCycle(context.Background())
	if sets := scaler.sets(); len(sets) != 0 {
		t.Fatalf("scale sets = %v, want none while k8s is unreachable", sets)
	}
	if c.Ready() {
		t.Fatal("controller ready without a successful k8s poll")
	}
}

func TestControllerMetrics(t *testing.T) {
	reg := metrics.New()
	m := Metrics{
		QueueDepth:            reg.Gauge("flow_queue_depth", "queue depth"),
		WorkerReplicasDesired: reg.Gauge("flow_worker_replicas_desired", "worker replicas"),
		ScaleOperations:       reg.Counter("flow_orchestrator_scale_operations_total", "scale ops"),
		PollErrors:            reg.Counter("flow_orchestrator_poll_errors_total", "poll errors"),
	}
	stats := &fakeStats{err: errors.New("down")}
	scaler := &fakeScaler{current: 2}
	c := NewController(Options{
		Stats:           stats,
		Scaler:          scaler,
		MinReplicas:     0,
		MaxReplicas:     10,
		DesiredReplicas: 1,
		PollInterval:    time.Hour,
		ScaleDownIdle:   0,
		Metrics:         m,
	})

	c.runCycle(context.Background())
	stats.set(QueueStats{Queued: 3}, nil)
	c.runCycle(context.Background())

	buf := &strings.Builder{}
	reg.Render(buf)
	rendered := buf.String()
	for _, want := range []string{
		`flow_orchestrator_poll_errors_total{source="coordinator"} 1`,
		`flow_orchestrator_scale_operations_total{direction="up"} 1`,
		`flow_queue_depth 3`,
		`flow_worker_replicas_desired 3`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered metrics missing %q:\n%s", want, rendered)
		}
	}
}

func TestCoordinatorPoller(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/queue/stats" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer orch-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"queued":3,"claimed_or_running":1,"by_bucket":{"ephemeral":3}}`))
	}))
	t.Cleanup(server.Close)

	poller := NewCoordinatorPoller(server.URL, "orch-token", server.Client())
	stats, err := poller.QueueStats(context.Background())
	if err != nil {
		t.Fatalf("QueueStats() error = %v", err)
	}
	if stats.Queued != 3 || stats.ClaimedOrRunning != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	badPoller := NewCoordinatorPoller(server.URL, "wrong-token", server.Client())
	if _, err := badPoller.QueueStats(context.Background()); err == nil {
		t.Fatal("QueueStats() with bad token succeeded, want error")
	}
}

func TestK8sScaler(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	replicas := 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"spec":{"replicas":2}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotBody = string(body)
		var decoded scaleBody
		if err := json.Unmarshal(body, &decoded); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		replicas = decoded.Spec.Replicas
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	scaler := NewK8sScaler(server.URL, "sa-token", server.Client(), "flow", "flow-worker")

	current, err := scaler.GetScale(context.Background())
	if err != nil {
		t.Fatalf("GetScale() error = %v", err)
	}
	if current != 2 {
		t.Fatalf("GetScale() = %d, want 2", current)
	}
	wantPath := "/apis/apps/v1/namespaces/flow/deployments/flow-worker/scale"
	if gotPath != wantPath {
		t.Fatalf("path = %q, want %q", gotPath, wantPath)
	}
	if gotAuth != "Bearer sa-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}

	if err := scaler.SetScale(context.Background(), 5); err != nil {
		t.Fatalf("SetScale() error = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", gotMethod)
	}
	if replicas != 5 {
		t.Fatalf("replicas after SetScale = %d, want 5; body %s", replicas, gotBody)
	}
}
