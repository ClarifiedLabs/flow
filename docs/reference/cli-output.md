# CLI machine output contract

This document is the stability contract for `flow`'s machine-readable output.
It is what agents and scripts may rely on. Human-readable output is free to
change; everything described here is governed by `agent_format` and changes
only through a deliberate version bump.

`internal/cliout` implements the contract; `ContractVersion` there is the
single source of truth for `agent_format`.

## Discovering the contract

```sh
flow --agent version
```

```json
{
  "contract_version": 1,
  "command": "version",
  "ok": true,
  "data": {
    "version": "0.5.7",
    "commit": "...",
    "date": "...",
    "agent_format": 1,
    "protocol": "8"
  }
}
```

- `agent_format` — the version of the envelope and per-command data shapes in
  this document. Agents should check this before parsing any other output.
- `protocol` — the HTTP API protocol version (`internal/api/contract`), sent
  as a request header and checked by the server. Independent of
  `agent_format`.

## Output modes

Every API-backed command accepts `--json` and `--agent`, either globally
(`flow --agent task list`) or on the command (`flow task list --agent`).
`--agent` wins when both are set. Both modes write machine output to
**stdout**; human diagnostics and progress stay on stderr, so an agent parses
exactly one stream.

- **human** (default) — free-form, may change without notice.
- **`--json`** — the command's primary result as one bare JSON object.
- **`--agent`** — the same result wrapped in the versioned envelope below,
  with structured errors.

## The envelope (`--agent`)

Success:

```json
{
  "contract_version": 1,
  "command": "task create",
  "ok": true,
  "data": { "task": { "id": "t-demo-0001" }, "reused": false }
}
```

Failure:

```json
{
  "contract_version": 1,
  "command": "task show",
  "ok": false,
  "error": { "code": "task_not_found", "message": "task t-demo-9999 not found" }
}
```

Fields:

- `contract_version` (int) — equals `agent_format`.
- `command` (string) — the command name, stable per command.
- `ok` (bool) — success indicator.
- `data` (object) — present on success; shape is per-command and additive-only
  within a contract version.
- `error` (object) — present on failure; `code` is a stable machine string,
  `message` is human context.

In `--json` mode the same information appears without the envelope: bare
`data` on success, `{"error": {"code", "message"}}` on failure.

## Error codes

`error.code` is stable within a contract version. API failures carry the
server-provided code (e.g. `task_not_found`, `idempotency_key_conflict`,
`protocol_mismatch`); other failures are classified generically as
`command_failed` or `usage_error`. Scripts should branch on `code`, never on
`message`.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | success |
| 1 | command error (see `error.code`) |
| 2 | usage error (bad flags/arguments) |
| 3 | domain-specific (e.g. `flow wait` timeout) |

## Idempotency on creates

Mutating create commands auto-generate an idempotency key when one is not
supplied, so a retried command cannot create duplicates. When the server
replays a stored response instead of executing a fresh create, the command's
data sets `reused: true` (backed by the `X-Flow-Idempotent-Replay` response
header). Agents can use `reused` to distinguish "I created this" from "this
already existed."

## Stability rules

Within a contract version:

- Existing fields are never removed, renamed, or retyped.
- New fields may be added; consumers must ignore fields they do not know.
- Enum-like fields (event kinds, states) may gain new values; consumers must
  tolerate unknown values.
- Field order in JSON is not significant and may change.

Breaking any of the above bumps `agent_format`.
