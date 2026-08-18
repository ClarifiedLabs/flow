package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

func TestFlowListResponseDefaultMetadataIsRevisionCoherent(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	oldDefault, err := fixture.Bundle.Flows.Create(ctx, terminalFlowInput("api coherent old default"))
	if err != nil {
		t.Fatalf("create old default flow: %v", err)
	}
	newDefault, err := fixture.Bundle.Flows.Create(ctx, terminalFlowInput("api coherent new default"))
	if err != nil {
		t.Fatalf("create new default flow: %v", err)
	}
	if err := fixture.Bundle.Flows.SetDefaultFlow(ctx, oldDefault.ID); err != nil {
		t.Fatalf("set initial default flow: %v", err)
	}

	writerStore, err := flowdb.Open(ctx, flowgit.ProjectDatabasePath(fixture.DataDir, fixture.Project.ID))
	if err != nil {
		t.Fatalf("open second project database handle: %v", err)
	}
	t.Cleanup(func() { _ = writerStore.Close() })
	writer := coordinator.NewFlowService(writerStore.DB())

	listed := make(chan struct{})
	resumeResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resumeResponse) }) }
	t.Cleanup(release)
	// Pause after List has pinned its transaction snapshot but before it reads
	// default metadata. A handler that performs List followed by DefaultFlowID
	// will resume its second read after the switch below and return mismatched
	// per-flow and top-level defaults.
	fixture.Bundle.Flows.ListRowsReadTestHook = func() {
		close(listed)
		<-resumeResponse
	}
	t.Cleanup(func() { fixture.Bundle.Flows.ListRowsReadTestHook = nil })
	project := fixture.Server.forBundle(fixture.Bundle)

	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v2/flows", nil)
		project.handleFlowsPath(response, request, coordinator.Principal{Scope: coordinator.TokenScopeOwner})
		responseDone <- response
	}()

	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("flow-list response did not pause after pinning its list snapshot")
	}
	if err := writer.SetDefaultFlow(ctx, newDefault.ID); err != nil {
		t.Fatalf("switch default through second handle while flow-list snapshot is pinned: %v", err)
	}
	release()

	var response *httptest.ResponseRecorder
	select {
	case response = <-responseDone:
	case <-time.After(5 * time.Second):
		t.Fatal("flow-list response did not finish after release")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("GET /v2/flows status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var payload flowsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode flow-list response: %v", err)
	}
	if payload.DefaultFlowID != oldDefault.ID {
		t.Fatalf("response default_flow_id = %q, want transactional default %q", payload.DefaultFlowID, oldDefault.ID)
	}

	markedDefault := ""
	for _, flow := range payload.Flows {
		if flow.Default {
			if markedDefault != "" {
				t.Fatalf("response marks multiple defaults: %q and %q", markedDefault, flow.ID)
			}
			markedDefault = flow.ID
		}
		if flow.ID == newDefault.ID && flow.Default {
			t.Fatalf("response marks concurrently selected default %q in the older list snapshot", newDefault.ID)
		}
	}
	if markedDefault != payload.DefaultFlowID {
		t.Fatalf("response default marker = %q, default_flow_id = %q", markedDefault, payload.DefaultFlowID)
	}
	currentDefault, err := writer.DefaultFlowID(ctx)
	if err != nil {
		t.Fatalf("read current default flow: %v", err)
	}
	if currentDefault != newDefault.ID {
		t.Fatalf("current default = %q, want concurrent update %q", currentDefault, newDefault.ID)
	}
}

func terminalFlowInput(name string) coordinator.FlowInput {
	return coordinator.FlowInput{
		Name:      name,
		StartNode: "done",
		Nodes: []coordinator.FlowNodeInput{{
			Key:  "done",
			Name: "Done",
			Kind: coordinator.NodeTerminal,
			Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{
				Resolution: coordinator.ResolutionCompleted,
			}},
		}},
	}
}
