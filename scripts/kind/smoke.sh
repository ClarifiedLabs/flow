#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=common.sh disable=SC1091
source "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/common.sh"

SMOKE_TIMEOUT="${FLOW_KIND_SMOKE_TIMEOUT:-180}"
SMOKE_ROOT="${STATE_DIR}/smoke"
SMOKE_REPO="${SMOKE_ROOT}/repo"
SMOKE_API="${BIN_DIR}/kind-smoke-api"
FLOW_BIN="${BIN_DIR}/flow"
CLIENT_CONFIG="${STATE_DIR}/client.yaml"
PROJECT_ID=p-flow-kind-smoke
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$"

require_up_prerequisites
require_command git
ensure_state_dirs
cluster_exists || fatal "kind cluster '${CLUSTER_NAME}' does not exist; run scripts/kind/up.sh first"
kube cluster-info >/dev/null || fatal "kubectl cannot reach context '${KUBE_CONTEXT}'"
[ -x "${FLOW_BIN}" ] || fatal "local CLI ${FLOW_BIN} is missing; run scripts/kind/up.sh first"
[ -r "${CLIENT_CONFIG}" ] || fatal "local client config ${CLIENT_CONFIG} is missing; run scripts/kind/up.sh first"
[ -s "${TOKEN_DIR}/owner" ] || fatal "owner token is missing; run scripts/kind/up.sh first"

kube -n flow wait --for=condition=available deployment/flow-server deployment/flow-orchestrator --timeout="${SMOKE_TIMEOUT}s"
"${FLOW_BIN}" jobs --config "${CLIENT_CONFIG}" >/dev/null || fatal "host API 127.0.0.1:${API_HOST_PORT} is unavailable"
wait_for_worker_resource_count 1
log "idle invariant: one verified linux-agent capacity slot"

mkdir -p "${SMOKE_ROOT}" "${SMOKE_REPO}"
chmod 700 "${SMOKE_ROOT}" "${SMOKE_REPO}"

cat >"${STATE_DIR}/kind-smoke-api.go" <<'GOEOF'
package main

import (
	"fmt"
	"os"
	"strings"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 5 {
		fail("usage: kind-smoke-api SERVER TOKEN PROJECT enqueue|state ARGS...")
	}
	client, err := flowclient.New(config.ClientConfig{ServerURL: os.Args[1], Token: os.Args[2]})
	if err != nil { fail("create client: %v", err) }
	client = client.WithProject(os.Args[3])
	switch os.Args[4] {
	case "enqueue":
		if len(os.Args) != 7 { fail("enqueue requires TASK_ID|- and success|startup|cancel") }
		taskID := strings.TrimSpace(os.Args[5])
		var task *string
		if taskID != "" && taskID != "-" { task = &taskID }
		payload := map[string]any{
			"base": "main", "branch": "main",
			"entrypoint": map[string]any{"argv": []string{"/bin/sh", "-c", "printf 'flow-kind smoke success\\n'"}, "shell": false},
		}
		switch os.Args[6] {
		case "success":
		case "startup":
			payload["base"] = "flow-kind-missing-base"
			payload["branch"] = "flow-kind-missing-base"
		case "cancel":
			payload["entrypoint"] = map[string]any{"argv": []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 1; done"}, "shell": false}
		default:
			fail("unknown enqueue mode %q", os.Args[6])
		}
		job, err := client.EnqueueJob(flowclient.EnqueueJobInput{
			TaskID: task, Role: flowworker.RoleCI, CapacityBucket: flowworker.BucketEphemeral,
			Priority: 100, Payload: payload,
		})
		if err != nil { fail("enqueue job: %v", err) }
		fmt.Println(job.ID)
	case "state":
		if len(os.Args) != 6 { fail("state requires JOB_ID") }
		jobs, err := client.ListJobs()
		if err != nil { fail("list jobs: %v", err) }
		for _, job := range jobs {
			if job.ID == os.Args[5] { fmt.Println(job.State); return }
		}
		fail("job %s not found", os.Args[5])
	default:
		fail("unknown command %q", os.Args[4])
	}
}
GOEOF
chmod 600 "${STATE_DIR}/kind-smoke-api.go"
go build -o "${SMOKE_API}" "${STATE_DIR}/kind-smoke-api.go"
chmod 700 "${SMOKE_API}"

if [ ! -d "${SMOKE_REPO}/.git" ]; then
  git -C "${SMOKE_REPO}" init >/dev/null
  git -C "${SMOKE_REPO}" symbolic-ref HEAD refs/heads/main
  git -C "${SMOKE_REPO}" config user.name 'Flow kind smoke'
  git -C "${SMOKE_REPO}" config user.email 'flow-kind-smoke@example.invalid'
  printf 'flow kind smoke fixture\n' >"${SMOKE_REPO}/README.md"
  git -C "${SMOKE_REPO}" add README.md
  git -C "${SMOKE_REPO}" commit -m 'chore: seed kind smoke repository' >/dev/null
fi
# The smoke repository is generated state and can outlive a kind cluster. Remove
# the old exchange remote so flow init can install the fresh cluster's advertised
# Git URL when the host port changes between runs.
if git -C "${SMOKE_REPO}" remote get-url flow >/dev/null 2>&1; then
  git -C "${SMOKE_REPO}" remote remove flow
fi
# Never invoke a workstation credential helper (for example macOS Keychain)
# from this unattended smoke path; the generated client config already supplies
# the owner token and flow init can safely skip optional credential persistence.
git -C "${SMOKE_REPO}" config credential.helper ''

mkdir -p "${STATE_DIR}/home" "${STATE_DIR}/xdg/config" "${STATE_DIR}/xdg/data"
# Preserve kubectl's host configuration before isolating Flow's HOME/XDG state.
# Otherwise every post-init kube call silently loses the kind context.
export KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"
export HOME="${STATE_DIR}/home"
export XDG_CONFIG_HOME="${STATE_DIR}/xdg/config"
export XDG_DATA_HOME="${STATE_DIR}/xdg/data"
"${FLOW_BIN}" init --config "${CLIENT_CONFIG}" --repo "${SMOKE_REPO}" --name 'Flow kind smoke' --base main >/dev/null

OWNER_TOKEN="$(tr -d '[:space:]' <"${TOKEN_DIR}/owner")"
api() {
  "${SMOKE_API}" "http://127.0.0.1:${API_HOST_PORT}" "${OWNER_TOKEN}" "${PROJECT_ID}" "$@"
}

wait_for_one_worker_job() {
  local deadline output count
  deadline=$(( $(date +%s) + SMOKE_TIMEOUT ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    output="$(kube -n flow get jobs -l flow.clarifiedlabs.com/profile-name=linux-ci -o name 2>/dev/null || true)"
    count="$(printf '%s\n' "${output}" | awk 'NF { n++ } END { print n+0 }')"
    if [ "${count}" -gt 1 ]; then
      fatal "expected exactly one assignment Job, observed ${count}"
    fi
    if [ "${count}" -eq 1 ]; then
      printf '%s\n' "${output}"
      return 0
    fi
    sleep 1
  done
  fatal "timed out waiting for exactly one assignment Job"
}

wait_for_flow_state() {
  local job_id expected deadline state
  job_id="$1"
  expected="$2"
  deadline=$(( $(date +%s) + SMOKE_TIMEOUT ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    state="$(api state "${job_id}" 2>/dev/null || true)"
    if [ "${state}" = "${expected}" ]; then
      return 0
    fi
    case "${state}" in
      finished|failed|crashed|canceled)
        fatal "Flow job ${job_id} reached ${state}, expected ${expected}"
        ;;
    esac
    sleep 1
  done
  fatal "timed out waiting for Flow job ${job_id} to reach ${expected}"
}

wait_for_cleanup() {
  local deadline jobs pods
  deadline=$(( $(date +%s) + SMOKE_TIMEOUT ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
	jobs="$(kube -n flow get jobs -l flow.clarifiedlabs.com/profile-name=linux-ci -o name 2>/dev/null | awk 'NF { n++ } END { print n+0 }')"
	pods="$(kube -n flow get pods -l flow.clarifiedlabs.com/profile-name=linux-ci -o name 2>/dev/null | awk 'NF { n++ } END { print n+0 }')"
    if [ "${jobs}" -eq 0 ] && [ "${pods}" -eq 0 ]; then
      return 0
    fi
    sleep 1
  done
  fatal "timed out waiting for assignment cleanup (Jobs=$(worker_resource_count jobs), Pods=$(worker_resource_count pods))"
}

log "success scenario: enqueueing one deterministic ephemeral CI payload"
success_id="$(api enqueue - success)"
success_k8s_job="$(wait_for_one_worker_job)"
kube -n flow wait --for=condition=complete "${success_k8s_job}" --timeout="${SMOKE_TIMEOUT}s"
wait_for_flow_state "${success_id}" finished
wait_for_cleanup
log "success scenario: exactly one Kubernetes Job completed and cleaned up"

log "startup-failure scenario: a missing base must fail the Flow job, not crash it"
startup_id="$(api enqueue - startup)"
startup_k8s_job="$(wait_for_one_worker_job)"
kube -n flow wait --for=condition=complete "${startup_k8s_job}" --timeout="${SMOKE_TIMEOUT}s"
wait_for_flow_state "${startup_id}" failed
wait_for_cleanup
log "startup-failure scenario: Flow state is failed (not crashed), worker Job exited successfully and cleaned up"

log "cancellation scenario: task cancellation must close and clean its assignment"
task_line="$("${FLOW_BIN}" task create --config "${CLIENT_CONFIG}" --project "${PROJECT_ID}" --title "kind smoke cancellation ${RUN_ID}")"
task_id="$(printf '%s\n' "${task_line}" | awk 'NR == 1 { print $1 }')"
[ -n "${task_id}" ] || fatal "could not parse task id from flow task create"
cancel_id="$(api enqueue "${task_id}" cancel)"
wait_for_one_worker_job >/dev/null
wait_for_flow_state "${cancel_id}" running
"${FLOW_BIN}" task "done" --config "${CLIENT_CONFIG}" --project "${PROJECT_ID}" --resolution cancelled --note 'kind smoke cancellation' "${task_id}" >/dev/null
wait_for_flow_state "${cancel_id}" canceled
wait_for_cleanup
assert_worker_resource_count 1
kube -n flow wait --for=condition=available deployment/flow-server deployment/flow-orchestrator --timeout="${SMOKE_TIMEOUT}s"
log "PASS: idle capacity, one-shot success, cleanup, startup failure, and cancellation invariants hold"
