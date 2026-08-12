package api

// documentedRoute is one documented v2 HTTP route: the machine-readable
// source of truth for docs/reference/http-api.md.
type documentedRoute struct {
	Method string
	Path   string
}

// documentedRoutes lists the stable, documented v2 HTTP surface: the
// project-scoped routes under /v2/projects/{id}/... plus the unscoped
// aggregates of the same reads.
//
// This list MUST stay in sync with the dispatchers in server.go (dispatch)
// and project_routing.go (handleProjectScopedPath). Adding, renaming, or
// removing a route there requires updating this list and regenerating
// docs/reference/http-api.md:
//
//	go test ./internal/api -run 'Contract|Route' -update
//
// TestRouteRegistryResponds fails when a listed route stops responding, and
// TestRouteDocMatchesGenerated fails when the checked-in doc drifts from this
// list. The SSE stream endpoints (/v2/events/stream and
// /v2/projects/{id}/events/stream) are intentionally excluded: they never
// return without a cancelable request context, so they cannot be exercised by
// the route walk.
var documentedRoutes = []documentedRoute{
	{Method: "GET", Path: "/v2/health"},

	{Method: "GET", Path: "/v2/projects"},
	{Method: "POST", Path: "/v2/projects"},
	{Method: "GET", Path: "/v2/projects/{id}"},
	{Method: "GET", Path: "/v2/projects/{id}/tasks"},
	{Method: "POST", Path: "/v2/projects/{id}/tasks"},
	{Method: "GET", Path: "/v2/projects/{id}/board"},
	{Method: "GET", Path: "/v2/projects/{id}/completions"},
	{Method: "GET", Path: "/v2/projects/{id}/search"},
	{Method: "GET", Path: "/v2/projects/{id}/events"},

	{Method: "GET", Path: "/v2/tasks"},
	{Method: "POST", Path: "/v2/tasks"},
	{Method: "GET", Path: "/v2/board"},
	{Method: "GET", Path: "/v2/completions"},
	{Method: "GET", Path: "/v2/search"},
	{Method: "GET", Path: "/v2/events"},
	{Method: "GET", Path: "/v2/done"},
	{Method: "GET", Path: "/v2/stats/completions"},
}
