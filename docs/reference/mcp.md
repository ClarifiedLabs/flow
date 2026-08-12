# Flow MCP server

`flow mcp serve` exposes flow's agent-facing read surface over the Model
Context Protocol (stdio JSON-RPC), so an MCP-capable agent harness can query
the local flow-server without shelling out to the CLI.

## Running

```sh
flow mcp serve [--project P]
```

The server resolves the client exactly like the rest of the CLI: the
discovered owner config plus the project (from `--project`, the worker's
`FLOW_PROJECT_ID`, or the repo registered for the current directory). Only MCP
protocol bytes go to stdout; logs go to stderr.

Client config snippet (e.g. a harness MCP entry):

```json
{
  "mcpServers": {
    "flow": {
      "command": "flow",
      "args": ["mcp", "serve"]
    }
  }
}
```

## Tool catalog (read-only allowlist)

Every tool's result is structured JSON including the bound `project_id`, plus
a text summary. List results are capped at 100.

| Tool | Args | Returns |
|------|------|---------|
| `flow.task_list` | `state?`, `tags?[]`, `ready?` | compact tasks (id, title, state) |
| `flow.task_show` | `id` | full task detail incl. done resolution/message/evidence |
| `flow.ready` | — | unblocked, unscheduled tasks, ordered |
| `flow.search` | `query`, `limit?` | ranked/substring task hits |
| `flow.events` | `since?`, `limit?`, `kind?`, `task?` | one event-log page + `next_since` cursor |

## Excluded by design

No write or admin tool is ever registered. An MCP client cannot create, done,
reset, reopen, retry, merge, or land tasks; cannot manage features, epics,
flows, agent defs, workers, or jobs; and cannot run anything owner-only beyond
the bound project. The allowlist above is the entire surface — there is no
flag to widen it. Agents that need to mutate use the `flow` CLI directly under
the operator's normal control.

## Notes

- `flow.events` pages the durable event log; resume with the returned
  `next_since` cursor. (Live SSE streaming stays on `flow events --follow`.)
- The server is long-lived per MCP client connection; coordinator restarts
  surface as tool errors, not server crashes.
