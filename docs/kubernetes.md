# Kubernetes deployment

Flow's Kubernetes control plane runs `flow-server` (the coordinator) and
`flow-orchestrator` as Deployments. Workers are one-shot capacity slots, not a
reusable Deployment: a slot may be launched and verified before job binding,
but it binds at most once. The orchestrator creates one `batch/v1` Job and one
private Secret for each durable slot. The reference manifests live in
[`k8s/`](../k8s).

## Local Kind quickstart

For development and basic end-to-end testing from a source checkout, the Kind
helpers build the local Flow images and run the complete capacity-slot stack
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
capacity-slot Job for the configured profile receives the two
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
failure, cancellation, idle capacity, and cleanup of one-shot worker Jobs and Pods:

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
`--orchestrator-provider-ids`. Each capacity slot receives a separate direct
worker credential before job binding.

## Durable assignment model

Capacity slots and runtime worker capabilities are coordinator-global records.
Once a ready slot is selected, its project-local assignment becomes the
authoritative job binding. Binding commits the project assignment first; crash
recovery repairs the global slot by stable worker ID.

Each reconciliation cycle is recovery-first:

1. List durable capacity slots and assignments.
2. Inspect open slot resources and delete closed, not-yet-cleaned resources.
3. Recover missing resources only when their persisted provider and
   scheduling descriptor still matches a locally configured profile, and subject
   to their durable retry time and startup deadline. A missing resource for a
   removed or changed profile is abandoned rather than launched from untrusted
   persisted settings. A transient launch failure is retried with bounded
   backoff. An expired startup deadline closes the slot; a failed Harness/model
   promise opens the profile circuit and leaves one automatically retrying probe.
4. Bind verified ready slots, calculate demand, and provision the active target
   plus each profile's `idle_capacity`.

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
    idle_capacity: 2
    startup_timeout: 2m
    allowed_roles: [author, reviewer, verifier, ci, console]
    accepts: [persistent_agent, ephemeral]
    labels:
      os: linux
      agent.harness.harness: "true"
    kubernetes:
      namespace: flow
      image: ghcr.io/clarifiedlabs/flow-worker:latest
      service_account: flow-worker
      work_dir: /var/lib/flow-worker
      image_pull_policy: IfNotPresent
      work_volume:
        type: generic_ephemeral
        mount_path: /var/lib/flow-worker
        size: 20Gi
        access_modes: [ReadWriteOnce]
        # Omit to use the cluster default; otherwise use a class supplied by
        # your cluster rather than a cloud-provider-specific example name.
        # storage_class_name: your-cluster-storage-class
      resources:
        requests:
          cpu: 500m
          memory: 1Gi
        limits:
          memory: 4Gi
      node_selector:
        kubernetes.io/os: linux

metrics:
  listen: :8422
```

`max_concurrency` bounds active assignments. `idle_capacity` is additional
verified standby capacity, so the hard instance cap is their sum. The target is
`min(max_concurrency, active assignments + eligible queued jobs) + idle_capacity`.
Profiles select candidates by role, workload bucket,
labels, taints, harness models, and `required_selector`. An omitted `accepts`
defaults to the `ephemeral` workload bucket. The bucket names retain their job
semantics: `persistent_agent` is used by author/reviewer/verifier/console work,
while `ephemeral` is used by CI/check work. They do not imply process reuse.

For each slot, the coordinator returns a direct credential scoped to its stable
worker ID. Before binding, it can only register, heartbeat, claim-wait, and use
its control channel; project, git, history, terminal, and job data are denied.
The orchestrator writes that token and private slot configuration to a mode-0400
`worker.yaml` in the slot Secret. The Job mounts it read-only and runs:

```text
flow-worker run --one-shot --config /var/run/flow/worker.yaml
```

The Job uses `restartPolicy: Never` and `backoffLimit: 0`; retries are owned by
the durable orchestrator reconciliation, not by kubelet/container restarts. Job
and Secret names are deterministic from slot identity, making launch and
cleanup safe to retry. Keep the generated Secret private: it contains the direct
worker bearer token. When the profile sets `kubernetes.harness_config_file`, the
orchestrator also embeds that file as a `harness.json` key in the same Secret
and records `harness_config_file: /var/run/flow/harness.json` in `worker.yaml`;
the Secret's mode 0400 plus fsGroup 1000 keeps it private while readable by the
worker and its job shells.

Profiles are independent provider configurations and may use different images,
service accounts, pull policies, work volumes, resource requests/limits, node
selectors, and Harness model-proxy Secrets. Omitting `work_volume`, `resources`,
and `node_selector` preserves the prior Job shape: an unbounded `emptyDir` is
mounted at `work_dir`, and no container resources or node selector are emitted.

`kubernetes.harness_config_file` names an absolute path inside the orchestrator
pod (mount it via ConfigMap or Secret, like `flow-orchestrator.yaml` itself).
At each worker launch the orchestrator reads it — failing the launch
permanently, without logging its content, if it is missing, unreadable, or over
1 MiB — embeds it in the slot Secret, and the worker copies it into every job's
hermetic `HOME` as the Harness **global** config
(`$HOME/.config/harness/config.json`, mode 0600). It is a defaults layer, not
an override: a repository's `.harness/config.json` still wins per key, and
`harness_args`/flags and `HARNESS_*` environment variables win over the file.
Because it is per-job global config, runtime `harness config set` writes stay
job-scoped. The file must be a JSON object matching the Harness version baked
into the worker image (the `HARNESS_VERSION` build arg in the Dockerfile); flow
validates only that it parses as a JSON object. It may contain credentials, so
it travels only in the private per-worker Secret and each job's hermetic HOME
and is never logged. Only newly launched worker Jobs pick up changes. Omitting
the key preserves the exact prior Job/Secret/`worker.yaml` shape.
Resource names are limited to `cpu`, `memory`, and `ephemeral-storage`; when a
resource has both a request and a limit, the request must not exceed the limit.
Quantities use Kubernetes parsing; decimal exponent magnitude is capped at 1000
to bound configuration-validation cost.
An explicit `work_volume.type: empty_dir` accepts an optional `size_limit`;
Kubernetes uses it as `emptyDir.sizeLimit`, not as reserved capacity, and local
ephemeral storage pressure can still cause eviction. `generic_ephemeral` requires
`size`, defaults `access_modes` to `[ReadWriteOnce]`, and creates a PVC template
owned by the worker Pod. `mount_path` defaults to `work_dir`; when set above it,
`work_dir` must remain nested beneath it. Both paths must be absolute and clean,
must not be `/`, and must not overlap the read-only worker configuration at
`/var/run/flow`. Supported writable access modes are `ReadWriteOnce`,
`ReadWriteMany`, and `ReadWriteOncePod`; the selected CSI driver and StorageClass
must actually support the requested mode. `ReadWriteOncePod` must be the only
mode when selected.

For generic ephemeral storage, omit `storage_class_name` only when the cluster's
default StorageClass has the desired provisioning, topology, expansion, and
reclaim behavior. Otherwise set it to any dynamically provisioning StorageClass
installed and governed by your cluster operator; Flow deliberately does not
assume provider-specific class names. Admission policy, quotas, LimitRanges, scheduler capacity, and CSI
provisioning can still reject or delay a Job even after local validation. Configured
`agent.harness.*` labels are treated as requirements, not facts: the worker
removes them from static labels, probes Harness, and reports its live model
catalog. An explicit job model must appear by qualified ID in that catalog.
Reasoning level is never checked because Harness standardizes reasoning levels.

If a probe cannot satisfy its profile's Harness/model promise, bulk launches
for that profile pause and one probe retries with backoff. Orchestrator
`/readyz` returns 503 while that circuit is open; other profiles continue.

The orchestrator credential is distinct from the owner token. Its scope is
`provisioner`: it can reconcile slots and assignments for bound provider IDs,
but has no general task, flow, or
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

Every worker identity belongs to one slot. It may wait unbound, binds at most
once, and can claim only that assignment's job. Active work is bounded by
`max_concurrency`; `idle_capacity` is additional one-shot standby capacity.

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
| `flow-orchestrator` | one complete recovery, binding, and capacity cycle has succeeded |

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

Owner-guidance and review-scope workflow metrics are deliberately low
cardinality:

| Metric | Meaning |
| --- | --- |
| `flow_owner_rulings_total{source}` | durable rulings by owner, scope decision, or convergence return |
| `flow_owner_ruling_deliveries_total{result}` | live-session ruling delivery attempts and `succeeded`, `duplicate`, or `failed` outcomes |
| `flow_review_scope_decisions_opened_total` | durable scope-decision waits opened |
| `flow_review_scope_decisions_resolved_total{choice}` | owner resolutions by fixed choice |
| `flow_review_scope_decision_reruns_total{kind}` | aggregation-only or full-discovery reruns |
| `flow_review_scope_decision_rejections_total{reason}` | repeated-key or attempt-limit rejections |

## History durability for capacity workers

`flow-server` stores its SQLite databases and the default local history blob
backend beneath `/var/lib/flow`, which the reference Deployment mounts from the
`flow-server-data` PVC. Preserve the database and blobs together; choosing S3 for
history blobs does not remove the database durability requirement.

Mandatory history capture also has a worker-side durability requirement. An
capacity worker keeps active job sources beneath its configured `work_dir` and
keeps the replayable history outbox outside `work_dir/jobs` (by default at
`<work_dir>/history-outbox`). Both must survive a worker process or Pod restart
until the coordinator acknowledges final publication. The outbox alone cannot
reconstruct a workspace or Harness source directory that has disappeared.

The Kubernetes provider mounts `work_volume.mount_path` from either `empty_dir`
or a generic ephemeral PVC and writes the possibly nested `work_dir` into
`worker.yaml`. Mounting `/home/flow` with `work_dir: /home/flow/work` also places
project work, history outbox data, hermetic task homes and their Android/tool
caches, and the shipped worker image's rootless Docker data at
`/home/flow/.local/share/docker` on the same large volume.

Both volume modes follow the **Pod** lifecycle: they survive container restarts
in that Pod, but Kubernetes deletes their contents or generated PVC when the Pod
is deleted. A generic ephemeral PVC may offer different capacity, performance,
and topology than node-local `emptyDir`; it is not assignment storage durable
across Pod replacement. `emptyDir.sizeLimit` is also not a reservation and may
compete with other node ephemeral storage.

Consequently neither mode preserves pending history across Pod deletion or a
replacement Job. S3 protects only bytes already uploaded to the coordinator.
Avoid deleting a worker Pod with a pending capture and treat such loss as
evidence loss requiring recovery or an explicit owner waiver. A future durable
assignment-volume design would need independent PVC ownership, fencing,
reattachment, retention, and cleanup semantics; generic ephemeral volumes do not
provide those guarantees.

Flow deletes worker Jobs with foreground propagation, so the Job controller first
removes the Pod and Kubernetes' generic ephemeral volume controller then removes
the Pod-owned PVC. Flow needs no PVC RBAC for this process. A CSI, Pod, PV, or PVC
finalizer that remains stuck can delay Job deletion and storage reclamation even
though Flow has initiated cleanup; Flow only removes the assignment Secret after
foreground Job deletion completes.

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
`flow-worker run --one-shot --config PATH` child process per capacity slot instead
of Kubernetes resources. The provider stores a private worker config, PID,
status, and log in a
stable slot directory beneath `darwin.state_dir`; its work directory is
slot-specific. On cleanup it terminates the process group when necessary.
Successful slot directories are removed; failed logs and work products are
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
