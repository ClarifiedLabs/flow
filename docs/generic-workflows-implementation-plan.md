# Generic Workflows Implementation Plan

## Outcome

Flow has one small issue lifecycle and a separate, user-configurable execution graph:

1. **Unscheduled** — represented by a null persisted lifecycle state.
2. **Scheduled** — queued for work; unresolved issue dependencies prevent execution without removing it from the queue.
3. **In Progress** — a workflow run has started.
   - **Working** is derived when automation can act.
   - **Blocked** is derived when an open workflow wait requires human action.
4. **Done** — terminal, with one of `completed`, `merged`, `rejected`, `abandoned`, `cancelled`, or `failed`.

Triage and backlog are not lifecycle states. New and materialized issues are Unscheduled until explicitly scheduled.

This is a breaking replacement. Project databases use storage format 2; databases from the old model are rejected and must be recreated. The API and worker protocol are version 2. There are no migration or compatibility paths.

## Separation of concerns

The lifecycle answers only where an issue is from the user's perspective. A frozen workflow run answers what is happening inside In Progress.

```text
null / Unscheduled -> Scheduled -> In Progress -> Done
                           |           |
                    dependency gate   +-- Working
                                       +-- Blocked (open wait)
```

Scheduling snapshots the selected flow, including resolved agent definitions, so edits to a flow affect future runs only. A run has exactly one active node visit. Graphs may branch and cycle, but they do not execute parallel nodes.

## Workflow graph contract

A flow contains:

- a stable start-node key;
- a configurable transition budget (default 50, maximum 500);
- up to 50 nodes and 200 directed edges;
- exactly one edge for every declared non-terminal outcome;
- only nodes reachable from the start;
- a path from every reachable node to a terminal node; and
- artifact-compatible edges.

Cycles and branches are valid. The transition budget stops a runaway cycle in Blocked until an owner extends it.

Only trusted node kinds can be configured:

| Node kind | Purpose | Outcomes |
| --- | --- | --- |
| `agent` | Run a frozen agent definition in a base or change workspace and produce a typed artifact | `completed` |
| `automated_checks` | Run repository-defined CI checks | `passed`, `failed` |
| `change_review` | Run configured review agents | `approved`, `changes_requested` |
| `human_gate` | Wait for an owner response | configured outcome set |
| `verify_change` | Verify requirements and review-thread claims | `passed`, `changes_requested` |
| `materialize_issue_set` | Transactionally create child issues and dependencies | `completed` |
| `merge_change` | Squash-merge the run-owned change | `merged`, `conflict` |
| `terminal` | Finish the issue with a fixed Done resolution | none |

Node configuration is a strict discriminated union. Arbitrary executable node kinds, shell snippets in graph configuration, and client-selected handlers are not allowed.

## Typed artifacts

Agent nodes declare one output kind:

- `handoff` — Markdown summary and optional structured payload;
- `change` — a run-owned change ID pinned to the submitted head SHA; or
- `issue_set` — a strict schema-version-1 issue manifest.

Artifacts are immutable, content-hashed, bound to the producing workflow/node/session, and idempotent by creator plus client key. Only the active agent node can create its declared artifact kind. Downstream nodes receive the current artifact; graph validation rejects incompatible paths before a flow can be saved.

Issue-set materialization is transactional and idempotent. It creates Unscheduled child issues, parent links, tags, and blocker relationships, and can select only valid implementation flows allowed by the materialization node.

## Execution and ownership

Every run owns its jobs, node visits, artifacts, waits, sessions, and changes. Coding runs use a branch such as `issue/i-1234/run-2`, preventing reopened or reset runs from sharing revision identity.

Agent jobs stay Scheduled while queued and move the issue to In Progress only when a worker starts the node. Base-workspace jobs cannot push. Change-workspace session credentials are restricted to the run branch. `flow complete` creates the typed artifact and completes the active agent node; change completion resolves and pushes the current branch and pins its head SHA.

Review, verification, and repository checks are separate trusted handlers. Each graph-node visit gets distinct check identities so cycles run checks again. Failed checks from earlier visits remain visible but are made non-required when a later visit begins.

## Human control and terminal behavior

A human gate opens a durable wait and derives In Progress / Blocked. The owner chooses one of the node's configured outcomes and may attach feedback, which is provided to the next agent prompt.

Owners may:

- reset an active run, cancelling its jobs, leases, sessions, and active node and returning the issue to Unscheduled while retaining history;
- extend an exhausted transition budget;
- force Done with any fixed resolution except `merged`; or
- reopen any Done issue, including a merged issue, as a new Unscheduled run.

Only a successful `merge_change` node can produce the `merged` resolution, and the terminal handler verifies that the run owns a merged change.

## Built-in flows

The **Coding** default is:

```text
implement -> automated checks -> change review -> human review -> verify -> merge -> merged
     ^             |                 |                |          |
     +-------------+-----------------+----------------+----------+ conflict/changes

human review --rejected-----------------------------------------> rejected
```

The **Planning** flow is:

```text
write issue set -> human review -> materialize issue set -> completed
       ^                |
       +-- changes -----+
                        +-- rejected -> rejected
```

## Persistence and API

The relational flow definition lives in `flows`, `flow_nodes`, and `flow_edges`. Runtime state lives in `workflow_runs`, `workflow_node_runs`, `workflow_artifacts`, `workflow_waits`, `workflow_materializations`, and the append-only `workflow_transitions` audit log.

The version-2 issue API exposes schedule, workflow detail, artifact creation, agent completion, human response, budget extension, reset, owner Done, and reopen operations. The CLI exposes the same lifecycle operations plus `flow complete` for active agent sessions.

## UI

The board has Unscheduled, Scheduled, In Progress, and Done lanes. In Progress cards show Working or Blocked. Issue detail shows the frozen run, current node, node-visit history, budget use, and human-gate outcomes.

The flow editor uses node cards, strict per-kind JSON configuration, transition selectors, start-node and budget controls, and a read-only graph preview. Server validation remains authoritative.

## Verification

Implementation verification covers:

- format-2 database creation and rejection of old project databases;
- graph normalization, reachability, terminal paths, cycle acceptance, outcome completeness, and artifact compatibility;
- scheduling with and without unresolved issue blockers;
- agent, check, review, verification, human-gate, materialization, merge, and terminal handlers;
- repeated node visits and transition-budget exhaustion;
- artifact ownership, idempotency, revision pinning, and issue-set materialization replay;
- reset, force-Done, and reopen history semantics;
- run-specific branch authorization; and
- version-2 API, CLI, worker, board, issue-detail, and flow-editor behavior.
