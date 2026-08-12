package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// routeDocPath is the checked-in generated doc, relative to this package dir.
const routeDocPath = "../../docs/reference/http-api.md"

// TestRouteRegistryResponds walks every documented route and asserts the
// server routes it: a listed route that no longer responds (renamed or
// removed without updating documentedRoutes) fails here.
func TestRouteRegistryResponds(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	// Seed a task so the listed reads exercise a non-empty store.
	if _, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
		Task: coordinator.CreateTaskInput{Title: "Route seed task", CreatedBy: coordinator.ActorHuman},
	}); err != nil {
		t.Fatalf("create seed task: %v", err)
	}

	for _, route := range documentedRoutes {
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			path := strings.ReplaceAll(route.Path, "{id}", fixture.Project.ID)
			if strings.HasSuffix(path, "/search") {
				path += "?q=Route"
			}
			response := httptest.NewRecorder()
			fixture.Server.ServeHTTP(response, authorizedRequest(route.Method, path, nil))
			if response.Code == http.StatusNotFound {
				t.Fatalf("%s %s returned 404 (%s); update documentedRoutes in internal/api/routes.go",
					route.Method, route.Path, response.Body.String())
			}
			// GET routes are reads and must succeed; POST routes with an empty
			// body may 400 (invalid_json), which still proves the route exists.
			if route.Method == http.MethodGet && response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200: %s", route.Path, response.Code, response.Body.String())
			}
		})
	}
}

// TestRouteDocMatchesGenerated asserts the checked-in docs/reference/http-api.md
// matches the doc generated from documentedRoutes, so adding a route without
// documenting it fails the build. Regenerate with -update.
func TestRouteDocMatchesGenerated(t *testing.T) {
	t.Parallel()
	generated := renderRouteDoc(documentedRoutes)

	if *updateGoldens {
		if err := os.WriteFile(routeDocPath, []byte(generated), 0o644); err != nil {
			t.Fatalf("write route doc %s: %v", routeDocPath, err)
		}
	}

	checkedIn, err := os.ReadFile(routeDocPath)
	if err != nil {
		t.Fatalf("read route doc %s: %v (run with -update to create it)", routeDocPath, err)
	}
	if string(checkedIn) != generated {
		t.Errorf("%s is stale; regenerate with `go test ./internal/api -run Route -update`\n%s",
			routeDocPath, goldenDiff(string(checkedIn), generated))
	}
}

// renderRouteDoc renders the documented v2 route list as Markdown, grouped
// into unscoped and project-scoped routes.
func renderRouteDoc(routes []documentedRoute) string {
	var unscoped, projectScoped []documentedRoute
	for _, route := range routes {
		if strings.Contains(route.Path, "/projects/{id}") {
			projectScoped = append(projectScoped, route)
		} else {
			unscoped = append(unscoped, route)
		}
	}
	sortRoutes := func(routes []documentedRoute) {
		sort.Slice(routes, func(i, j int) bool {
			if routes[i].Path != routes[j].Path {
				return routes[i].Path < routes[j].Path
			}
			return routes[i].Method < routes[j].Method
		})
	}
	sortRoutes(unscoped)
	sortRoutes(projectScoped)

	var doc strings.Builder
	doc.WriteString(`# HTTP API reference (v2)

Generated from ` + "`documentedRoutes`" + ` in ` + "`internal/api/routes.go`" + `; do not edit by hand.
Regenerate with:

` + "```sh" + `
go test ./internal/api -run 'Contract|Route' -update
` + "```" + `

Every route requires the protocol header ` + "`X-Flow-Protocol: 8`" + `
(` + "`contract.ProtocolVersion`" + `) and, except ` + "`GET /v2/health`" + `, a bearer token.
The SSE stream endpoints ` + "`GET /v2/events/stream`" + ` and
` + "`GET /v2/projects/{id}/events/stream`" + ` are intentionally excluded from this list:
they stream until the request context is canceled. ` + "`{id}`" + ` is the project id
(for example ` + "`p-my-project`" + `).
`)

	writeTable := func(title string, routes []documentedRoute) {
		fmt.Fprintf(&doc, "\n## %s\n\n| Method | Path |\n| --- | --- |\n", title)
		for _, route := range routes {
			fmt.Fprintf(&doc, "| %s | `%s` |\n", route.Method, route.Path)
		}
	}
	writeTable("Unscoped routes", unscoped)
	writeTable("Project-scoped routes", projectScoped)

	return doc.String()
}
