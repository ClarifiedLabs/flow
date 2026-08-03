# Kubernetes deployment

Flow's Kubernetes control plane runs `flow-server` (the coordinator) and
`flow-orchestrator` as Deployments. Workers are not a Deployment or a warm
replica pool: the orchestrator creates one `batch/v1` Job and one private Secret
for each durable coordinator assignment. The reference manifests live in
[`k8s/`](../k8s).

The normal manifest path deliberately contains no credential-bearing `Secret`.
Create the namespace and tokens out of band, retain the generated values in your
secret manager, and then apply the control plane:

```sh
kubectl create namespace flow --dry-run=client -o yaml | kubectl apply -f -

export FLOW_OWNER_TOKEN="${FLOW_OWNER_TOKEN:-$(openssl rand -hex 32)}"
export FLOW_HOOK_TOKEN="${FLOW_HOOK_TOKEN:-$(openssl rand -hex 32)}"
export FLOW_WORKER_JOIN_TOKEN="${FLOW_WORKER_JOIN_TOKEN:-$(openssl rand -hex 32)}"
export FLOW_ORCHESTRATOR_TOKEN="${FLOW_ORCHESTRATOR_TOKEN:-$(openssl rand -hex 32)}"

kubectl -n flow create secret generic flow-tokens \
  --from-literal=owner="$FLOW_OWNER_TOKEN" \
  --from-literal=hook="$FLOW_HOOK_TOKEN" \
  --from-literal=worker-join="$FLOW_WORKER_JOIN_TOKEN" \
  --from-literal=orchestrator="$FLOW_ORCHESTRATOR_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f k8s/server.yaml
kubectl apply -f k8s/orchestrator.yaml
```

The reusable worker-join credential is only for optional, separately managed
static workers. Assignment workers receive coordinator-issued,
assignment-specific worker credentials directly and never receive it.

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
standby replica count. Profiles select candidates by role, capacity bucket,
labels, taints, harness models, and `required_selector`. An omitted `accepts`
defaults to the `ephemeral` workload bucket. The bucket names retain their job
semantics: `persistent_agent` is used by author/reviewer/verifier/console work,
while `ephemeral` is used by CI/check work. They do not imply process reuse.

For each reservation, the coordinator returns a direct worker credential scoped
to the assignment's stable worker ID. The orchestrator writes that token and the
single assigned capacity bucket to a mode-0400 `worker.yaml` in the assignment
Secret. The Job mounts the Secret read-only and runs:

```text
flow-worker --config /var/run/flow/worker.yaml --one-shot
```

The Job uses `restartPolicy: Never` and `backoffLimit: 0`; retries are owned by
the durable orchestrator reconciliation, not by kubelet/container restarts. Job
and Secret names are deterministic from assignment identity, making launch and
cleanup safe to retry. Keep the generated Secret private: it contains the direct
worker bearer token.

The orchestrator credential is distinct from the owner token. It can use the
provisioner-assignment API to reserve/list assignments and record launch,
abandon, and cleanup transitions; it does not grant general task, flow, or owner
authority. Kubernetes RBAC should likewise be namespace-scoped to the Job and
Secret operations required by the provider. The worker Job's ServiceAccount is
separate and does not need permission to create workers or read other assignment
Secrets.

## One-shot worker lifecycle

`flow-worker --one-shot` registers with its direct credential, long-polls until
it claims the exact job bound to its worker ID, runs that one job, reports its
job-scoped outcome, and exits. A reported job failure is not a worker process
failure, so the process exits zero after that report. `SIGINT` and `SIGTERM`
cancel registration retries, long polling, maintenance, job supervision, and
telemetry through the worker's shared context; signal cancellation also exits
zero. If interruption prevents a terminal report, lease expiry and coordinator
recovery remain authoritative.

Worker configuration uses canonical `accepts` bucket names. For compatibility,
a legacy positive `capacity.persistent_agent` or `capacity.ephemeral` value is
normalized to acceptance value 1; magnitudes greater than one are ignored with a
warning. `accepts` and `capacity` cannot be configured together. Regardless of
how many buckets it accepts, one worker identity may hold only one live lease in
total. Concurrency therefore comes from independent assignments and Jobs, not
multiple claim loops in one worker process.

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

## Migration from a worker Deployment

1. Deploy the assignment-aware coordinator and configure the orchestrator token,
   bind it to the profile's provider ID, and add at least one Kubernetes profile
   matching the labels, taints, roles, harness models, selectors, and workload
   buckets your jobs require.
2. Grant the orchestrator namespace-scoped Job and Secret RBAC and verify its
   recovery cycle becomes ready.
3. Stop new claims from the old `flow-worker` Deployment, then allow its live
   leases to finish or recover through normal lease expiry. Do not delete active
   work directories as a substitute for lease recovery.
4. Scale the old worker Deployment to zero and remove its shared worker join
   secret only after no legacy worker still needs to register.
5. Confirm queued jobs receive durable assignments and exactly one Job plus one
   Secret each. Confirm closed assignments are eventually marked cleaned and
   leave no provider resources.
6. Remove obsolete Deployment scaling RBAC, replica bounds, standby-pool
   settings, and queue-stat polling configuration.

Do not run the old worker Deployment and an overlapping assignment profile as a
long-term mixed pool: both are eligible claimers, which makes migration capacity
and cleanup harder to reason about. A short drain window is sufficient.

## Darwin provider

For local macOS operation, a `provider: darwin` profile creates one
`flow-worker --one-shot` child process per assignment instead of Kubernetes
resources. The provider stores a private worker config, PID, status, and log in a
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
