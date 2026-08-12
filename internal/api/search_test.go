package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestSearchEndpointFindsAndScopes(t *testing.T) {
	t.Parallel()
	fixture := newTestFixture(t)
	ctx := context.Background()

	mk := func(title, body string) {
		if _, err := fixture.Tasks.CreateTaskWithDetails(ctx, coordinator.CreateTaskWithDetailsInput{
			Task: coordinator.CreateTaskInput{Title: title, Body: body, CreatedBy: coordinator.ActorHuman},
		}); err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
	}
	mk("Fix login redirect loop", "users bounced between pages")
	mk("Improve dashboard", "slow metrics page")

	path := "/v2/projects/" + fixture.Project.ID + "/search"

	response := httptest.NewRecorder()
	fixture.Server.ServeHTTP(response, authorizedRequest(http.MethodGet, path+"?q=redirect", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body tasksResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(body.Tasks) != 1 || body.Tasks[0].Title != "Fix login redirect loop" {
		t.Fatalf("hits = %+v", body.Tasks)
	}

	// missing q is a 400
	bad := httptest.NewRecorder()
	fixture.Server.ServeHTTP(bad, authorizedRequest(http.MethodGet, path, nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("missing q status = %d, want 400", bad.Code)
	}

	// bad limit is a 400
	badLimit := httptest.NewRecorder()
	fixture.Server.ServeHTTP(badLimit, authorizedRequest(http.MethodGet, path+"?q=x&limit=-1", nil))
	if badLimit.Code != http.StatusBadRequest {
		t.Fatalf("bad limit status = %d, want 400", badLimit.Code)
	}

	// hook token is forbidden
	hookReq := httptest.NewRequest(http.MethodGet, path+"?q=redirect", nil)
	hookReq.Header.Set("Authorization", "Bearer hook-token")
	hookReq.Header.Set(protocolHeader, contract.ProtocolVersion)
	hookResp := httptest.NewRecorder()
	fixture.Server.ServeHTTP(hookResp, hookReq)
	if hookResp.Code != http.StatusForbidden {
		t.Fatalf("hook token status = %d, want 403", hookResp.Code)
	}
}
