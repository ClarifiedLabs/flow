# Kubernetes deployment

Flow runs on Kubernetes as three Deployments: `flow-server` (coordinator),
`flow-worker` (ephemeral workers), and `flow-orchestrator` (autoscaler). The
reference manifests live in [`k8s/`](../k8s).

```sh
kubectl apply -f k8s/
```

Generate real token values first (the manifest ships `CHANGE_ME` placeholders):

```sh
kubectl create namespace flow
kubectl -n flow create secret generic flow-tokens \
  --from-literal=owner="$(openssl rand -hex 32)" \
  --from-literal=hook="$(openssl rand -hex 32)" \
  --from-literal=worker-join="$(openssl rand -hex 32)" \
  --from-literal=orchestrator="$(openssl rand -hex 32)"
```

## Telemetry endpoints

Every flow binary serves an unauthenticated telemetry port with:

- `GET /readyz` — readiness probe (503 until ready)
- `GET /livez` — liveness probe (always 200 while the process is up)
- `GET /metrics` — Prometheus text exposition (0.0.4)

The default listen address is `127.0.0.1:8422`; the manifests set `:8422` via
`--metrics-listen` or config `metrics.listen`. Config `metrics.enabled: false`
or `--no-metrics` disables the endpoint. **The telemetry port carries no
authentication: keep it cluster-internal (probes and Prometheus scraping
only). Never expose it through an Ingress or LoadBalancer.**

Readiness semantics:

| Binary | Ready when |
| --- | --- |
| `flow-server` | the global SQLite database answers a ping |
| `flow-worker` | the worker has registered with the coordinator |
| `flow-orchestrator` | the first successful poll of both the coordinator and the Kubernetes API |

## Ephemeral workers

`flow-worker --ephemeral` keeps long-polling for a claim, runs exactly one
job, then exits 0. The kubelet restarts the container for the next
assignment; workers are never reused across jobs. A job-scoped failure is
reported to the coordinator (release state, check verdict) and does **not**
fail the process — the worker exits 0 either way.

The worker Deployment sets `FLOW_WORKER_ID` from the pod name, so each pod is
a distinct worker identity, and joins with the shared `worker-join` token from
`FLOW_WORKER_JOIN_TOKEN`.

## flow-orchestrator

The orchestrator polls `GET /v2/queue/stats` on the coordinator and scales the
`flow-worker` Deployment through the Kubernetes `apps/v1 deployments/scale`
subresource:

```
target = clamp(max(desired_replicas, queued), min_replicas, max_replicas)
```

It scales **up immediately** (queued work or a standby-pool deficit never
waits) and scales **down only after the queue has been empty for
`scaledown_idle`** (default 2m). It never scales while blind: if the
coordinator or the Kubernetes API is unreachable, the cycle is skipped and
`flow_orchestrator_poll_errors_total{source=...}` is counted.

`min_replicas` / `max_replicas` bound the pool; `desired_replicas` is the
standby pool — workers up and ready for a job assignment even when the queue
is empty. Configure via `examples/flow-orchestrator.yaml` or the
`FLOW_ORCHESTRATOR_*` environment variables.

### Orchestrator token scope

The orchestrator does **not** use the owner token. flow-server seeds a
dedicated `orchestrator`-scoped credential from `--orchestrator-token`,
`--orchestrator-token-file`, or `FLOW_ORCHESTRATOR_TOKEN` (no token is
generated when unset). That credential authorizes only `GET /v2/queue/stats`;
the owner token retains access for debugging and CLI use. The orchestrator
pod therefore holds no credential that can mutate tasks, flows, or workers.

RBAC is equally narrow: the `flow-orchestrator` ServiceAccount may only
`get`/`update`/`patch` `deployments/scale` in the flow namespace.

## Images

```sh
docker build --target flow-server -t ghcr.io/clarifiedlabs/flow-server .
docker build --target flow-worker -t ghcr.io/clarifiedlabs/flow-worker .
docker build --target flow-orchestrator -t ghcr.io/clarifiedlabs/flow-orchestrator .
```

## Metrics

| Metric | Exported by | Meaning |
| --- | --- | --- |
| `flow_http_requests_total{route,status}` | flow-server | API requests |
| `flow_jobs_enqueued_total` | flow-server | jobs enqueued via `POST /v2/jobs` |
| `flow_jobs_completed_total` | flow-server | jobs released to `finished` |
| `flow_queue_depth{state}` | flow-server | jobs by state across all projects |
| `flow_jobs_claimed_total` | flow-worker | jobs claimed by this worker |
| `flow_jobs_completed_total{result}` | flow-worker | jobs released by this worker |
| `flow_queue_depth` | flow-orchestrator | queued jobs (the scaling signal) |
| `flow_worker_replicas_desired` | flow-orchestrator | worker Deployment spec replicas |
| `flow_orchestrator_scale_operations_total{direction}` | flow-orchestrator | scale up/down operations |
| `flow_orchestrator_poll_errors_total{source}` | flow-orchestrator | skipped cycles by failing source |
| `flow_{server,worker,orchestrator}_build_info{version}` | all | build information |
