# Kubernetes deployment

Flow's Kubernetes control plane runs `flow-server` (the coordinator) and
`flow-orchestrator` as Deployments. Workers are not a Deployment or a warm
replica pool: the orchestrator creates one `batch/v1` Job and one private Secret
for each durable coordinator assignment. The reference manifests live in
[`k8s/`](../k8s).

## Local Kind quickstart

For development and basic end-to-end testing from a source checkout, the Kind
helpers build the local Flow images and run the complete assignment-based stack
in a local cluster. Install `kind`, `kubectl`, Go, and a running Docker-compatible
runtime (`docker`, `podman`, or `nerdctl`), then run from the repository root:

```sh
export HARNESS_MODEL_PROXY_URL=http://your-local-model-proxy:8080
export HARNESS_MODEL_PROXY_API_KEY="$(cat /path/to/local/model-proxy-api-key)"
./scripts/kind/up.sh
```

The proxy URL must be reachable from Kind worker Pods. `up.sh` creates or updates
`flow-harness-model-proxy` in the `flow` namespace from those two environment
variables. It retains them only in the private, gitignored `.flow-kind/tokens`
directory and in the cluster Secret; it does not write either value to a tracked
manifest or generated Flow configuration. This is profile-wide: every
assignment-created worker Job for the configured profile receives the two
variables, and the proxy must enforce its local-network security model.

The script creates or reuses the `flow` Kind cluster, loads locally built
`flow-server`, `flow-worker`, and `flow-orchestrator` images, and deploys the
server and orchestrator. It writes generated configuration, private tokens, and
persistent data under the gitignored `.flow-kind/` directory. The Flow API is
mapped only to `http://127.0.0.1:8421`; unauthenticated telemetry remains
cluster-internal.

Use the generated owner client to inspect the cluster:

```sh
./.flow-kind/bin/flow --config .flow-kind/client.yaml jobs
kubectl --context kind-flow -n flow get deployments,jobs,pods
```

With Git installed, run the smoke test to exercise successful execution, startup
failure, cancellation, and cleanup of assignment-created worker Jobs and Pods:

```sh
./scripts/kind/smoke.sh
```

The default cluster name, host API port, and container runtime can be overridden.
Use the same overrides for `up.sh`, `smoke.sh`, and `down.sh`:

```sh
FLOW_KIND_CLUSTER=flow-test \
FLOW_KIND_API_HOST_PORT=9421 \
FLOW_KIND_RUNTIME=podman \
./scripts/kind/up.sh
```

Delete the cluster when finished. Persistent coordinator data is retained by
default so a later cluster can reuse it; pass `--delete-data` to remove that data
too:

```sh
./scripts/kind/down.sh
./scripts/kind/down.sh --delete-data
```

This workflow is intended for local testing. For a real cluster, provision
credentials out of band and use the reference manifests described below.

## Reference manifests

The normal manifest path deliberately contains no credential-bearing `Secret`.
Create the namespace and tokens out of band, retain the generated values in your
secret manager, and then apply the control plane. The referenced
`flow-harness-model-proxy` Secret must contain `HARNESS_MODEL_PROXY_URL` and
`HARNESS_MODEL_PROXY_API_KEY` keys; provision it from your secret manager rather
than adding its data to these manifests:

```sh
kubectl create namespace flow --dry-run=client -o yaml | kubectl apply -f -

export FLOW_OWNER_TOKEN="${FLOW_OWNER_TOKEN:-$(openssl rand -hex 32)}"
export FLOW_HOOK_TOKEN="${FLOW_HOOK_TOKEN:-$(openssl rand -hex 32)}"
export FLOW_ORCHESTRATOR_TOKEN="${FLOW_ORCHESTRATOR_TOKEN:-$(openssl rand -hex 32)}"

kubectl -n flow create secret generic flow-tokens \
  --from-literal=owner="$FLOW_OWNER_TOKEN" \
  --from-literal=hook="$FLOW_HOOK_TOKEN" \
  --from-literal=orchestrator="$FLOW_ORCHESTRATOR_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f k8s/server.yaml
kubectl apply -f k8s/orchestrator.yaml
```

The orchestrator credential has provisioner scope and is bound by
`--orchestrator-provider-ids`. Each reservation returns a separate short-lived
worker credential scoped to its assignment.

## Durable assignment model

Assignments are coordinator-owned records stored in the selected job's
project-local SQLite database. Reservation atomically binds one exact queued job
to a stable assignment, worker identity, provider/profile identity, scheduling
snapshot, and startup deadline. The assignment remains the source of truth if
the orchestrator or Kubernetes API restarts.

Each reconciliation cycle is recovery-first:

1. List durable assignments across projects.
2. Inspect resources for open assignments and delete resources for closed,
   not-yet-cleaned assignments.
3. Recover missing pending resources only when their persisted provider and
   scheduling descriptor still matches a locally configured profile, and subject
   to their durable retry time and startup deadline. A missing resource for a
   removed or changed profile is abandoned rather than launched from untrusted
   persisted settings. A transient launch failure is retried with bounded
   backoff; a permanent failure or expired startup deadline abandons the pending
   assignment.
4. Only after recovery, reserve additional eligible jobs up to each profile's
   `max_concurrency` and launch their resources.

A worker that has claimed its assignment is governed by the ordinary lease and
job recovery lifecycle; provider failure does not itself put claimed work back
on the queue. On terminal assignment cleanup, the coordinator first revokes the worker's
credentials, then the orchestrator deletes the provider resource before the
coordinator removes its worker-directory row and records the assignment as
cleaned. These
steps are reconciled rather than assumed to be atomic, so retain the coordinator
SQLite databases during operational recovery.

## Orchestrator profiles

Provider profiles are configured only in the orchestrator YAML/JSON file; scalar
connection and retry settings may also use the documented
`FLOW_ORCHESTRATOR_*` environment variables. A Kubernetes profile resembles:

```yaml
coordinator_url: http://flow-server:8421
poll_interval: 5s
retry_base: 1s
retry_max: 1m

profiles:
  - name: linux
    provider: kubernetes
    provider_id: in-cluster
    max_concurrency: 10
    startup_timeout: 2m
    allowed_roles: [author, reviewer, verifier, ci, console]
    accepts: [persistent_agent, ephemeral]
    labels:
      os: linux
    kubernetes:
      namespace: flow
      image: ghcr.io/clarifiedlabs/flow-worker:latest
      service_account: flow-worker
      work_dir: /var/lib/flow-worker
      image_pull_policy: IfNotPresent

metrics:
  listen: :8422
```

`max_concurrency` bounds open assignments for that provider/profile; it is not a
standby replica count. Profiles select candidates by role, workload bucket,
labels, taints, harness models, and `required_selector`. An omitted `accepts`
defaults to the `ephemeral` workload bucket. The bucket names retain their job
semantics: `persistent_agent` is used by author/reviewer/verifier/console work,
while `ephemeral` is used by CI/check work. They do not imply process reuse.

For each reservation, the coordinator returns a direct worker credential scoped
to the assignment's stable worker ID. The orchestrator writes that token and the
private assignment configuration to a mode-0400 `worker.yaml` in the assignment
Secret. The Job mounts the Secret read-only and runs:

```text
flow-worker run --one-shot --config /var/run/flow/worker.yaml
```

The Job uses `restartPolicy: Never` and `backoffLimit: 0`; retries are owned by
the durable orchestrator reconciliation, not by kubelet/container restarts. Job
and Secret names are deterministic from assignment identity, making launch and
cleanup safe to retry. Keep the generated Secret private: it contains the direct
worker bearer token.

The orchestrator credential is distinct from the owner token. Its scope is
`provisioner`: it can reserve/list assignments and record launch, abandon, and
cleanup transitions for bound provider IDs, but has no general task, flow, or
owner authority. Kubernetes RBAC should likewise be namespace-scoped to the Job and
Secret operations required by the provider. The worker Job's ServiceAccount is
separate and does not need permission to create workers or read other assignment
Secrets.

## One-shot worker lifecycle

`flow-worker run --one-shot --config PATH` registers with its direct credential,
long-polls until it exact-claims the job bound to its worker ID, runs that one
job, reports its job-scoped outcome, and exits. A reported job failure is not a
worker process failure, so the process exits zero after that report. `SIGINT` and
`SIGTERM` cancel registration retries, long polling, maintenance, job
supervision, and telemetry through the worker's shared context; signal
cancellation also exits zero. If interruption prevents a terminal report, lease
expiry and coordinator recovery remain authoritative.

Every worker identity belongs to one assignment and can claim only its bound
job. Concurrency comes from independent assignments and Jobs, bounded by the
orchestrator profile's `max_concurrency`.

## Telemetry endpoints

Every Flow binary serves an unauthenticated telemetry port with:

- `GET /readyz` — readiness probe (503 until ready)
- `GET /livez` — liveness probe (always 200 while the process is up)
- `GET /metrics` — Prometheus text exposition (0.0.4)

The default listen address is `127.0.0.1:8422`; Kubernetes control-plane
manifests set `:8422` via `--metrics-listen` or config `metrics.listen`. Config
`metrics.enabled: false` or `--no-metrics` disables the endpoint. **The telemetry
port carries no authentication: keep it cluster-internal (probes and Prometheus
scraping only). Never expose it through an Ingress or LoadBalancer.** Apply the
same rule if worker Job telemetry is enabled on a pod-reachable address.

Readiness semantics:

| Binary | Ready when |
| --- | --- |
| `flow-server` | the global SQLite database answers a ping |
| `flow-worker` | the worker has registered and claims are not paused by disk pressure |
| `flow-orchestrator` | one complete recovery-and-reservation cycle has succeeded |

Assignment-centric orchestrator metrics are:

| Metric | Meaning |
| --- | --- |
| `flow_orchestrator_active_assignments{provider,profile,state}` | open durable assignments |
| `flow_orchestrator_reserve_errors_total{provider,profile}` | coordinator reservation errors |
| `flow_orchestrator_launch_errors_total{provider,profile}` | provider launch errors |
| `flow_orchestrator_cleanup_errors_total{provider,profile}` | provider deletion/cleanup errors |
| `flow_orchestrator_reconcile_errors_total{operation,provider,profile}` | reconciliation errors by operation |

Coordinator `flow_queue_depth{state}` remains useful queue telemetry, but it is
not a worker provisioning signal.

## History durability for assignment workers

`flow-server` stores its SQLite databases and the default local history blob
backend beneath `/var/lib/flow`, which the reference Deployment mounts from the
`flow-server-data` PVC. Preserve the database and blobs together; choosing S3 for
history blobs does not remove the database durability requirement.

Mandatory history capture also has a worker-side durability requirement. An
assignment worker keeps active job sources beneath its configured `work_dir` and
keeps the replayable history outbox outside `work_dir/jobs` (by default at
`<work_dir>/history-outbox`). Both must survive a worker process or Pod restart
until the coordinator acknowledges final publication. The outbox alone cannot
reconstruct a workspace or Harness source directory that has disappeared.

The current Kubernetes provider profile sets `work_dir: /var/lib/flow-worker`,
but generated one-shot Jobs do not mount a PVC or another durable volume and the
profile schema has no volume-mount option. Their writable Pod filesystem therefore
does **not** preserve pending history across Pod deletion/replacement. S3 protects
only bytes already uploaded to the coordinator. Do not claim cross-Pod full-fidelity
history durability from the reference manifests until provider-managed durable
worker storage is implemented; avoid deleting a worker Pod with a pending capture
and treat such loss as evidence loss requiring recovery or an explicit owner
waiver.

See [Full-fidelity execution history](history.md) for capture boundaries, outbox
sizing, recovery, authorization, and waiver consequences.

## Operational recovery and cleanup

- Keep every retired `provider_id` in the coordinator's
  `--orchestrator-provider-ids` / `FLOW_ORCHESTRATOR_PROVIDER_IDS` authorization
  until all of that provider's assignments are cleaned. It does not need an
  active orchestrator profile and therefore cannot reserve new work; the retained
  token binding is an explicit recovery tombstone that lets the orchestrator list,
  fence, and clean durable assignments for the removed ID. Removing the ID from
  the token binding revokes that recovery access.
- Assignment rows retain their immutable provider type and options, so removed or
  changed profile names, Kubernetes namespaces, and Darwin state paths do not
  prevent inspection and cleanup of existing work. Current profile settings
  authorize launches and apply to new reservations only.
- Check `/readyz`, assignment metrics, and orchestrator logs after restart. The
  process does not become ready until a full recovery-first cycle succeeds.
- Do not manually delete a pending Job while leaving its assignment open merely
  to retry it. Reconciliation detects a missing resource and relaunches it when
  its retry time permits.
- A closed assignment with no `cleaned_at` still needs provider cleanup. Leave
  the orchestrator running until its Job and Secret are gone and cleanup is
  recorded. If Kubernetes deletion fails, it remains retryable.
- A claimed assignment follows lease expiry/recovery even when its Job has
  disappeared. Do not manually requeue it based only on provider state.
- The Kubernetes provider uses background propagation for Job deletion and then
  deletes the Secret. Verify both resource types when investigating leaked
  resources.

## Darwin provider

For local macOS operation, a `provider: darwin` profile creates one
`flow-worker run --one-shot --config PATH` child process per assignment instead
of Kubernetes resources. The provider stores a private worker config, PID,
status, and log in a
stable assignment directory beneath `darwin.state_dir`; its work directory is
assignment-specific. On cleanup it terminates the process group when necessary.
Successful assignment directories are removed; failed logs and work products are
retained for diagnosis after the private worker config and process-identity files
are deleted. Keep `state_dir` on durable local storage and mode 0700 so restart
inspection and credential cleanup remain possible. The provider rejects symlinked,
foreign-owned, or group/world-accessible state roots and assignment directories.

The Darwin provider is a same-account local execution mechanism, not a security
boundary between mutually untrusted jobs: workers and the orchestrator run under
the same macOS user and can access that user's resources. Use the Kubernetes
provider (with the cluster's workload-isolation controls) when jobs must not trust
other concurrent jobs or the orchestrator account.

## S3 history-storage permissions

When `flow-server` uses the S3 history backend, its workload identity needs the
usual object read/write permissions plus the version-aware cleanup permissions.
Grant the bucket-level actions `s3:ListBucket`, `s3:ListBucketVersions`, and
`s3:ListBucketMultipartUploads`, and grant `s3:GetObject`, `s3:PutObject`,
`s3:DeleteObject`, `s3:DeleteObjectVersion`, and `s3:AbortMultipartUpload` on the
configured history prefix. Scope both the bucket condition and object resource
to that prefix where the IAM provider supports it.

`ListObjectVersions` and `DeleteObjectVersion` are required even when bucket
versioning is currently disabled. Reconciliation deletes exact stale temporary
versions and delete markers; it does not fall back to adding delete markers,
because doing so would retain hidden temporary payloads indefinitely. With
SSE-KMS, also grant the selected key's encrypt/decrypt/data-key permissions.

## Images

```sh
docker build --target flow-server -t ghcr.io/clarifiedlabs/flow-server .
docker build --target flow-worker -t ghcr.io/clarifiedlabs/flow-worker .
docker build --target flow-orchestrator -t ghcr.io/clarifiedlabs/flow-orchestrator .
```
