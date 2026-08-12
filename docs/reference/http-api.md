# HTTP API reference (v2)

Generated from `documentedRoutes` in `internal/api/routes.go`; do not edit by hand.
Regenerate with:

```sh
go test ./internal/api -run 'Contract|Route' -update
```

Every route requires the protocol header `X-Flow-Protocol: 8`
(`contract.ProtocolVersion`) and, except `GET /v2/health`, a bearer token.
The SSE stream endpoints `GET /v2/events/stream` and
`GET /v2/projects/{id}/events/stream` are intentionally excluded from this list:
they stream until the request context is canceled. `{id}` is the project id
(for example `p-my-project`).

## Unscoped routes

| Method | Path |
| --- | --- |
| GET | `/v2/board` |
| GET | `/v2/completions` |
| GET | `/v2/done` |
| GET | `/v2/events` |
| GET | `/v2/health` |
| GET | `/v2/projects` |
| POST | `/v2/projects` |
| GET | `/v2/search` |
| GET | `/v2/stats/completions` |
| GET | `/v2/tasks` |
| POST | `/v2/tasks` |

## Project-scoped routes

| Method | Path |
| --- | --- |
| GET | `/v2/projects/{id}` |
| GET | `/v2/projects/{id}/board` |
| GET | `/v2/projects/{id}/completions` |
| GET | `/v2/projects/{id}/events` |
| GET | `/v2/projects/{id}/search` |
| GET | `/v2/projects/{id}/tasks` |
| POST | `/v2/projects/{id}/tasks` |
