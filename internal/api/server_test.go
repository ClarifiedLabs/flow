package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestServerRequiresOwnerToken(t *testing.T) {
	server := newTestServer(t)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v2/tasks", nil)
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", response.Code)
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v2/tasks", nil)
	request.Header.Set("Authorization", "Bearer wrong")
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", response.Code)
	}
}

func TestServerReportsProtocolMismatch(t *testing.T) {
	server := newTestServer(t)

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, "/v2/tasks", nil)
	request.Header.Set(protocolHeader, "999")
	server.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	var body errorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "protocol_mismatch" {
		t.Fatalf("error code = %q, want protocol_mismatch", body.Error.Code)
	}
}

func TestHarnessOptionsUseLiveWorkerHarnessLabels(t *testing.T) {
	server := newTestServer(t)

	if _, err := server.registry.Directory().RegisterWorker(context.Background(), flowworker.RegisterWorkerInput{
		ID: "w-harness",
		Labels: map[string]string{
			flowharness.AgentHarnessLabel(flowharness.Harness): "true",
		},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register harness worker: %v", err)
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v2/harnesses", nil, http.StatusOK, &response)

	agents := harnessOptionNames(response.Agents)
	if len(agents) != 1 || !agents[flowharness.Harness] {
		t.Fatalf("agent harness options = %+v, want only harness", response.Agents)
	}
	consoles := harnessOptionNames(response.Consoles)
	if len(consoles) != 2 || !consoles[flowharness.Harness] || !consoles[flowharness.Shell] {
		t.Fatalf("console harness options = %+v, want harness and shell", response.Consoles)
	}
}

func TestHarnessOptionsIncludeModelsAvailableOnEveryLiveHarnessWorker(t *testing.T) {
	server := newTestServer(t)
	ctx := context.Background()
	minBudget := 1024

	common := flowharness.Model{
		ProviderID:   "anthropic",
		ProviderName: "anthropic",
		ModelID:      "claude-opus-4-8",
		QualifiedID:  "anthropic:claude-opus-4-8",
		ModelName:    "claude-opus-4-8",
		Harness:      flowharness.Harness,
		Reasoning: flowharness.ReasoningInfo{
			Supported: true,
			Options: []flowharness.ReasoningOption{{
				Type:   "effort",
				Values: []string{"low", "high"},
			}},
		},
	}
	onlyFirst := flowharness.Model{
		ProviderID:  "google",
		ModelID:     "gemini-3.5-flash",
		QualifiedID: "google:gemini-3.5-flash",
		Harness:     flowharness.Harness,
		Reasoning: flowharness.ReasoningInfo{
			Supported: true,
			Options: []flowharness.ReasoningOption{{
				Type: "budget_tokens",
				Min:  &minBudget,
			}},
		},
	}

	for _, input := range []flowworker.RegisterWorkerInput{
		{
			ID: "w-harness-a",
			Labels: map[string]string{
				flowharness.AgentHarnessLabel(flowharness.Harness): "true",
			},
			HarnessModels:           []flowharness.Model{onlyFirst, common},
			CapacityPersistentAgent: 1,
			HeartbeatTTL:            time.Minute,
		},
		{
			ID: "w-harness-b",
			Labels: map[string]string{
				flowharness.AgentHarnessLabel(flowharness.Harness): "true",
			},
			HarnessModels:           []flowharness.Model{common},
			CapacityPersistentAgent: 1,
			HeartbeatTTL:            time.Minute,
		},
	} {
		if _, err := server.registry.Directory().RegisterWorker(ctx, input); err != nil {
			t.Fatalf("register harness worker %s: %v", input.ID, err)
		}
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v2/harnesses", nil, http.StatusOK, &response)

	var harnessOption *contract.HarnessOption
	for i := range response.Agents {
		if response.Agents[i].Name == flowharness.Harness {
			harnessOption = &response.Agents[i]
			break
		}
	}
	if harnessOption == nil {
		t.Fatalf("agent harness options = %+v, missing harness", response.Agents)
	}
	if len(harnessOption.Models) != 1 || harnessOption.Models[0].QualifiedID != common.QualifiedID {
		t.Fatalf("harness models = %+v, want only %s", harnessOption.Models, common.QualifiedID)
	}
	if got := harnessOption.Models[0].Reasoning.Options[0].Values; len(got) != 2 || got[0] != "low" || got[1] != "high" {
		t.Fatalf("harness reasoning values = %#v", got)
	}
}

func TestHarnessOptionsExcludeExpiredWorkers(t *testing.T) {
	server := newTestServer(t)

	ctx := context.Background()
	liveModel := flowharness.Model{
		ProviderID:  "anthropic",
		ModelID:     "claude-opus-4-8",
		QualifiedID: "anthropic:claude-opus-4-8",
		Harness:     flowharness.Harness,
	}
	if _, err := server.registry.Directory().RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-live-harness",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		HarnessModels:           []flowharness.Model{liveModel},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register live harness worker: %v", err)
	}
	expiredModel := flowharness.Model{
		ProviderID:  "openai",
		ModelID:     "gpt-5.5",
		QualifiedID: "openai:gpt-5.5",
		Harness:     flowharness.Harness,
	}
	if _, err := server.registry.Directory().RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-expired-harness",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		HarnessModels:           []flowharness.Model{expiredModel},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register expired harness worker: %v", err)
	}
	if _, err := server.registry.global.DB().ExecContext(ctx, `UPDATE workers SET expires_at = ? WHERE id = ?`, "2020-01-01T00:00:00Z", "w-expired-harness"); err != nil {
		t.Fatalf("expire harness worker: %v", err)
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v2/harnesses", nil, http.StatusOK, &response)

	agents := harnessOptionNames(response.Agents)
	if len(agents) != 1 || !agents[flowharness.Harness] {
		t.Fatalf("agent harness options = %+v, want only harness", response.Agents)
	}
	consoles := harnessOptionNames(response.Consoles)
	if len(consoles) != 2 || !consoles[flowharness.Harness] || !consoles[flowharness.Shell] {
		t.Fatalf("console harness options = %+v, want harness and shell", response.Consoles)
	}
	for _, option := range response.Agents {
		if option.Name != flowharness.Harness {
			continue
		}
		if len(option.Models) != 1 || option.Models[0].QualifiedID != liveModel.QualifiedID {
			t.Fatalf("harness models = %+v, want only the live worker's %s", option.Models, liveModel.QualifiedID)
		}
	}
}

func TestHarnessOptionsIncludeDefaultArgs(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	registry, err := NewRegistry(RegistryOptions{
		DataDir:     dataDir,
		Global:      global,
		HarnessArgs: []string{"--model", "gpt-5"},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	if err := registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
		Token: "owner-token",
		Scope: coordinator.TokenScopeOwner,
	}); err != nil {
		t.Fatalf("store owner token: %v", err)
	}
	server, err := NewServer(ServerOptions{Registry: registry, OwnerToken: "owner-token"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if _, err := registry.Directory().RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-harness",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register harness worker: %v", err)
	}

	var response contract.HarnessesResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v2/harnesses", nil, http.StatusOK, &response)
	var harnessDefaults []string
	for _, option := range response.Agents {
		if option.Name == flowharness.Harness {
			harnessDefaults = option.DefaultArgs
		}
	}
	if len(harnessDefaults) != 2 || harnessDefaults[0] != "--model" || harnessDefaults[1] != "gpt-5" {
		t.Fatalf("harness default args = %#v", harnessDefaults)
	}
}

func TestWorkflowCheckRoleInstructionsUsesDedicatedReviewAggregator(t *testing.T) {
	node := coordinator.FlowNodeSnapshot{Config: coordinator.FlowNodeSnapshotConfig{
		ChangeReview: &coordinator.ChangeReviewNodeSnapshotConfig{
			Agents: []coordinator.SnapshotReviewAgent{{Agent: coordinator.AgentDefSnapshot{
				Name: "code-reviewer", Prompt: "Discover correctness findings.",
			}}},
			Aggregator: coordinator.AgentDefSnapshot{Name: "review-aggregator", Prompt: "Synthesize candidate reports."},
		},
	}}
	if got := workflowCheckRoleInstructions(node, "code-reviewer.node.nr-1"); got != "Discover correctness findings." {
		t.Fatalf("discovery role instructions = %q", got)
	}
	if got := workflowCheckRoleInstructions(node, coordinator.ReviewAggregationCheckName+".node.nr-1"); got != "Synthesize candidate reports." {
		t.Fatalf("aggregation role instructions = %q", got)
	}
}

func TestRegistrySeedsGlobalDefsWithConfiguredDefaultAgent(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	configured := flowharness.AgentSelection{Harness: flowharness.Harness, Model: "sonnet", ReasoningEffort: "high"}
	registry, err := NewRegistry(RegistryOptions{DataDir: dataDir, Global: global, DefaultAgent: configured})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	if got := registry.DefaultAgent(); got != configured {
		t.Fatalf("DefaultAgent = %+v, want %+v", got, configured)
	}
	defs, err := registry.GlobalAgentDefs().List(ctx)
	if err != nil {
		t.Fatalf("list global agent defs: %v", err)
	}
	if len(defs) != 8 {
		t.Fatalf("global default agent definitions = %d, want 8", len(defs))
	}
	for _, def := range defs {
		if def.Harness != flowharness.Harness || def.Model != "sonnet" || def.ReasoningEffort != "high" {
			t.Errorf("seeded %q = harness %q model %q effort %q, want harness/sonnet/high",
				def.Name, def.Harness, def.Model, def.ReasoningEffort)
		}
	}
}

func TestNewRegistryRejectsInvalidDefaultAgent(t *testing.T) {
	ctx := context.Background()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	_, err = NewRegistry(RegistryOptions{DataDir: t.TempDir(), Global: global, DefaultAgent: flowharness.AgentSelection{Harness: "bogus"}})
	if err == nil || !strings.Contains(err.Error(), "default agent") {
		t.Fatalf("new registry error = %v, want default agent rejection", err)
	}
}

func TestSessionHarnessForJobFallsBackToConfiguredDefault(t *testing.T) {
	changeID := "ch-test-0001"
	legacy := flowworker.Job{Role: flowworker.RoleAuthor, ChangeID: &changeID, Payload: map[string]any{}}
	if got := sessionHarnessForJob(legacy, flowharness.Agents); got != flowharness.Agents {
		t.Fatalf("sessionHarnessForJob legacy payload = %q, want %q", got, flowharness.Agents)
	}
	if got := sessionHarnessForJob(legacy, ""); got != flowharness.DefaultAgentName() {
		t.Fatalf("sessionHarnessForJob empty default = %q, want %q", got, flowharness.DefaultAgentName())
	}
	stamped := flowworker.Job{Role: flowworker.RoleAuthor, ChangeID: &changeID, Payload: map[string]any{"agent_harness": "harness"}}
	if got := sessionHarnessForJob(stamped, flowharness.Agents); got != flowharness.Harness {
		t.Fatalf("sessionHarnessForJob stamped payload = %q, want harness", got)
	}
	console := flowworker.Job{Role: flowworker.RoleConsole, Payload: map[string]any{}}
	if got := sessionHarnessForJob(console, flowharness.Agents); got != flowharness.DefaultConsoleName() {
		t.Fatalf("sessionHarnessForJob console = %q, want %q", got, flowharness.DefaultConsoleName())
	}
}

func TestDefaultAgentDefsAreGlobalAndInheritedByProjects(t *testing.T) {
	fixture := newTestFixture(t)

	var globalList agentDefsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/global/agent-defs", nil, http.StatusOK, &globalList)
	if len(globalList.AgentDefs) != 8 {
		t.Fatalf("global default agent definitions = %d, want 8", len(globalList.AgentDefs))
	}

	var projectList agentDefsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/agent-defs", nil, http.StatusOK, &projectList)
	if len(projectList.AgentDefs) != len(globalList.AgentDefs) {
		t.Fatalf("project agent definitions = %d, want %d inherited defaults", len(projectList.AgentDefs), len(globalList.AgentDefs))
	}
	for _, global := range globalList.AgentDefs {
		inherited := findAgentDefByName(projectList.AgentDefs, global.Name)
		if !global.Builtin || global.Inherited || global.Prompt == "" {
			t.Errorf("global default %q = %+v, want non-inherited built-in with prompt", global.Name, global)
		}
		if inherited == nil || inherited.ID != global.ID || !inherited.Builtin || !inherited.Inherited {
			t.Errorf("project default %q = %+v, want inherited global definition %+v", global.Name, inherited, global)
		}
	}

	var localCount int
	if err := fixture.DB.QueryRow(`SELECT COUNT(*) FROM agent_defs`).Scan(&localCount); err != nil {
		t.Fatalf("count project-local agent definitions: %v", err)
	}
	if localCount != 0 {
		t.Fatalf("project-local agent definitions = %d, want none", localCount)
	}
}

func TestCreateProjectRestoresMutatedGlobalDefaultAgentDefs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *Server, coordinator.AgentDef)
	}{
		{
			name: "renamed",
			mutate: func(t *testing.T, server *Server, def coordinator.AgentDef) {
				t.Helper()
				input := coordinator.AgentDefInput{
					Name: "renamed-author", Harness: def.Harness, Model: def.Model,
					ReasoningEffort: def.ReasoningEffort, Prompt: def.Prompt,
				}
				doJSONRequestAs(t, server, "owner-token", http.MethodPatch, "/v2/global/agent-defs/"+def.ID, input, http.StatusOK, nil)
			},
		},
		{
			name: "deleted",
			mutate: func(t *testing.T, server *Server, def coordinator.AgentDef) {
				t.Helper()
				doJSONRequestAs(t, server, "owner-token", http.MethodDelete, "/v2/global/agent-defs/"+def.ID, nil, http.StatusOK, nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			registry, _, _, _ := newTestRegistryInDir(t, t.TempDir())
			if err := registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
				Token: "owner-token", Scope: coordinator.TokenScopeOwner,
			}); err != nil {
				t.Fatalf("store owner token: %v", err)
			}
			server, err := NewServer(ServerOptions{Registry: registry, OwnerToken: "owner-token"})
			if err != nil {
				t.Fatalf("new server: %v", err)
			}

			author, err := registry.GlobalAgentDefs().GetByName(ctx, "author")
			if err != nil {
				t.Fatalf("get default author: %v", err)
			}
			tc.mutate(t, server, author)

			project, err := registry.CreateProject(ctx, coordinator.Project{Name: "after-" + tc.name, BaseBranch: "main"})
			if err != nil {
				t.Fatalf("create project after default was %s: %v", tc.name, err)
			}
			restored, err := registry.GlobalAgentDefs().GetByName(ctx, "author")
			if err != nil {
				t.Fatalf("get restored default author: %v", err)
			}
			if restored.ID == author.ID {
				t.Fatalf("restored author id = %q, want a new canonical definition after it was %s", restored.ID, tc.name)
			}

			bundle, ok := registry.Bundle(project.ID)
			if !ok {
				t.Fatalf("bundle for created project %q not found", project.ID)
			}
			coding, err := bundle.Flows.GetByName(ctx, "coding")
			if err != nil {
				t.Fatalf("get seeded coding flow: %v", err)
			}
			if got := coding.Nodes[0].Config.Agent.AgentDefID; got != restored.ID {
				t.Fatalf("seeded coding author id = %q, want restored global id %q", got, restored.ID)
			}
			var localCount int
			if err := bundle.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_defs`).Scan(&localCount); err != nil {
				t.Fatalf("count project-local agent definitions: %v", err)
			}
			if localCount != 0 {
				t.Fatalf("project-local agent definitions = %d, want none", localCount)
			}
		})
	}
}

func TestGlobalAgentDefMutationsWaitForProjectFlowSeeding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		body       func(coordinator.AgentDef) any
		wantStatus int
	}{
		{
			name:   "update",
			method: http.MethodPatch,
			body: func(def coordinator.AgentDef) any {
				return coordinator.AgentDefInput{
					Name: "renamed-author", Harness: def.Harness, Model: def.Model,
					ReasoningEffort: def.ReasoningEffort, Prompt: def.Prompt,
				}
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "delete",
			method:     http.MethodDelete,
			wantStatus: http.StatusConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			registry, _, _, _ := newTestRegistryInDir(t, t.TempDir())
			catalog := registry.GlobalAgentDefs()
			author, err := catalog.GetByName(ctx, "author")
			if err != nil {
				t.Fatalf("get default author: %v", err)
			}
			if err := registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
				Token: "owner-token", Scope: coordinator.TokenScopeOwner,
			}); err != nil {
				t.Fatalf("store owner token: %v", err)
			}
			server, err := NewServer(ServerOptions{Registry: registry, OwnerToken: "owner-token"})
			if err != nil {
				t.Fatalf("new server: %v", err)
			}

			seedReached := make(chan struct{})
			releaseSeed := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseSeed) }) }
			t.Cleanup(release)
			registry.beforeProjectFlowSeed = func() {
				close(seedReached)
				<-releaseSeed
			}

			type createResult struct {
				project coordinator.Project
				err     error
			}
			createDone := make(chan createResult, 1)
			go func() {
				project, createErr := registry.CreateProject(ctx, coordinator.Project{Name: "concurrent-" + tc.name, BaseBranch: "main"})
				createDone <- createResult{project: project, err: createErr}
			}()

			select {
			case <-seedReached:
			case <-time.After(10 * time.Second):
				t.Fatal("project creation did not reach the flow-seeding boundary")
			}
			if registry.catalogMu.TryLock() {
				registry.catalogMu.Unlock()
				t.Error("catalog mutex was not held at the flow-seeding boundary")
			}

			mutationLockBlocked := make(chan struct{})
			registry.catalogMutationLockBlocked = func() { close(mutationLockBlocked) }
			mutationDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				response := httptest.NewRecorder()
				var body any
				if tc.body != nil {
					body = tc.body(author)
				}
				request := authorizedRequest(tc.method, "/v2/global/agent-defs/"+author.ID, body)
				server.ServeHTTP(response, request)
				mutationDone <- response
			}()
			select {
			case <-mutationLockBlocked:
			case <-time.After(10 * time.Second):
				t.Fatalf("global %s did not block on the catalog lock", tc.name)
			}
			release()

			var created createResult
			select {
			case created = <-createDone:
			case <-time.After(10 * time.Second):
				t.Fatal("project creation did not finish after releasing flow seeding")
			}
			if created.err != nil {
				t.Fatalf("create project during global %s: %v", tc.name, created.err)
			}

			var mutationResponse *httptest.ResponseRecorder
			select {
			case mutationResponse = <-mutationDone:
			case <-time.After(10 * time.Second):
				t.Fatalf("global %s did not finish after project creation", tc.name)
			}
			if mutationResponse.Code != tc.wantStatus {
				t.Fatalf("global %s status = %d, want %d; body: %s", tc.name, mutationResponse.Code, tc.wantStatus, mutationResponse.Body.String())
			}

			bundle, ok := registry.Bundle(created.project.ID)
			if !ok {
				t.Fatalf("bundle for created project %q not found", created.project.ID)
			}
			coding, err := bundle.Flows.GetByName(ctx, "coding")
			if err != nil {
				t.Fatalf("get seeded coding flow: %v", err)
			}
			if got := coding.Nodes[0].Config.Agent.AgentDefID; got != author.ID {
				t.Fatalf("seeded coding author id = %q, want serialized global id %q", got, author.ID)
			}
			if _, err := bundle.AgentDefs.Resolve(ctx, author.ID); err != nil {
				t.Fatalf("resolve seeded global author after %s: %v", tc.name, err)
			}
		})
	}
}

func TestProjectFlowMutationWaitsForGlobalAgentDefDelete(t *testing.T) {
	ctx := context.Background()
	fixture := newTestFixture(t)
	custom, err := fixture.Registry.GlobalAgentDefs().Create(ctx, coordinator.AgentDefInput{
		Name: "concurrent-delete", Harness: "harness", Prompt: "custom global definition",
	})
	if err != nil {
		t.Fatalf("create global agent definition: %v", err)
	}

	deleteReached := make(chan struct{})
	releaseDelete := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDelete) }) }
	t.Cleanup(release)
	fixture.Registry.beforeGlobalAgentDefDelete = func() {
		close(deleteReached)
		<-releaseDelete
	}

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := authorizedRequest(http.MethodDelete, "/v2/global/agent-defs/"+custom.ID, nil)
		fixture.Server.ServeHTTP(response, request)
		deleteDone <- response
	}()
	select {
	case <-deleteReached:
	case <-time.After(10 * time.Second):
		t.Fatal("global delete did not reach the post-reference-check boundary")
	}
	if fixture.Registry.catalogMu.TryLock() {
		fixture.Registry.catalogMu.Unlock()
		t.Error("catalog mutex was not held after the global delete reference check")
	}

	flowInput := coordinator.FlowInput{
		Name: "concurrent-global-delete", StartNode: "author",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "author", Name: "Author", Kind: coordinator.NodeAgent, Config: coordinator.FlowNodeConfig{Agent: &coordinator.AgentNodeConfig{AgentDefID: custom.ID, Workspace: coordinator.WorkspaceChange, Artifact: coordinator.ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
		},
		Edges: []coordinator.FlowEdgeInput{{From: "author", Outcome: "completed", To: "done"}},
	}
	flowLockBlocked := make(chan struct{})
	fixture.Registry.catalogMutationLockBlocked = func() { close(flowLockBlocked) }
	flowDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := authorizedRequest(http.MethodPost, "/v2/projects/"+fixture.Project.ID+"/flows", flowInput)
		fixture.Server.ServeHTTP(response, request)
		flowDone <- response
	}()
	select {
	case <-flowLockBlocked:
	case <-time.After(10 * time.Second):
		t.Fatal("flow creation did not block on the catalog lock")
	}
	release()

	var deleteResponse *httptest.ResponseRecorder
	select {
	case deleteResponse = <-deleteDone:
	case <-time.After(10 * time.Second):
		t.Fatal("global delete did not finish after release")
	}
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("global delete status = %d, want 200; body: %s", deleteResponse.Code, deleteResponse.Body.String())
	}

	var flowResponse *httptest.ResponseRecorder
	select {
	case flowResponse = <-flowDone:
	case <-time.After(10 * time.Second):
		t.Fatal("flow creation did not finish after global delete")
	}
	if flowResponse.Code != http.StatusBadRequest {
		t.Fatalf("flow creation status = %d, want 400 after referenced global deletion; body: %s", flowResponse.Code, flowResponse.Body.String())
	}
	if _, err := fixture.Bundle.Flows.GetByName(ctx, flowInput.Name); !errors.Is(err, coordinator.ErrFlowNotFound) {
		t.Fatalf("get rejected flow error = %v, want ErrFlowNotFound", err)
	}
}

func TestGlobalAgentDefsAreInheritedAndProjectOverridesWin(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	input := coordinator.AgentDefInput{Name: "organization-reviewer", Harness: "harness", Model: "gpt-global", Prompt: "global prompt"}
	var created agentDefResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/global/agent-defs", input, http.StatusCreated, &created)
	if created.AgentDef.Inherited {
		t.Fatalf("global definition = %+v, should not be marked inherited in the global catalog", created.AgentDef)
	}

	var projectList agentDefsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/agent-defs", nil, http.StatusOK, &projectList)
	inherited := findAgentDefByName(projectList.AgentDefs, input.Name)
	if inherited == nil || inherited.ID != created.AgentDef.ID || !inherited.Inherited {
		t.Fatalf("project agent definitions = %+v, want inherited global definition", projectList.AgentDefs)
	}

	overrideInput := coordinator.AgentDefInput{Name: input.Name, Harness: "harness", Model: "sonnet", Prompt: "project prompt"}
	var override agentDefResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPatch, "/v2/projects/"+fixture.Project.ID+"/agent-defs/"+created.AgentDef.ID, overrideInput, http.StatusOK, &override)
	if override.AgentDef.ID == created.AgentDef.ID || override.AgentDef.Inherited || override.AgentDef.Prompt != "project prompt" {
		t.Fatalf("project override = %+v, want a new local definition", override.AgentDef)
	}

	projectList = agentDefsResponse{}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/agent-defs", nil, http.StatusOK, &projectList)
	effective := findAgentDefByName(projectList.AgentDefs, input.Name)
	if effective == nil || effective.ID != override.AgentDef.ID || effective.Inherited {
		t.Fatalf("effective project definition = %+v, want local override", effective)
	}
	var globalList agentDefsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/global/agent-defs", nil, http.StatusOK, &globalList)
	global := findAgentDefByName(globalList.AgentDefs, input.Name)
	if global == nil || global.ID != created.AgentDef.ID || global.Prompt != "global prompt" {
		t.Fatalf("global definition after project override = %+v", global)
	}

	flow, err := fixture.Bundle.Flows.Create(ctx, coordinator.FlowInput{
		Name: "inherited-flow", StartNode: "review",
		Nodes: []coordinator.FlowNodeInput{
			{Key: "review", Name: "Review", Kind: coordinator.NodeAgent, Config: coordinator.FlowNodeConfig{Agent: &coordinator.AgentNodeConfig{AgentDefID: created.AgentDef.ID, Workspace: coordinator.WorkspaceChange, Artifact: coordinator.ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: coordinator.NodeTerminal, Config: coordinator.FlowNodeConfig{Terminal: &coordinator.TerminalNodeConfig{Resolution: coordinator.ResolutionCompleted}}},
		},
		Edges: []coordinator.FlowEdgeInput{{From: "review", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create flow referencing global definition: %v", err)
	}
	snapshot, err := fixture.Bundle.Flows.ResolveSnapshot(ctx, flow.ID)
	if err != nil {
		t.Fatalf("resolve flow snapshot: %v", err)
	}
	node, ok := snapshot.Node("review")
	if !ok || node.Config.Agent == nil || node.Config.Agent.Agent.ID != override.AgentDef.ID || node.Config.Agent.Agent.Prompt != "project prompt" {
		t.Fatalf("resolved review agent = %+v, want project override", node.Config.Agent)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodDelete, "/v2/global/agent-defs/"+created.AgentDef.ID, nil, http.StatusConflict, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodDelete, "/v2/global/agent-defs/%20"+created.AgentDef.ID+"%20", nil, http.StatusConflict, nil)
	hookResponse := httptest.NewRecorder()
	hookRequest := authorizedRequest(http.MethodGet, "/v2/global/agent-defs", nil)
	hookRequest.Header.Set("Authorization", "Bearer hook-token")
	fixture.Server.ServeHTTP(hookResponse, hookRequest)
	if hookResponse.Code != http.StatusForbidden {
		t.Fatalf("hook global agent-def status = %d, want 403", hookResponse.Code)
	}
}

func findAgentDefByName(defs []coordinator.AgentDef, name string) *coordinator.AgentDef {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	return nil
}

func TestTaskAttachmentUploadDetailAndDownload(t *testing.T) {
	fixture := newTestFixture(t)
	task, err := fixture.Tasks.CreateTask(context.Background(), coordinator.CreateTaskInput{Title: "Attachment task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	uploadPath := "/v2/projects/" + fixture.Project.ID + "/tasks/" + task.ID + "/attachments"
	uploaded := uploadTaskAttachmentForTest(t, fixture, uploadPath, string(coordinator.TaskAttachmentStageReviewer), "review.png", "image/png", []byte("png-data"))
	if uploaded.Stage != coordinator.TaskAttachmentStageReviewer || uploaded.Filename != "review.png" || uploaded.ContentType != "image/png" {
		t.Fatalf("uploaded attachment = %+v", uploaded)
	}

	var detail taskResponse
	doJSONRequest(t, fixture.Server, http.MethodGet, "/v2/projects/"+fixture.Project.ID+"/tasks/"+task.ID, nil, http.StatusOK, &detail)
	if detail.Detail == nil || len(detail.Detail.Attachments) != 1 || detail.Detail.Attachments[0].ID != uploaded.ID {
		t.Fatalf("detail attachments = %+v", detail.Detail)
	}

	downloadPath := uploadPath + "/" + uploaded.ID
	download := getAs(t, fixture.Server, "owner-token", downloadPath+"?download=1")
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200; body: %s", download.Code, download.Body.String())
	}
	if got := download.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("download content type = %q, want image/png", got)
	}
	if got := download.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, "review.png") {
		t.Fatalf("download disposition = %q", got)
	}
	if download.Body.String() != "png-data" {
		t.Fatalf("download body = %q", download.Body.String())
	}

	inline := getAs(t, fixture.Server, "owner-token", downloadPath)
	if got := inline.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("inline content type = %q, want image/png", got)
	}
	if got := inline.Header().Get("Content-Disposition"); !strings.Contains(got, "inline") {
		t.Fatalf("inline disposition = %q", got)
	}
}

func TestTaskAttachmentUnsafeContentTypesAreDownloadOnly(t *testing.T) {
	fixture := newTestFixture(t)
	task, err := fixture.Tasks.CreateTask(context.Background(), coordinator.CreateTaskInput{Title: "Unsafe attachment task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	uploadPath := "/v2/projects/" + fixture.Project.ID + "/tasks/" + task.ID + "/attachments"
	for _, tc := range []struct {
		name        string
		filename    string
		contentType string
		body        string
	}{
		{name: "html", filename: "proof.html", contentType: "text/html", body: "<script>alert(1)</script>"},
		{name: "svg", filename: "proof.svg", contentType: "image/svg+xml", body: `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uploaded := uploadTaskAttachmentForTest(t, fixture, uploadPath, string(coordinator.TaskAttachmentStageReviewer), tc.filename, tc.contentType, []byte(tc.body))
			if uploaded.ContentType != tc.contentType {
				t.Fatalf("stored content type = %q, want %q", uploaded.ContentType, tc.contentType)
			}

			response := getAs(t, fixture.Server, "owner-token", uploadPath+"/"+uploaded.ID)
			if response.Code != http.StatusOK {
				t.Fatalf("attachment status = %d, want 200; body: %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("content type = %q, want application/octet-stream", got)
			}
			disposition := response.Header().Get("Content-Disposition")
			if !strings.Contains(disposition, "attachment") || strings.Contains(disposition, "inline") || !strings.Contains(disposition, tc.filename) {
				t.Fatalf("content disposition = %q, want attachment with filename %q", disposition, tc.filename)
			}
			if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("x-content-type-options = %q, want nosniff", got)
			}
			if response.Body.String() != tc.body {
				t.Fatalf("attachment body = %q", response.Body.String())
			}
		})
	}
}

func uploadTaskAttachmentForTest(t *testing.T, fixture testFixture, uploadPath string, stage string, filename string, contentType string, data []byte) coordinator.TaskAttachment {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("stage", stage); err != nil {
		t.Fatalf("write stage: %v", err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name":     "file",
		"filename": filename,
	}))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("create multipart part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write multipart part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	upload := httptest.NewRecorder()
	request := authorizedRequest(http.MethodPost, uploadPath, nil)
	request.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	request.ContentLength = int64(body.Len())
	request.Header.Set("Content-Type", writer.FormDataContentType())
	fixture.Server.ServeHTTP(upload, request)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201; body: %s", upload.Code, upload.Body.String())
	}
	var uploaded taskAttachmentResponse
	if err := json.NewDecoder(upload.Body).Decode(&uploaded); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	return uploaded.Attachment
}

func TestIdempotentCreateDoesNotDuplicateTask(t *testing.T) {
	fixture := newTestFixture(t)

	first := taskResponse{}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks", createTaskRequest{
		Title: "Idempotent task",
	}, http.StatusCreated, &first, idempotencyHeader, "create-1")

	second := taskResponse{}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks", createTaskRequest{
		Title: "Idempotent task",
	}, http.StatusCreated, &second, idempotencyHeader, "create-1")

	if first.Task.ID != second.Task.ID {
		t.Fatalf("second idempotent create returned %s, want %s", second.Task.ID, first.Task.ID)
	}
	tasks, err := fixture.Tasks.ListTasks(context.Background(), coordinator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(tasks))
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks", createTaskRequest{
		Title: "Different task",
	}, http.StatusConflict, nil, idempotencyHeader, "create-1")
}

func TestListTasksFiltersByTextSearch(t *testing.T) {
	fixture := newTestFixture(t)

	var match taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks", createTaskRequest{
		Title: "Fix flaky checkout retries",
	}, http.StatusCreated, &match)
	var other taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks", createTaskRequest{
		Title: "Ship the changelog",
	}, http.StatusCreated, &other)

	// ?q= matches title/body case-insensitively and narrows the aggregate list.
	var found aggregateTasksResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks?q=FLAKY", nil, http.StatusOK, &found)
	if len(found.Tasks) != 1 || found.Tasks[0].ID != match.Task.ID {
		t.Fatalf("q=FLAKY tasks = %+v, want only %s", found.Tasks, match.Task.ID)
	}

	// The text search composes with the lifecycle state filter (AND): both
	// tasks are unscheduled, so the scheduled filter empties the same search.
	found = aggregateTasksResponse{}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks?q=flaky&state=scheduled", nil, http.StatusOK, &found)
	if len(found.Tasks) != 0 {
		t.Fatalf("q=flaky&state=scheduled tasks = %+v, want none", found.Tasks)
	}
	found = aggregateTasksResponse{}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks?q=flaky&state=unscheduled", nil, http.StatusOK, &found)
	if len(found.Tasks) != 1 || found.Tasks[0].ID != match.Task.ID {
		t.Fatalf("q=flaky&state=unscheduled tasks = %+v, want %s", found.Tasks, match.Task.ID)
	}
}

func TestConcurrentIdempotentCreateDoesNotDuplicateTask(t *testing.T) {
	fixture := newTestFixture(t)

	const requests = 24
	start := make(chan struct{})
	results := make(chan taskResponse, requests)
	errors := make(chan string, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response := httptest.NewRecorder()
			request := authorizedRequest(http.MethodPost, "/v2/tasks", createTaskRequest{Title: "Concurrent idempotent task"})
			request.Header.Set(idempotencyHeader, "concurrent-create")
			fixture.Server.ServeHTTP(response, request)
			if response.Code != http.StatusCreated {
				errors <- response.Body.String()
				return
			}
			var body taskResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				errors <- err.Error()
				return
			}
			results <- body
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)

	for err := range errors {
		t.Fatalf("idempotent request failed: %s", err)
	}

	taskIDs := map[string]bool{}
	for result := range results {
		taskIDs[result.Task.ID] = true
	}
	if len(taskIDs) != 1 {
		t.Fatalf("idempotent task IDs = %+v, want exactly one", taskIDs)
	}

	tasks, err := fixture.Tasks.ListTasks(context.Background(), coordinator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(tasks))
	}
}

func TestCredentialStoreInvalidCredentialDoesNotFallBackToConfiguredToken(t *testing.T) {
	fixture := newTestFixture(t)
	if _, err := fixture.GlobalDB.ExecContext(context.Background(), `
UPDATE tokens
SET revoked_at = ?
WHERE token_hash = ?`, time.Now().UTC().Format(time.RFC3339Nano), coordinator.HashToken("owner-token")); err != nil {
		t.Fatalf("revoke owner token: %v", err)
	}

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, "/v2/tasks", nil)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked owner token status = %d, want 401", response.Code)
	}

	if _, err := fixture.GlobalDB.ExecContext(context.Background(), `
UPDATE tokens
SET revoked_at = ?
WHERE token_hash = ?`, time.Now().UTC().Format(time.RFC3339Nano), coordinator.HashToken("hook-token")); err != nil {
		t.Fatalf("revoke hook token: %v", err)
	}

	response = httptest.NewRecorder()
	request = authorizedRequest(http.MethodPost, fixture.gitEventsPath(), gitEventsRequest{
		OldSHA: "old",
		NewSHA: "new",
		Ref:    "refs/heads/task/t-api-0001",
		Actor:  "hook",
	})
	request.Header.Set("Authorization", "Bearer hook-token")
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked hook token status = %d, want 401", response.Code)
	}
}

func TestWebUIBootstrapLoginAndCookieAuth(t *testing.T) {
	fixture := newTestFixture(t)

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, http.StatusForbidden, nil)

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)
	if !strings.HasPrefix(bootstrap.LoginPath, "/ui/login?token=") || bootstrap.ExpiresAt.IsZero() {
		t.Fatalf("bootstrap = %+v", bootstrap)
	}

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil)
	fixture.Server.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}
	if login.Header().Get("Location") != "/ui/" {
		t.Fatalf("login location = %q, want /ui/", login.Header().Get("Location"))
	}
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			sessionCookie = cookie
		case webCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("login cookies = %+v", login.Result().Cookies())
	}
	if !sessionCookie.HttpOnly {
		t.Fatalf("session cookie is not HttpOnly: %+v", sessionCookie)
	}
	if csrfCookie.HttpOnly {
		t.Fatalf("csrf cookie should be readable by browser JavaScript: %+v", csrfCookie)
	}
	if sessionCookie.Path != "/ui" || csrfCookie.Path != "/ui" {
		t.Fatalf("cookie paths = session:%q csrf:%q, want /ui", sessionCookie.Path, csrfCookie.Path)
	}

	reusedLogin := httptest.NewRecorder()
	fixture.Server.ServeHTTP(reusedLogin, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if reusedLogin.Code != http.StatusUnauthorized {
		t.Fatalf("reused login status = %d, want 401", reusedLogin.Code)
	}

	directBoard := httptest.NewRecorder()
	directBoardRequest := httptest.NewRequest(http.MethodGet, "/v2/board", nil)
	directBoardRequest.Header.Set(webCSRFHeader, csrfCookie.Value)
	directBoardRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(directBoard, directBoardRequest)
	if directBoard.Code != http.StatusUnauthorized {
		t.Fatalf("direct cookie board status = %d, want 401; body: %s", directBoard.Code, directBoard.Body.String())
	}

	missingReadCSRF := httptest.NewRecorder()
	missingReadCSRFRequest := httptest.NewRequest(http.MethodGet, "/ui/api/v2/board", nil)
	missingReadCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(missingReadCSRF, missingReadCSRFRequest)
	if missingReadCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("missing read csrf status = %d, want 401; body: %s", missingReadCSRF.Code, missingReadCSRF.Body.String())
	}

	boardResponse := httptest.NewRecorder()
	boardRequest := httptest.NewRequest(http.MethodGet, "/ui/api/v2/board", nil)
	boardRequest.Header.Set(webCSRFHeader, csrfCookie.Value)
	boardRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(boardResponse, boardRequest)
	if boardResponse.Code != http.StatusOK {
		t.Fatalf("cookie board status = %d, want 200; body: %s", boardResponse.Code, boardResponse.Body.String())
	}

	missingCSRF := httptest.NewRecorder()
	missingCSRFRequest := httptest.NewRequest(http.MethodPost, "/ui/api/v2/tasks", strings.NewReader(`{"title":"Missing csrf"}`))
	missingCSRFRequest.Header.Set("Content-Type", "application/json")
	missingCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(missingCSRF, missingCSRFRequest)
	if missingCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("missing csrf status = %d, want 401; body: %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	var created taskResponse
	withCSRF := httptest.NewRecorder()
	withCSRFRequest := httptest.NewRequest(http.MethodPost, "/ui/api/v2/tasks", strings.NewReader(`{"title":"Browser task"}`))
	withCSRFRequest.Header.Set("Content-Type", "application/json")
	withCSRFRequest.Header.Set(webCSRFHeader, csrfCookie.Value)
	withCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(withCSRF, withCSRFRequest)
	if withCSRF.Code != http.StatusCreated {
		t.Fatalf("with csrf status = %d, want 201; body: %s", withCSRF.Code, withCSRF.Body.String())
	}
	if err := json.NewDecoder(withCSRF.Body).Decode(&created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.Task.Title != "Browser task" {
		t.Fatalf("created task = %+v", created.Task)
	}
}

func loginWebUI(t *testing.T, fixture testFixture) (*http.Cookie, *http.Cookie) {
	t.Helper()

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)
	login := httptest.NewRecorder()
	fixture.Server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			sessionCookie = cookie
		case webCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("login cookies = %+v", login.Result().Cookies())
	}
	return sessionCookie, csrfCookie
}

func TestWebUIRoutesAndAssets(t *testing.T) {
	fixture := newTestFixture(t)

	for _, path := range []string{"/ui/", "/ui/board", "/ui/merge", "/ui/projects/" + fixture.Project.ID + "/tasks/t-api-0001", "/ui/changes/ch-0001", "/ui/sessions/s-0001/terminal", "/ui/workers", "/ui/jobs"} {
		response := httptest.NewRecorder()
		fixture.Server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body: %s", path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "<flow-app>") {
			t.Fatalf("%s did not serve app shell: %s", path, response.Body.String())
		}
	}

	asset := httptest.NewRecorder()
	fixture.Server.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/ui/assets/app.js?v=test-version", nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want 200; body: %s", asset.Code, asset.Body.String())
	}
	if asset.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("asset content type = %q", asset.Header().Get("Content-Type"))
	}
	if asset.Header().Get("Cache-Control") != "max-age=31536000, immutable" {
		t.Fatalf("asset cache control = %q", asset.Header().Get("Cache-Control"))
	}
	if !strings.Contains(asset.Body.String(), "customElements.define") {
		t.Fatalf("asset body missing app script")
	}

	// Unversioned asset requests — notably the browser's native ES module
	// imports (import "./markdown.js"), which carry no ?v= cache key — must
	// revalidate via ETag rather than be cached immutably, so an edited module
	// is never served stale. (The behavior keys on the ?v= query, not the file
	// name, so an unversioned app.js request exercises the same code path.)
	module := httptest.NewRecorder()
	fixture.Server.ServeHTTP(module, httptest.NewRequest(http.MethodGet, "/ui/assets/app.js", nil))
	if module.Code != http.StatusOK {
		t.Fatalf("unversioned asset status = %d, want 200", module.Code)
	}
	if got := module.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unversioned asset cache control = %q, want no-cache", got)
	}
	etag := module.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("unversioned asset missing ETag")
	}

	// A matching If-None-Match must yield 304 Not Modified with an empty body.
	revalidated := httptest.NewRecorder()
	conditional := httptest.NewRequest(http.MethodGet, "/ui/assets/app.js", nil)
	conditional.Header.Set("If-None-Match", etag)
	fixture.Server.ServeHTTP(revalidated, conditional)
	if revalidated.Code != http.StatusNotModified {
		t.Fatalf("conditional asset status = %d, want 304", revalidated.Code)
	}
	if revalidated.Body.Len() != 0 {
		t.Fatalf("304 response should have empty body, got %d bytes", revalidated.Body.Len())
	}
}

func TestWebUITerminalAttachCreatesOwnerBrowserURL(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Web terminal task")
	if _, err := fixture.Sessions.RegisterTerminal(context.Background(), started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}

	var bootstrap webBootstrapResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/ui/bootstrap", map[string]string{}, http.StatusOK, &bootstrap)
	login := httptest.NewRecorder()
	fixture.Server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, bootstrap.LoginPath, nil))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", login.Code, login.Body.String())
	}
	var sessionCookie *http.Cookie
	var csrfCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		switch cookie.Name {
		case webSessionCookie:
			sessionCookie = cookie
		case webCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("login cookies = %+v", login.Result().Cookies())
	}

	missingCSRF := httptest.NewRecorder()
	missingCSRFRequest := httptest.NewRequest(http.MethodPost, "/ui/api/v2/sessions/"+started.Session.ID+"/terminal-token", strings.NewReader(`{}`))
	missingCSRFRequest.Header.Set("Content-Type", "application/json")
	missingCSRFRequest.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(missingCSRF, missingCSRFRequest)
	if missingCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("missing csrf status = %d, want 401; body: %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/ui/api/v2/sessions/"+started.Session.ID+"/terminal-token", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(webCSRFHeader, csrfCookie.Value)
	request.AddCookie(sessionCookie)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("terminal-token status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var access sessionTerminalAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&access); err != nil {
		t.Fatalf("decode terminal access: %v", err)
	}
	if !strings.HasPrefix(access.Access.LoginPath, "/v2/sessions/"+started.Session.ID+"/terminal-login?token=") {
		t.Fatalf("terminal access = %+v", access.Access)
	}
}

func TestBoardIncludesUITaskCardReadModels(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Card read model task")
	if _, err := fixture.Sessions.UpdateSessionState(context.Background(), started.Session.ID, coordinator.SessionWaiting); err != nil {
		t.Fatalf("mark waiting: %v", err)
	}
	if _, err := fixture.Status.WriteSessionStatus(context.Background(), started.Session.ID, "Waiting on product decision", "author", coordinator.StatusKindNote); err != nil {
		t.Fatalf("write status: %v", err)
	}
	if _, err := fixture.DB.Exec(`
INSERT INTO handoff_snapshots (
	change_id,
	head_sha,
	present,
	valid,
	summary,
	content,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		started.Change.ID,
		"abc123",
		1,
		1,
		"Waiting for product decision before final polish.",
		"# Flow Handoff\n\n## Current Goal\n\nWaiting for product decision before final polish.\n\nPRIVATE HANDOFF DETAIL - do not expose\n",
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert handoff snapshot: %v", err)
	}
	required := true
	if _, err := fixture.Checks.ReportCheck(context.Background(), coordinator.ReportCheckInput{
		TaskID:   started.Session.TaskID,
		Name:     "unit",
		Required: &required,
		Verdict:  coordinator.CheckBlocked,
		Reporter: "ci",
	}); err != nil {
		t.Fatalf("report check: %v", err)
	}

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, fixture.boardPath(), nil)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("board status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "token_hash") || strings.Contains(response.Body.String(), "TokenHash") {
		t.Fatalf("board response leaked session token hash: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "PRIVATE HANDOFF DETAIL") || strings.Contains(response.Body.String(), `"content"`) {
		t.Fatalf("board response leaked handoff content: %s", response.Body.String())
	}
	for _, obsolete := range []string{`"review_state"`, `"blocking_reason"`, `"primary_action"`, `"flow"`} {
		if strings.Contains(response.Body.String(), obsolete) {
			t.Fatalf("board response contains obsolete card field %s: %s", obsolete, response.Body.String())
		}
	}
	var board boardResponse
	if err := json.NewDecoder(response.Body).Decode(&board); err != nil {
		t.Fatalf("decode board: %v", err)
	}

	card, ok := board.TaskCards[started.Session.TaskID]
	if !ok {
		t.Fatalf("task cards = %+v, missing %s", board.TaskCards, started.Session.TaskID)
	}
	if card.ActiveSession == nil || card.ActiveSession.ID != started.Session.ID || card.ActiveSession.State != coordinator.SessionWaiting {
		t.Fatalf("active session summary = %+v", card.ActiveSession)
	}
	if card.Change == nil || card.Change.ID != started.Change.ID || card.Change.Branch != started.Change.Branch {
		t.Fatalf("change summary = %+v, want %s", card.Change, started.Change.ID)
	}
	if card.LatestStatus == nil || card.LatestStatus.Message != "Waiting on product decision" {
		t.Fatalf("latest status = %+v", card.LatestStatus)
	}
	if card.Handoff == nil || !card.Handoff.Present || !card.Handoff.Valid || card.Handoff.Summary != "Waiting for product decision before final polish." || card.Handoff.HeadSHA != "abc123" {
		t.Fatalf("handoff summary = %+v", card.Handoff)
	}
	if card.RequiredChecks.Total != 1 || card.RequiredChecks.Blocked != 1 {
		t.Fatalf("required check summary = %+v", card.RequiredChecks)
	}
	if card.CurrentStep == nil || card.CurrentStep.Key != "implement" || card.CurrentStep.Name != "Implement" || card.CurrentStep.Kind != coordinator.NodeAgent {
		t.Fatalf("current workflow step = %+v, want frozen Implement agent node", card.CurrentStep)
	}
	if card.TerminalAvailable {
		t.Fatal("terminal should not be available before a target is registered")
	}

	if _, err := fixture.Sessions.RegisterTerminal(context.Background(), started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}
	response = httptest.NewRecorder()
	request = authorizedRequest(http.MethodGet, fixture.boardPath(), nil)
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("board with terminal status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&board); err != nil {
		t.Fatalf("decode board with terminal: %v", err)
	}
	card = board.TaskCards[started.Session.TaskID]
	if !card.TerminalAvailable {
		t.Fatalf("terminal availability = false, card = %+v", card)
	}
	if card.ActiveSession == nil || !card.ActiveSession.TerminalAvailable {
		t.Fatalf("active session terminal availability = %+v", card.ActiveSession)
	}
}

func TestBoardCurrentStepUsesFrozenWorkflowSnapshot(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	flow, err := fixture.Bundle.Flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("load coding flow: %v", err)
	}
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Frozen card step", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule workflow: %v", err)
	}

	nodes := make([]coordinator.FlowNodeInput, 0, len(flow.Nodes))
	for _, node := range flow.Nodes {
		name := node.Name
		if node.Key == "implement" {
			name = "Renamed live implementation"
		}
		nodes = append(nodes, coordinator.FlowNodeInput{Key: node.Key, Name: name, Kind: node.Kind, Config: node.Config})
	}
	if _, err := fixture.Bundle.Flows.Update(ctx, flow.ID, coordinator.FlowInput{
		Name:             flow.Name,
		Description:      flow.Description,
		StartNode:        flow.StartNode,
		TransitionBudget: flow.TransitionBudget,
		Nodes:            nodes,
		Edges:            flow.Edges,
	}); err != nil {
		t.Fatalf("rename live flow node: %v", err)
	}

	assertCurrentStep := func(wantState coordinator.LaneState) {
		t.Helper()
		var board boardResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
		if board.LaneStates[task.ID] != wantState {
			t.Fatalf("lane state = %q, want %q", board.LaneStates[task.ID], wantState)
		}
		step := board.TaskCards[task.ID].CurrentStep
		if step == nil || step.Key != "implement" || step.Name != "Implement" || step.Kind != coordinator.NodeAgent {
			t.Fatalf("current step = %+v, want frozen Implement agent node", step)
		}
	}

	assertCurrentStep(coordinator.LaneStateScheduled)
	if err := fixture.Bundle.WorkflowExecutor.Advance(ctx, run.ID); err != nil {
		t.Fatalf("advance workflow: %v", err)
	}
	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list workflow jobs: %v", err)
	}
	var nodeRunID string
	for _, job := range jobs {
		if job.TaskID != nil && *job.TaskID == task.ID && job.NodeRunID != nil {
			nodeRunID = *job.NodeRunID
			break
		}
	}
	if nodeRunID == "" {
		t.Fatalf("advanced workflow jobs = %+v, want current node run", jobs)
	}
	if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, nodeRunID); err != nil {
		t.Fatalf("mark current node running: %v", err)
	}
	// Advancing the run enqueued an author job that no worker has claimed, so
	// the derived lane state is awaiting_worker rather than working.
	assertCurrentStep(coordinator.LaneStateAwaitingWorker)
}

func TestBoardCurrentStepToleratesMissingAndMalformedRunData(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	createTask := func(title string) coordinator.Task {
		t.Helper()
		task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: title})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return task
	}
	scheduleTask := func(title string) (coordinator.Task, coordinator.WorkflowRun) {
		t.Helper()
		task := createTask(title)
		run, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, task.ID)
		if err != nil {
			t.Fatalf("schedule %s: %v", title, err)
		}
		return task, run
	}

	unscheduled := createTask("No workflow yet")
	missingRun := createTask("Scheduled without a run")
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE tasks SET lifecycle_state = 'scheduled' WHERE id = ?`, missingRun.ID); err != nil {
		t.Fatalf("mark task scheduled without run: %v", err)
	}

	emptyKey, emptyKeyRun := scheduleTask("Empty workflow key")
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE workflow_runs SET current_node_key = '' WHERE id = ?`, emptyKeyRun.ID); err != nil {
		t.Fatalf("clear current node key: %v", err)
	}

	unresolved, unresolvedRun := scheduleTask("Unresolved workflow key")
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE workflow_runs SET current_node_key = 'legacy-node_key' WHERE id = ?`, unresolvedRun.ID); err != nil {
		t.Fatalf("set unresolved current node key: %v", err)
	}

	emptyName, emptyNameRun := scheduleTask("Empty snapshot node name")
	for i := range emptyNameRun.Snapshot.Nodes {
		if emptyNameRun.Snapshot.Nodes[i].Key == emptyNameRun.CurrentNodeKey {
			emptyNameRun.Snapshot.Nodes[i].Name = ""
		}
	}
	snapshotJSON, err := json.Marshal(emptyNameRun.Snapshot)
	if err != nil {
		t.Fatalf("encode malformed snapshot: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE workflow_runs SET flow_snapshot_json = ? WHERE id = ?`, string(snapshotJSON), emptyNameRun.ID); err != nil {
		t.Fatalf("empty snapshot node name: %v", err)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	for _, taskID := range []string{unscheduled.ID, missingRun.ID, emptyKey.ID} {
		if step := board.TaskCards[taskID].CurrentStep; step != nil {
			t.Fatalf("task %s current step = %+v, want omitted", taskID, step)
		}
	}
	if step := board.TaskCards[unresolved.ID].CurrentStep; step == nil || step.Key != "legacy-node_key" || step.Name != "legacy node key" || step.Kind != "" {
		t.Fatalf("unresolved current step = %+v, want readable stable-key fallback", step)
	}
	if step := board.TaskCards[emptyName.ID].CurrentStep; step == nil || step.Key != "implement" || step.Name != "implement" || step.Kind != coordinator.NodeAgent {
		t.Fatalf("empty-name current step = %+v, want key fallback with resolved kind", step)
	}
}

func TestBoardSupportsActiveBaseWorkspaceSessionWithoutChange(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Plan task set"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	const workerID = "w-base-workspace"
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{
			"workspace_mode": coordinator.WorkspaceBase,
			"branch":         "main",
			"base":           "main",
		},
	})
	if err != nil {
		t.Fatalf("enqueue base-workspace job: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, workerID, job.ID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark job running: %v", err)
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: workerID,
	})
	if err != nil {
		t.Fatalf("start base-workspace session: %v", err)
	}
	if started.Session.ChangeID != "" {
		t.Fatalf("base-workspace session change id = %q, want empty", started.Session.ChangeID)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	card, ok := board.TaskCards[task.ID]
	if !ok {
		t.Fatalf("task cards = %+v, missing %s", board.TaskCards, task.ID)
	}
	if card.ActiveSession == nil || card.ActiveSession.ID != started.Session.ID {
		t.Fatalf("active session summary = %+v, want %s", card.ActiveSession, started.Session.ID)
	}
	if card.Change != nil {
		t.Fatalf("change summary = %+v, want nil", card.Change)
	}
}

func TestBoardHidesUITaskCardsFromSessionTokens(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	source, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Session source"})
	if err != nil {
		t.Fatalf("create source task: %v", err)
	}
	unrelated := startAuthorSessionForStatusTest(t, fixture, "Unrelated live task")
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "session-token",
		Scope:        coordinator.TokenScopeSession,
		Subject:      "s-session",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: &source.ID,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	response := httptest.NewRecorder()
	request := authorizedRequest(http.MethodGet, fixture.boardPath(), nil)
	request.Header.Set("Authorization", "Bearer session-token")
	fixture.Server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("board status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "task_cards") || strings.Contains(response.Body.String(), unrelated.Session.ID) || strings.Contains(response.Body.String(), unrelated.Session.WorkerID) {
		t.Fatalf("session board leaked card metadata: %s", response.Body.String())
	}
	var board boardResponse
	if err := json.NewDecoder(response.Body).Decode(&board); err != nil {
		t.Fatalf("decode session board: %v", err)
	}
	if board.LaneStates[unrelated.Session.TaskID] != coordinator.LaneStateWorking {
		t.Fatalf("session board lane states = %+v, want working for %s", board.LaneStates, unrelated.Session.TaskID)
	}
}

func TestBoardUITaskCardsShowRelationBlockers(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	blocker, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Finish dependency"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Blocked work"})
	if err != nil {
		t.Fatalf("create blocked task: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, blocker.ID, blocked.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, blocked.ID); err != nil {
		t.Fatalf("schedule blocked workflow: %v", err)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	if len(board.Board.Scheduled) != 1 || board.Board.Scheduled[0].ID != blocked.ID {
		t.Fatalf("scheduled lane = %+v, want %s", board.Board.Scheduled, blocked.ID)
	}
	if len(board.BlockedIDs) != 0 {
		t.Fatalf("blocked ids = %+v, want no in-progress workflow wait", board.BlockedIDs)
	}
	card, ok := board.TaskCards[blocked.ID]
	if !ok {
		t.Fatalf("task cards = %+v, missing %s", board.TaskCards, blocked.ID)
	}
	if card.Blockers.Count != 1 || len(card.Blockers.Tasks) != 1 || card.Blockers.Tasks[0].ID != blocker.ID {
		t.Fatalf("blocker summary = %+v", card.Blockers)
	}
	if card.CurrentStep == nil || card.CurrentStep.Name != "Implement" {
		t.Fatalf("scheduled blocked task current step = %+v, want Implement", card.CurrentStep)
	}
}

func TestBoardUITaskCardsIncludeTagsAndRelationSummary(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	parent, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Parent"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Child"})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	related, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Related"})
	if err != nil {
		t.Fatalf("create related: %v", err)
	}
	tag, err := fixture.Tasks.CreateTag(ctx, coordinator.CreateTagInput{Slug: "triage-tag", Name: "Triage Tag"})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := fixture.Tasks.TagTask(ctx, child.ID, tag.ID, coordinator.ActorHuman); err != nil {
		t.Fatalf("tag child: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, parent.ID, child.ID, coordinator.RelationParentOf, coordinator.ActorHuman); err != nil {
		t.Fatalf("link parent: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, child.ID, related.ID, coordinator.RelationRelatedTo, coordinator.ActorHuman); err != nil {
		t.Fatalf("link related: %v", err)
	}

	var board boardResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, fixture.boardPath(), nil, http.StatusOK, &board)
	card, ok := board.TaskCards[child.ID]
	if !ok {
		t.Fatalf("task cards = %+v, missing %s", board.TaskCards, child.ID)
	}
	if len(card.Tags) != 1 || card.Tags[0].Slug != "triage-tag" {
		t.Fatalf("card tags = %+v", card.Tags)
	}
	if card.Relations.Total != 2 || card.Relations.Parents != 1 || card.Relations.Related != 1 {
		t.Fatalf("card relation summary = %+v", card.Relations)
	}
}

func TestTaskDetailReadModelIsOwnerOnly(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Task detail metadata")
	tag, err := fixture.Tasks.CreateTag(ctx, coordinator.CreateTagInput{Slug: "web-ui", Name: "Web UI"})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := fixture.Tasks.TagTask(ctx, started.Session.TaskID, tag.ID, coordinator.ActorHuman); err != nil {
		t.Fatalf("tag task: %v", err)
	}
	blocker, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Blocker"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, blocker.ID, started.Session.TaskID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link blocker: %v", err)
	}
	if _, err := fixture.Sessions.RegisterTerminal(ctx, started.Session.ID, "http://127.0.0.1:7777"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}
	required := true
	if _, err := fixture.Checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		TaskID: started.Session.TaskID, Name: "unit", Kind: coordinator.CheckKindCI,
		Required: &required, Verdict: coordinator.CheckPending,
	}); err != nil {
		t.Fatalf("report detail check: %v", err)
	}

	var owner taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+started.Session.TaskID, nil, http.StatusOK, &owner)
	if owner.Detail == nil {
		t.Fatal("owner task response missing detail")
	}
	if len(owner.Detail.Tags) != 1 || owner.Detail.Tags[0].Slug != "web-ui" {
		t.Fatalf("detail tags = %+v", owner.Detail.Tags)
	}
	if len(owner.Detail.Relations) != 1 || owner.Detail.Relations[0].SourceTaskID != blocker.ID {
		t.Fatalf("detail relations = %+v", owner.Detail.Relations)
	}
	if owner.Detail.ActiveSession == nil || owner.Detail.ActiveSession.ID != started.Session.ID {
		t.Fatalf("active session = %+v", owner.Detail.ActiveSession)
	}
	if !owner.Detail.TerminalAvailable || !owner.Detail.ActiveSession.TerminalAvailable {
		t.Fatalf("active terminal availability = detail:%t session:%+v", owner.Detail.TerminalAvailable, owner.Detail.ActiveSession)
	}
	if len(owner.Detail.Sessions) != 1 || len(owner.Detail.Changes) != 1 || owner.Detail.Changes[0].ID != started.Change.ID {
		t.Fatalf("sessions/changes = %+v / %+v", owner.Detail.Sessions, owner.Detail.Changes)
	}
	if !owner.Detail.Sessions[0].TerminalAvailable {
		t.Fatalf("session terminal availability = %+v", owner.Detail.Sessions[0])
	}
	if owner.Detail.RequiredChecks.Total != 1 || owner.Detail.RequiredChecks.Pending != 1 || len(owner.Detail.Checks) != 1 {
		t.Fatalf("checks = %+v summary=%+v", owner.Detail.Checks, owner.Detail.RequiredChecks)
	}

	var session taskResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodGet, "/v2/tasks/"+started.Session.TaskID, nil, http.StatusOK, &session)
	if session.Detail != nil {
		t.Fatalf("session task response leaked detail: %+v", session.Detail)
	}
}

func TestPromptContextAdvertisesNestedPlanningWorkflow(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	planning, err := fixture.Bundle.Flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("get planning flow: %v", err)
	}
	coding, err := fixture.Bundle.Flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("get coding flow: %v", err)
	}
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{
		Title: "Plan follow-on work", Body: "Produce coding tasks and narrower planning tasks where needed.", FlowID: planning.ID,
	})
	if err != nil {
		t.Fatalf("create planning task: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, task.ID); err != nil {
		t.Fatalf("schedule planning workflow: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token: "planner-session-token", Scope: coordinator.TokenScopeSession, Subject: "s-planner",
		ProjectID: &fixture.Project.ID, SourceTaskID: &task.ID,
	}); err != nil {
		t.Fatalf("store planner session token: %v", err)
	}

	var response promptContextResponse
	doJSONRequestAs(t, fixture.Server, "planner-session-token", http.MethodGet, "/v2/tasks/"+task.ID+"/prompt-context", nil, http.StatusOK, &response)
	if response.ArtifactKind != coordinator.ArtifactTaskSet || response.TaskSetWorkflow == nil {
		t.Fatalf("planning prompt context = %+v", response)
	}
	contract := response.TaskSetWorkflow
	if contract.DefaultChildFlowID != coding.ID || !contract.AllowChildFlowOverride || contract.MaxItems != 25 {
		t.Fatalf("task-set workflow contract = %+v", contract)
	}
	options := map[string]string{}
	for _, option := range contract.AvailableFlows {
		options[option.Name] = option.ID
	}
	if options["coding"] != coding.ID || options["planning"] != planning.ID {
		t.Fatalf("available workflow options = %+v", contract.AvailableFlows)
	}

	var workerResponse promptContextResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/tasks/"+task.ID+"/prompt-context", nil, http.StatusOK, &workerResponse)
	if workerResponse.TaskSetWorkflow != nil {
		t.Fatalf("worker prompt context leaked project workflow catalog: %+v", workerResponse.TaskSetWorkflow)
	}

	doJSONRequestAs(t, fixture.Server, "planner-session-token", http.MethodGet, "/v2/flows", nil, http.StatusForbidden, nil)
}

func TestArtifactSubmissionRejectsDisallowedChildWorkflowOverride(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	planning, err := fixture.Bundle.Flows.GetByName(ctx, "planning")
	if err != nil {
		t.Fatalf("get planning flow: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE flow_nodes
SET config_json = json_set(config_json, '$.materialize_task_set.allow_child_flow_override', json('false'))
WHERE flow_id = ? AND kind = 'materialize_task_set'`, planning.ID); err != nil {
		t.Fatalf("disable planning child-workflow overrides: %v", err)
	}
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Restricted plan", FlowID: planning.ID})
	if err != nil {
		t.Fatalf("create planning task: %v", err)
	}
	scheduled, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule planning workflow: %v", err)
	}
	if err := fixture.Bundle.WorkflowExecutor.Advance(ctx, scheduled.ID); err != nil {
		t.Fatalf("advance planning workflow: %v", err)
	}
	run, ok, err := fixture.Bundle.WorkflowRuns.ActiveForTask(ctx, task.ID)
	if err != nil || !ok || strings.TrimSpace(run.CurrentNodeRunID) == "" {
		t.Fatalf("active planning workflow = %+v found=%t err=%v", run, ok, err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, run.CurrentNodeRunID); err != nil {
		t.Fatalf("mark planning node running: %v", err)
	}
	payload, err := json.Marshal(coordinator.TaskSetManifest{
		SchemaVersion: 1,
		Tasks:         []coordinator.TaskSetItem{{Key: "nested-plan", Title: "Nested plan", Body: "Plan the nested work.", FlowID: planning.ID}},
	})
	if err != nil {
		t.Fatalf("marshal task set: %v", err)
	}
	var response errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+task.ID+"/workflow/artifacts", coordinator.CreateWorkflowArtifactInput{
		NodeRunID: run.CurrentNodeRunID, Kind: coordinator.ArtifactTaskSet,
		SummaryMarkdown: "Nested planning work.", Payload: payload, ClientKey: "restricted-override",
	}, http.StatusBadRequest, &response)
	if !strings.Contains(response.Error.Message, "may not override default child flow") {
		t.Fatalf("artifact rejection = %+v, want override-policy error", response)
	}
}

func TestWorkerTokenCanReadTask(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{
		Title: "Reviewer prompt context",
		Body:  "Check jobs fetch task context with the worker token. Worker-scope task reads must succeed.",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	var worker taskResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/tasks/"+task.ID, nil, http.StatusOK, &worker)
	if worker.Task.ID != task.ID || worker.Task.Body != task.Body {
		t.Fatalf("worker task response = %+v", worker.Task)
	}
	if worker.Detail != nil {
		t.Fatalf("worker task response leaked owner detail: %+v", worker.Detail)
	}
}

func TestHookTokenCanPostGitEvents(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "session-token",
		Scope:        coordinator.TokenScopeSession,
		Subject:      "s-1",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: nil,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	event := gitEventsRequest{
		OldSHA: "old",
		NewSHA: "new",
		Ref:    "refs/heads/task/t-api-0001",
		Actor:  "owner",
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusForbidden, nil)

	var response gitEventsResponse
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusAccepted, &response)
	if response.Recorded != 1 || response.Inserted != 1 {
		t.Fatalf("git event response = %+v", response)
	}

	events, err := fixture.GitEvents.List(ctx)
	if err != nil {
		t.Fatalf("list git events: %v", err)
	}
	if len(events) != 1 || events[0].Ref != "refs/heads/task/t-api-0001" || events[0].Source != coordinator.GitEventSourceAPI {
		t.Fatalf("events = %+v", events)
	}
}

func TestDrainGitEventSpoolRecoversMissedPostReceive(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := t.TempDir()
	repointFixtureExchange(t, fixture, exchangePath)

	if err := flowgit.HandlePostReceive(context.Background(), flowgit.HookOptions{
		ExchangeRepoPath: exchangePath,
		Stdin:            bytes.NewBufferString("old new refs/heads/task/t-api-0001\n"),
	}); err != nil {
		t.Fatalf("post receive spool: %v", err)
	}

	var response drainGitEventsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/git/events/drain", drainGitEventsRequest{
		ExchangeRepoPath: exchangePath,
	}, http.StatusOK, &response)
	if response.Drained != 1 {
		t.Fatalf("Drained = %d, want 1", response.Drained)
	}

	events, err := fixture.GitEvents.List(context.Background())
	if err != nil {
		t.Fatalf("list git events: %v", err)
	}
	if len(events) != 1 || events[0].Source != coordinator.GitEventSourceSpool {
		t.Fatalf("events = %+v", events)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/git/events/drain", drainGitEventsRequest{
		ExchangeRepoPath: exchangePath,
	}, http.StatusOK, &response)
	if response.Drained != 0 {
		t.Fatalf("second Drained = %d, want 0", response.Drained)
	}
}

func TestGitEventsDeduplicateDirectPostAndSpoolDrain(t *testing.T) {
	fixture := newTestFixture(t)
	exchangePath := t.TempDir()
	repointFixtureExchange(t, fixture, exchangePath)

	event := gitEventsRequest{
		OldSHA: "old",
		NewSHA: "new",
		Ref:    "refs/heads/task/t-api-0001",
		Actor:  "owner",
	}
	var postResponse gitEventsResponse
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodPost, fixture.gitEventsPath(), event, http.StatusAccepted, &postResponse)
	if postResponse.Inserted != 1 {
		t.Fatalf("Inserted = %d, want 1", postResponse.Inserted)
	}

	t.Setenv("FLOW_GIT_PRINCIPAL", "owner")
	if err := flowgit.HandlePostReceive(context.Background(), flowgit.HookOptions{
		ExchangeRepoPath: exchangePath,
		Stdin:            bytes.NewBufferString("old new refs/heads/task/t-api-0001\n"),
	}); err != nil {
		t.Fatalf("post receive spool: %v", err)
	}

	var drainResponse drainGitEventsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/git/events/drain", drainGitEventsRequest{
		ExchangeRepoPath: exchangePath,
	}, http.StatusOK, &drainResponse)
	if drainResponse.Drained != 0 {
		t.Fatalf("Drained = %d, want 0 for duplicate event", drainResponse.Drained)
	}
	events, err := fixture.GitEvents.List(context.Background())
	if err != nil {
		t.Fatalf("list git events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
}

func TestWorkerHTTPLifecycleAndJobDiagnostics(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Worker API task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/jobs", enqueueJobRequest{
		TaskID:         &task.ID,
		Role:           string(flowworker.RoleAuthor),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
	}, http.StatusForbidden, nil)

	var enqueue jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/jobs", enqueueJobRequest{
		TaskID:         &task.ID,
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
		Priority:       5,
		Payload:        map[string]any{"entrypoint": "make test"},
	}, http.StatusCreated, &enqueue)
	if enqueue.Job.State != flowworker.JobQueued || enqueue.Job.TaskID == nil || *enqueue.Job.TaskID != task.ID {
		t.Fatalf("enqueued job = %+v", enqueue.Job)
	}

	var list jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/jobs", nil, http.StatusOK, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != enqueue.Job.ID {
		t.Fatalf("jobs list = %+v", list.Jobs)
	}

	var reapJobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/workers/reap-jobs", nil, http.StatusOK, &reapJobs)
	if len(reapJobs.Jobs) != 1 || reapJobs.Jobs[0].ID != enqueue.Job.ID || reapJobs.Jobs[0].State != flowworker.JobQueued {
		t.Fatalf("worker reap jobs = %+v", reapJobs.Jobs)
	}
	if reapJobs.Jobs[0].Payload != nil {
		t.Fatalf("worker reap job payload = %+v, want omitted", reapJobs.Jobs[0].Payload)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/workers/reap-jobs", nil, http.StatusForbidden, nil)

	var show jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/jobs/"+enqueue.Job.ID, nil, http.StatusOK, &show)
	if show.Job.ID != enqueue.Job.ID {
		t.Fatalf("show job = %+v, want %s", show.Job, enqueue.Job.ID)
	}

	var registered workerResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, &registered)
	if registered.Worker.ID != "w-local" || registered.Worker.LastHeartbeatAt == nil || registered.Worker.ExpiresAt == nil {
		t.Fatalf("registered worker = %+v", registered.Worker)
	}

	var heartbeat workerResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/heartbeat", heartbeatWorkerRequest{
		WorkerID:            "w-local",
		HeartbeatTTLSeconds: 120,
	}, http.StatusOK, &heartbeat)
	if heartbeat.Worker.ID != "w-local" || heartbeat.Worker.ExpiresAt == nil {
		t.Fatalf("heartbeat worker = %+v", heartbeat.Worker)
	}

	var workers workersResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/workers", nil, http.StatusOK, &workers)
	if len(workers.Workers) != 1 || workers.Workers[0].ID != "w-local" {
		t.Fatalf("workers list = %+v", workers.Workers)
	}
	if workers.Queue.Queued != 1 || workers.Queue.PersistentAgent != 1 || workers.Queue.CI != 1 {
		t.Fatalf("worker queue summary = %+v", workers.Queue)
	}
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/workers", nil, http.StatusForbidden, nil)

	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
	if claim.Job.ID != enqueue.Job.ID || claim.Job.State != flowworker.JobClaimed {
		t.Fatalf("claimed job = %+v, want %s claimed", claim.Job, enqueue.Job.ID)
	}
	if claim.Lease.WorkerID != "w-local" {
		t.Fatalf("lease worker = %q, want w-local", claim.Lease.WorkerID)
	}

	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Job.ID != enqueue.Job.ID || running.Job.State != flowworker.JobRunning {
		t.Fatalf("running job = %+v, want %s running", running.Job, enqueue.Job.ID)
	}

	var runningJobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/jobs", nil, http.StatusOK, &runningJobs)
	runningDiagnostics, ok := runningJobs.Diagnostics[enqueue.Job.ID]
	if !ok || runningDiagnostics.Lease == nil || runningDiagnostics.Lease.ID != claim.Lease.ID || !runningDiagnostics.LiveLease || runningDiagnostics.LeaseStatus != "live" || runningDiagnostics.TmuxSession == "" {
		t.Fatalf("running job diagnostics = %+v ok=%t", runningDiagnostics, ok)
	}
	var liveWorkers workersResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/workers", nil, http.StatusOK, &liveWorkers)
	liveWorkerDiagnostics := liveWorkers.Diagnostics["w-local"]
	if liveWorkers.Queue.Queued != 0 || liveWorkerDiagnostics.LiveJobs != 1 || liveWorkerDiagnostics.LivePersistentAgent != 1 {
		t.Fatalf("live worker diagnostics = queue:%+v worker:%+v", liveWorkers.Queue, liveWorkerDiagnostics)
	}

	var renew leaseResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/renew", renewLeaseRequest{
		LeaseID:              claim.Lease.ID,
		LeaseDurationSeconds: 120,
	}, http.StatusOK, &renew)
	if renew.Lease.RenewalCount != 1 {
		t.Fatalf("renewal count = %d, want 1", renew.Lease.RenewalCount)
	}

	var release jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/release", releaseLeaseRequest{
		LeaseID:    claim.Lease.ID,
		FinalState: string(flowworker.JobFinished),
	}, http.StatusOK, &release)
	if release.Job.State != flowworker.JobFinished {
		t.Fatalf("released job state = %q, want finished", release.Job.State)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/jobs/"+enqueue.Job.ID, nil, http.StatusOK, &show)
	if show.Job.State != flowworker.JobFinished {
		t.Fatalf("show released job state = %q, want finished", show.Job.State)
	}
	if show.Diagnostics == nil || show.Diagnostics.Lease == nil || show.Diagnostics.LiveLease || show.Diagnostics.LeaseStatus != "released" {
		t.Fatalf("released job diagnostics = %+v", show.Diagnostics)
	}
}

func TestWorkerJoinTokenCreatesAndRotatesWorkerCredential(t *testing.T) {
	fixture := newTestFixture(t)
	server, err := NewServer(ServerOptions{
		Registry:        fixture.Registry,
		OwnerToken:      "owner-token",
		HookToken:       "hook-token",
		WorkerJoinToken: "join-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	doJSONRequestAs(t, server, "join-token", http.MethodPost, "/v2/workers/join", joinWorkerRequest{
		WorkerID: "w-local",
	}, http.StatusOK, nil)
	if _, err := fixture.Credentials.Authenticate(context.Background(), "worker-token"); err != coordinator.ErrInvalidCredential {
		t.Fatalf("old worker-token authenticate err = %v, want ErrInvalidCredential", err)
	}

	var joined joinWorkerResponse
	doJSONRequestAs(t, server, "join-token", http.MethodPost, "/v2/workers/join", joinWorkerRequest{
		WorkerID: "w-remote",
	}, http.StatusOK, &joined)
	if joined.WorkerID != "w-remote" || strings.TrimSpace(joined.Token) == "" {
		t.Fatalf("joined = %+v, want worker token for w-remote", joined)
	}
	principal, err := fixture.Credentials.Authenticate(context.Background(), joined.Token)
	if err != nil {
		t.Fatalf("authenticate joined token: %v", err)
	}
	if principal.Scope != coordinator.TokenScopeWorker || principal.Subject != "w-remote" {
		t.Fatalf("principal = %+v, want worker w-remote", principal)
	}
}

func TestWorkerJoinDisabledWithoutJoinToken(t *testing.T) {
	fixture := newTestFixture(t)

	doJSONRequestAs(t, fixture.Server, "join-token", http.MethodPost, "/v2/workers/join", joinWorkerRequest{
		WorkerID: "w-local",
	}, http.StatusNotFound, nil)
}

func TestWorkerJoinRejectsInvalidJoinToken(t *testing.T) {
	fixture := newTestFixture(t)
	server, err := NewServer(ServerOptions{
		Registry:        fixture.Registry,
		OwnerToken:      "owner-token",
		HookToken:       "hook-token",
		WorkerJoinToken: "join-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	doJSONRequestAs(t, server, "wrong-token", http.MethodPost, "/v2/workers/join", joinWorkerRequest{
		WorkerID: "w-local",
	}, http.StatusUnauthorized, nil)
}

func TestConsoleAPILifecycleAndScope(t *testing.T) {
	fixture := newTestFixture(t)

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/console", consoleRequest{
		Harness: "harness",
	}, http.StatusCreated, &startedConsole)
	if !startedConsole.Active || startedConsole.Job == nil || startedConsole.Job.Role != flowworker.RoleConsole {
		t.Fatalf("started console response = %+v", startedConsole)
	}
	if startedConsole.Job.TaskID != nil || startedConsole.Job.ChangeID != nil {
		t.Fatalf("console job task/change = %v/%v, want nil", startedConsole.Job.TaskID, startedConsole.Job.ChangeID)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Role != flowworker.RoleConsole || running.Session.TaskID != "" || running.Session.ChangeID != "" || running.SessionToken == "" {
		t.Fatalf("running console = %+v", running)
	}
	consoleToken := running.SessionToken
	sessionID := running.Session.ID

	var current consoleResponse
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v2/console", nil, http.StatusOK, &current)
	if !current.Active || current.Session == nil || current.Session.ID != sessionID {
		t.Fatalf("current console = %+v", current)
	}
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/sessions/"+sessionID+"/terminal", sessionTerminalRequest{
		TmuxSocketPath: "/tmp/flow-console-test.sock",
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/sessions/"+sessionID+"/event", sessionEventRequest{
		State: string(coordinator.SessionWaiting),
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/sessions/"+sessionID+"/status", sessionStatusRequest{
		Message: "unsupported",
	}, http.StatusBadRequest, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/sessions/"+sessionID+"/ready", readySessionRequest{}, http.StatusBadRequest, nil)

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/tasks", createTaskRequest{
		Title: "Console-created task",
	}, http.StatusCreated, &created)
	if created.Task.CreatedBy != coordinator.ActorAgent || created.Task.CreatedBySessionID == nil || *created.Task.CreatedBySessionID != sessionID {
		t.Fatalf("console-created task audit = %+v", created.Task)
	}
	title := "Console-edited task"
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPatch, "/v2/tasks/"+created.Task.ID, editTaskRequest{
		Title: &title,
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/tasks/"+created.Task.ID+"/schedule", scheduleTaskRequest{
		State: string(coordinator.ScheduleUpNext),
	}, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/tasks/"+created.Task.ID+"/checks/unit", reportCheckRequest{
		Kind:    string(coordinator.CheckKindCI),
		Verdict: string(coordinator.CheckSatisfied),
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/tasks/"+created.Task.ID+"/merge", map[string]string{}, http.StatusNotFound, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v2/jobs", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v2/workers", nil, http.StatusForbidden, nil)

	var blocker taskResponse
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/tasks", createTaskRequest{
		Title: "Console blocker",
	}, http.StatusCreated, &blocker)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodPost, "/v2/tasks/"+blocker.Task.ID+"/relations", relationRequest{
		TargetTaskID: created.Task.ID,
		Kind:         string(coordinator.RelationBlocks),
	}, http.StatusNoContent, nil)
	relations, err := fixture.Tasks.RelationsForTask(context.Background(), created.Task.ID)
	if err != nil {
		t.Fatalf("relations for console task: %v", err)
	}
	if len(relations) != 1 || relations[0].CreatedBy != coordinator.ActorAgent {
		t.Fatalf("relations = %+v, want one agent-created relation", relations)
	}
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodDelete, "/v2/tasks/"+blocker.Task.ID+"/relations", relationRequest{
		TargetTaskID: created.Task.ID,
		Kind:         string(coordinator.RelationBlocks),
	}, http.StatusNoContent, nil)

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/release", releaseLeaseRequest{
		LeaseID:    claim.Lease.ID,
		FinalState: string(flowworker.JobFinished),
	}, http.StatusBadRequest, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodDelete, "/v2/console", nil, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, consoleToken, http.MethodGet, "/v2/console", nil, http.StatusUnauthorized, nil)
}

func TestGetTaskRelationsAPI(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	source, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Source"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	target, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Target"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := fixture.Tasks.LinkTasks(ctx, source.ID, target.ID, coordinator.RelationBlocks, coordinator.ActorHuman); err != nil {
		t.Fatalf("link tasks: %v", err)
	}

	var sourceRelations contract.TaskRelationsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+source.ID+"/relations", nil, http.StatusOK, &sourceRelations)
	if len(sourceRelations.Relations) != 1 || sourceRelations.Relations[0].SourceTaskID != source.ID || sourceRelations.Relations[0].TargetTaskID != target.ID {
		t.Fatalf("source relations = %+v, want one blocks relation", sourceRelations.Relations)
	}
	if sourceRelations.Relations[0].SourceTitle != "Source" || sourceRelations.Relations[0].TargetTitle != "Target" {
		t.Fatalf("source relation titles = %q/%q, want %q/%q", sourceRelations.Relations[0].SourceTitle, sourceRelations.Relations[0].TargetTitle, "Source", "Target")
	}

	var targetRelations contract.TaskRelationsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+target.ID+"/relations", nil, http.StatusOK, &targetRelations)
	if len(targetRelations.Relations) != 1 || targetRelations.Relations[0].SourceTaskID != source.ID {
		t.Fatalf("target relations = %+v, want one incoming blocks relation", targetRelations.Relations)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/tasks/"+target.ID+"/relations", nil, http.StatusForbidden, nil)
}

func TestCreateTaskWithRelationsAPI(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	target, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Relation target"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Task with relations",
			Relations: []relationRequest{
				{TargetTaskID: target.ID, Kind: string(coordinator.RelationBlocks)},
			},
		}, http.StatusCreated, &created)

	relations, err := fixture.Tasks.RelationsForTask(ctx, created.Task.ID)
	if err != nil {
		t.Fatalf("relations for created task: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceTaskID != created.Task.ID || relations[0].TargetTaskID != target.ID || relations[0].Kind != coordinator.RelationBlocks {
		t.Fatalf("created task relations = %+v, want one blocks relation to target", relations)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Invalid relation kind",
			Relations: []relationRequest{
				{TargetTaskID: target.ID, Kind: "depends_on"},
			},
		}, http.StatusBadRequest, nil)

	// A relation whose source and target both resolve to the newly created task
	// is a self-reference and must be rejected.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Self referencing relation",
			Relations: []relationRequest{
				{Kind: string(coordinator.RelationBlocks)},
			},
		}, http.StatusBadRequest, nil)

	// A blank target_task_id is required to be rejected even when a source is
	// supplied: it must not be rewritten to the newly created task.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Blank target relation",
			Relations: []relationRequest{
				{SourceTaskID: target.ID, TargetTaskID: "", Kind: string(coordinator.RelationBlocks)},
			},
		}, http.StatusBadRequest, nil)
}

func TestCreateTaskWithRelationsSessionShorthandAPI(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	source, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Session source task"})
	if err != nil {
		t.Fatalf("create source task: %v", err)
	}
	other, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Unowned task"})
	if err != nil {
		t.Fatalf("create unowned task: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:        "session-token",
		Scope:        coordinator.TokenScopeSession,
		Subject:      "s-relations",
		ProjectID:    &fixture.Project.ID,
		SourceTaskID: &source.ID,
	}); err != nil {
		t.Fatalf("store session token: %v", err)
	}

	// A session bound to a task may relate that task to the newly created task
	// using the source-owned -> blank-target shorthand; the blank target resolves
	// to the new task rather than being rejected.
	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Session child task",
			Relations: []relationRequest{
				{SourceTaskID: source.ID, TargetTaskID: "", Kind: string(coordinator.RelationParentOf)},
			},
		}, http.StatusCreated, &created)

	relations, err := fixture.Tasks.RelationsForTask(ctx, source.ID)
	if err != nil {
		t.Fatalf("relations for source task: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceTaskID != source.ID || relations[0].TargetTaskID != created.Task.ID || relations[0].Kind != coordinator.RelationParentOf {
		t.Fatalf("source task relations = %+v, want one parent_of relation to the new task %s", relations, created.Task.ID)
	}

	// The shorthand must not extend to tasks the session does not own.
	doJSONRequestAs(t, fixture.Server, "session-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Session unrelated relation",
			Relations: []relationRequest{
				{SourceTaskID: other.ID, TargetTaskID: "", Kind: string(coordinator.RelationBlocks)},
			},
		}, http.StatusForbidden, nil)
}

// TestCreateTaskWithParentOfRelationAtomicAPI covers the owner-token new-task
// child-of path: a create request names an existing parent as the relation
// source with a blank target flagged target_is_new_task, and the single create
// transaction links "parent parent_of new task". A parent that cannot be linked
// fails the whole create (no committed parentless task); resubmitting the same
// request then recovers with exactly one linked child rather than a duplicate.
// Malformed uses of the flag are rejected up front.
func TestCreateTaskWithParentOfRelationAtomicAPI(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	parent, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Chosen parent"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	countTasks := func() int {
		t.Helper()
		tasks, err := fixture.Tasks.ListTasks(ctx, coordinator.TaskFilter{})
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}
		return len(tasks)
	}
	before := countTasks()

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Child task",
			Relations: []relationRequest{
				{SourceTaskID: parent.ID, TargetTaskID: "", Kind: string(coordinator.RelationParentOf), TargetIsNewTask: true},
			},
		}, http.StatusCreated, &created)

	relations, err := fixture.Tasks.RelationsForTask(ctx, created.Task.ID)
	if err != nil {
		t.Fatalf("relations for created task: %v", err)
	}
	if len(relations) != 1 || relations[0].SourceTaskID != parent.ID || relations[0].TargetTaskID != created.Task.ID || relations[0].Kind != coordinator.RelationParentOf {
		t.Fatalf("created task relations = %+v, want one %s parent_of %s", relations, parent.ID, created.Task.ID)
	}

	// A child-of link that cannot be applied (the named parent does not exist)
	// must fail the whole create: nothing is committed, so resubmitting the form
	// creates exactly one task rather than a duplicate plus an orphan.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Orphan task",
			Relations: []relationRequest{
				{SourceTaskID: "t-missing", TargetTaskID: "", Kind: string(coordinator.RelationParentOf), TargetIsNewTask: true},
			},
		}, http.StatusBadRequest, nil)
	if got := countTasks(); got != before+1 {
		t.Fatalf("task count after failed create = %d, want %d (failed create must not commit)", got, before+1)
	}

	// The flag requires a blank target and a named source; otherwise it is a
	// malformed request, rejected before anything is created.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Flag with non-blank target",
			Relations: []relationRequest{
				{SourceTaskID: parent.ID, TargetTaskID: parent.ID, Kind: string(coordinator.RelationParentOf), TargetIsNewTask: true},
			},
		}, http.StatusBadRequest, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Flag with blank source",
			Relations: []relationRequest{
				{SourceTaskID: "", TargetTaskID: "", Kind: string(coordinator.RelationParentOf), TargetIsNewTask: true},
			},
		}, http.StatusBadRequest, nil)
	if got := countTasks(); got != before+1 {
		t.Fatalf("task count after malformed requests = %d, want %d", got, before+1)
	}

	// Recovery: resubmitting the same create request after the earlier failure
	// succeeds and commits exactly one more task. It is not a duplicate of a
	// committed orphan, because the failed create left nothing behind, and the
	// retried child carries the single "parent parent_of child" relation in the
	// correct direction.
	var retried taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{
			Title: "Child task",
			Relations: []relationRequest{
				{SourceTaskID: parent.ID, TargetTaskID: "", Kind: string(coordinator.RelationParentOf), TargetIsNewTask: true},
			},
		}, http.StatusCreated, &retried)
	if got := countTasks(); got != before+2 {
		t.Fatalf("task count after recovery retry = %d, want %d (one recovered create on top of the first)", got, before+2)
	}
	retriedRelations, err := fixture.Tasks.RelationsForTask(ctx, retried.Task.ID)
	if err != nil {
		t.Fatalf("relations for retried task: %v", err)
	}
	if len(retriedRelations) != 1 || retriedRelations[0].SourceTaskID != parent.ID || retriedRelations[0].TargetTaskID != retried.Task.ID || retriedRelations[0].Kind != coordinator.RelationParentOf {
		t.Fatalf("retried task relations = %+v, want one %s parent_of %s", retriedRelations, parent.ID, retried.Task.ID)
	}
}

func TestTaskConsoleAPILifecycleAndScope(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Task recovery console"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	other, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Other task"})
	if err != nil {
		t.Fatalf("create other task: %v", err)
	}

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+task.ID+"/console", consoleRequest{
		Harness: flowharness.Shell,
	}, http.StatusCreated, &startedConsole)
	if !startedConsole.Active || startedConsole.Job == nil || startedConsole.Job.Role != flowworker.RoleConsole {
		t.Fatalf("started task console response = %+v", startedConsole)
	}
	if startedConsole.Job.TaskID == nil || *startedConsole.Job.TaskID != task.ID || startedConsole.Job.ChangeID == nil {
		t.Fatalf("task console job task/change = %v/%v, want %s/change", startedConsole.Job.TaskID, startedConsole.Job.ChangeID, task.ID)
	}
	if got := payloadString(startedConsole.Job.Payload, "console_scope"); got != "task_recovery" {
		t.Fatalf("console_scope = %q, want task_recovery", got)
	}
	if got := payloadString(startedConsole.Job.Payload, "session_purpose"); got != "task_console" {
		t.Fatalf("session_purpose = %q, want task_console", got)
	}
	if got := payloadString(startedConsole.Job.Payload, "branch"); got != "task/"+task.ID {
		t.Fatalf("task console branch = %q, want task/%s", got, task.ID)
	}

	var projectConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/console", nil, http.StatusOK, &projectConsole)
	if projectConsole.Active {
		t.Fatalf("project console should ignore task console state: %+v", projectConsole)
	}
	var currentTaskConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+task.ID+"/console", nil, http.StatusOK, &currentTaskConsole)
	if !currentTaskConsole.Active || currentTaskConsole.Job == nil || currentTaskConsole.Job.ID != startedConsole.Job.ID {
		t.Fatalf("current task console = %+v, want job %s", currentTaskConsole, startedConsole.Job.ID)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/register", registerWorkerRequest{
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim task console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Role != flowworker.RoleConsole || running.Session.TaskID != task.ID || running.Session.ChangeID != *startedConsole.Job.ChangeID || running.SessionToken == "" {
		t.Fatalf("running task console = %+v", running)
	}
	principal, err := fixture.Credentials.Authenticate(ctx, running.SessionToken)
	if err != nil {
		t.Fatalf("authenticate task console token: %v", err)
	}
	if principal.Scope != coordinator.TokenScopeConsole || principal.SourceTaskID == nil || *principal.SourceTaskID != task.ID {
		t.Fatalf("task console principal = %+v", principal)
	}
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodGet, "/v2/tasks/"+task.ID, nil, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodGet, "/v2/tasks/"+other.ID, nil, http.StatusForbidden, nil)

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodDelete, "/v2/tasks/"+task.ID+"/console", nil, http.StatusOK, nil)
	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodGet, "/v2/tasks/"+task.ID, nil, http.StatusUnauthorized, nil)
}

func TestConsoleAPIStartsShellHarness(t *testing.T) {
	fixture := newTestFixture(t)

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/console", consoleRequest{
		Harness: flowharness.Shell,
	}, http.StatusCreated, &startedConsole)
	if startedConsole.Job == nil {
		t.Fatalf("started console response = %+v", startedConsole)
	}
	if got := payloadString(startedConsole.Job.Payload, "console_harness"); got != flowharness.Shell {
		t.Fatalf("console_harness = %q, want %q", got, flowharness.Shell)
	}
	entrypoint, ok := startedConsole.Job.Payload["entrypoint"].(map[string]any)
	if !ok {
		t.Fatalf("console entrypoint payload = %#v", startedConsole.Job.Payload["entrypoint"])
	}
	argv, ok := entrypoint["argv"].([]any)
	if !ok || len(argv) != 1 || argv[0] != `exec "${SHELL:-/bin/sh}"` {
		t.Fatalf("console entrypoint argv = %#v", entrypoint["argv"])
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim shell console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Harness != flowharness.Shell {
		t.Fatalf("running shell console session = %+v", running.Session)
	}
}

func TestConsoleTokenIsProjectConfined(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	projectA := bundles[0].Project
	projectB := bundles[1].Project
	if err := server.credentials.EnsureToken(context.Background(), coordinator.CredentialInput{
		Token:     "console-token",
		Scope:     coordinator.TokenScopeConsole,
		Subject:   "s-console",
		ProjectID: &projectA.ID,
	}); err != nil {
		t.Fatalf("store console token: %v", err)
	}

	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v2/projects/"+projectA.ID, nil, http.StatusOK, nil)
	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v2/projects/"+projectB.ID, nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v2/projects/"+projectB.ID+"/board", nil, http.StatusForbidden, nil)

	taskA, err := bundles[0].Tasks.CreateTask(context.Background(), coordinator.CreateTaskInput{Title: "Alpha task"})
	if err != nil {
		t.Fatalf("create alpha task: %v", err)
	}
	taskB, err := bundles[1].Tasks.CreateTask(context.Background(), coordinator.CreateTaskInput{Title: "Beta task"})
	if err != nil {
		t.Fatalf("create beta task: %v", err)
	}
	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v2/tasks/"+taskA.ID, nil, http.StatusOK, nil)
	doJSONRequestAs(t, server, "console-token", http.MethodGet, "/v2/tasks/"+taskB.ID, nil, http.StatusForbidden, nil)
}

func TestDiagnosticsDistinguishExpiredUnreleasedLeases(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Expired lease diagnostics"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	var enqueue jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/jobs", enqueueJobRequest{
		TaskID:         &task.ID,
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketPersistentAgent),
	}, http.StatusCreated, &enqueue)
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claim.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	var jobs jobsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/jobs", nil, http.StatusOK, &jobs)
	diagnostic := jobs.Diagnostics[enqueue.Job.ID]
	if diagnostic.Lease == nil || diagnostic.Lease.ReleasedAt != nil || diagnostic.LiveLease || diagnostic.LeaseStatus != "expired" {
		t.Fatalf("expired job diagnostics = %+v", diagnostic)
	}

	var workers workersResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/workers", nil, http.StatusOK, &workers)
	workerDiagnostic := workers.Diagnostics["w-local"]
	if workerDiagnostic.LiveJobs != 0 || workerDiagnostic.ExpiredUnreleasedJobs != 1 || workerDiagnostic.ExpiredUnreleasedPersistentAgent != 1 {
		t.Fatalf("expired worker diagnostics = %+v", workerDiagnostic)
	}
}

// TestJobsListAggregateOrdersByUpdatedAndStampsProject verifies the aggregate
// /v2/jobs response carries each job's project name in its diagnostics and is
// ordered globally by updated_at descending across projects (rather than
// concatenating per-project lists in registry order).
func TestJobsListAggregateOrdersByUpdatedAndStampsProject(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	projectA, projectB := bundles[0].Project, bundles[1].Project
	ctx := context.Background()

	// Enqueue one CI job in each project.
	var jobA jobResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodPost, "/v2/jobs?project="+projectA.ID, enqueueJobRequest{
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketEphemeral),
	}, http.StatusCreated, &jobA)
	var jobB jobResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodPost, "/v2/jobs?project="+projectB.ID, enqueueJobRequest{
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketEphemeral),
	}, http.StatusCreated, &jobB)

	// Force distinct updated_at timestamps: project B's job is the most recently
	// updated even though project A was registered first, so the global sort must
	// place it ahead of project A's job.
	now := time.Now().UTC()
	if _, err := bundles[0].Store.DB().ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, now.Add(-2*time.Hour).Format(time.RFC3339Nano), jobA.Job.ID); err != nil {
		t.Fatalf("stamp jobA updated_at: %v", err)
	}
	if _, err := bundles[1].Store.DB().ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, now.Add(-time.Hour).Format(time.RFC3339Nano), jobB.Job.ID); err != nil {
		t.Fatalf("stamp jobB updated_at: %v", err)
	}

	var list jobsResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v2/jobs", nil, http.StatusOK, &list)
	if len(list.Jobs) != 2 {
		t.Fatalf("jobs list = %+v, want 2 jobs", list.Jobs)
	}
	// Global updated_at desc: project B's job (newer) first, then project A's.
	if list.Jobs[0].ID != jobB.Job.ID || list.Jobs[1].ID != jobA.Job.ID {
		t.Fatalf("jobs order = %s, %s; want %s, %s (updated_at desc)", list.Jobs[0].ID, list.Jobs[1].ID, jobB.Job.ID, jobA.Job.ID)
	}

	// Each job's diagnostics carry its owning project's name and id.
	diagA := list.Diagnostics[jobA.Job.ID]
	if diagA.ProjectName != projectA.Name || diagA.ProjectID != projectA.ID {
		t.Fatalf("jobA project diagnostics = %+v, want project %s (%s)", diagA, projectA.Name, projectA.ID)
	}
	diagB := list.Diagnostics[jobB.Job.ID]
	if diagB.ProjectName != projectB.Name || diagB.ProjectID != projectB.ID {
		t.Fatalf("jobB project diagnostics = %+v, want project %s (%s)", diagB, projectB.Name, projectB.ID)
	}
}

func startRunningAuthorSession(t *testing.T, fixture testFixture, taskID string) jobResponse {
	t.Helper()
	ctx := context.Background()

	var scheduled workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+taskID+"/schedule", nil, http.StatusOK, &scheduled)
	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Role != flowworker.RoleAuthor {
		t.Fatalf("jobs after schedule = %+v", jobs)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.SessionToken == "" {
		t.Fatalf("running response missing session metadata: %+v", running)
	}
	return running
}

func TestSessionSignalRejectsInvalidSignal(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Invalid signal task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	running := startRunningAuthorSession(t, fixture, task.ID)

	doJSONRequestAs(t, fixture.Server, running.SessionToken, http.MethodPost, "/v2/sessions/"+running.Session.ID+"/signal", sessionSignalRequest{
		Signal: "finished",
	}, http.StatusBadRequest, nil)
}

func TestAttentionReplyRejectsForeignStatusLogID(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Attention reply task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	startRunningAuthorSession(t, fixture, task.ID)

	other, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Other task"})
	if err != nil {
		t.Fatalf("create other task: %v", err)
	}
	foreign, err := fixture.Status.Write(ctx, coordinator.WriteStatusInput{
		TaskID:  other.ID,
		Actor:   "agent",
		Kind:    coordinator.StatusKindQuestion,
		Message: "Foreign question",
	})
	if err != nil {
		t.Fatalf("write foreign status: %v", err)
	}

	var resp errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+task.ID+"/attention/reply", attentionReplyRequest{
		Message:     "My answer",
		StatusLogID: &foreign.ID,
	}, http.StatusBadRequest, &resp)
	if resp.Error.Code != "invalid_status_log_id" {
		t.Fatalf("error code = %q, want invalid_status_log_id", resp.Error.Code)
	}

	entries, err := fixture.Status.ListForTask(ctx, task.ID, 20)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Message, "Human response:") {
			t.Fatalf("found orphaned human response status entry on target task: %+v", entry)
		}
	}
}

// TestAttentionReplyRejectsNonExistentStatusLogID is the regression for the
// finding that a non-existent status_log_id tripped the message FK only after an
// orphaned status row had been written. Validation must now reject it with 400
// before any write.
func TestAttentionReplyRejectsNonExistentStatusLogID(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Attention reply task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	startRunningAuthorSession(t, fixture, task.ID)

	missing := int64(987654321)
	var resp errorResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+task.ID+"/attention/reply", attentionReplyRequest{
		Message:     "My answer",
		StatusLogID: &missing,
	}, http.StatusBadRequest, &resp)
	if resp.Error.Code != "invalid_status_log_id" {
		t.Fatalf("error code = %q, want invalid_status_log_id", resp.Error.Code)
	}

	entries, err := fixture.Status.ListForTask(ctx, task.ID, 20)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Message, "Human response:") {
			t.Fatalf("found status entry written before validation: %+v", entry)
		}
	}
}

// TestAttentionReplyLinksOwnTaskStatusLogID is the regression confirming that a
// valid status_log_id belonging to the task is accepted and threaded onto the
// queued session message.
func TestAttentionReplyLinksOwnTaskStatusLogID(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Attention reply task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	startRunningAuthorSession(t, fixture, task.ID)

	question, err := fixture.Status.Write(ctx, coordinator.WriteStatusInput{
		TaskID:  task.ID,
		Actor:   "agent",
		Kind:    coordinator.StatusKindQuestion,
		Message: "What database should I use?",
	})
	if err != nil {
		t.Fatalf("write question status: %v", err)
	}

	var resp sessionMessageResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+task.ID+"/attention/reply", attentionReplyRequest{
		Message:     "Use sqlite",
		StatusLogID: &question.ID,
	}, http.StatusOK, &resp)
	if !resp.Queued {
		t.Fatalf("reply queued = false, want true for live session")
	}
	if resp.Message.StatusLogID == nil || *resp.Message.StatusLogID != question.ID {
		t.Fatalf("message status_log_id = %v, want %d", resp.Message.StatusLogID, question.ID)
	}
}

func TestSessionStatusIsVisibleInTaskDetail(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Status task")

	var written statusResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "Running focused tests",
	}, http.StatusOK, &written)
	if written.Status.TaskID != started.Session.TaskID || written.Status.ChangeID != started.Session.ChangeID {
		t.Fatalf("written status = %+v, want task %s change %s", written.Status, started.Session.TaskID, started.Session.ChangeID)
	}

	var task taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+started.Session.TaskID, nil, http.StatusOK, &task)
	if len(task.StatusLog) != 1 || task.StatusLog[0].Message != "Running focused tests" {
		t.Fatalf("task status log = %+v", task.StatusLog)
	}
	if task.StatusLog[0].Kind != coordinator.StatusKindNote {
		t.Fatalf("default status kind = %q, want %q", task.StatusLog[0].Kind, coordinator.StatusKindNote)
	}
}

func TestSessionStatusAcceptsKind(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Status kind task")

	var written statusResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "Which datastore should I use?",
		Kind:    coordinator.StatusKindQuestion,
	}, http.StatusOK, &written)
	if written.Status.Kind != coordinator.StatusKindQuestion {
		t.Fatalf("written status kind = %q, want %q", written.Status.Kind, coordinator.StatusKindQuestion)
	}

	var task taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+started.Session.TaskID, nil, http.StatusOK, &task)
	if len(task.StatusLog) != 1 || task.StatusLog[0].Kind != coordinator.StatusKindQuestion {
		t.Fatalf("task status log = %+v", task.StatusLog)
	}
}

func TestSessionStatusRejectsBadKind(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Bad kind task")

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "boom",
		Kind:    "urgent",
	}, http.StatusBadRequest, nil)
}

// TestSessionStatusTouchesAgentActivity is the regression for agent-level
// liveness: writing a status entry is agent activity and must stamp
// last_agent_activity_at, which is nil before the first signal.
func TestSessionStatusTouchesAgentActivity(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Liveness status task")

	before, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session before status: %v", err)
	}
	if before.LastAgentActivityAt != nil {
		t.Fatalf("LastAgentActivityAt before status = %v, want nil", before.LastAgentActivityAt)
	}

	var written statusResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/sessions/"+started.Session.ID+"/status", sessionStatusRequest{
		Message: "Running focused tests",
	}, http.StatusOK, &written)

	after, err := fixture.Sessions.GetSession(ctx, started.Session.ID)
	if err != nil {
		t.Fatalf("get session after status: %v", err)
	}
	if after.LastAgentActivityAt == nil {
		t.Fatalf("LastAgentActivityAt after status = nil, want a timestamp")
	}
}

func TestSessionAttachRequiresOwnerToken(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Attach task")
	if _, err := fixture.Sessions.RegisterTerminal(context.Background(), started.Session.ID, "/tmp/flow-session.sock"); err != nil {
		t.Fatalf("register terminal target: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodGet, "/v2/sessions/"+started.Session.ID+"/attach", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/sessions/"+started.Session.ID+"/attach", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodGet, "/v2/sessions/"+started.Session.ID+"/attach", nil, http.StatusForbidden, nil)

	var response attachResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/sessions/"+started.Session.ID+"/attach", nil, http.StatusOK, &response)
	if response.Attach.SessionID != started.Session.ID || response.Attach.TmuxSession == "" {
		t.Fatalf("attach response = %+v", response.Attach)
	}
	if len(response.Attach.Command) != 6 || response.Attach.Command[0] != "tmux" || response.Attach.Command[1] != "-S" || response.Attach.Command[2] != "/tmp/flow-session.sock" || response.Attach.Command[5] != response.Attach.TmuxSession {
		t.Fatalf("attach command = %#v", response.Attach.Command)
	}
	if response.Attach.ProxyPath != "/v2/sessions/"+started.Session.ID+"/terminal" {
		t.Fatalf("proxy path = %q", response.Attach.ProxyPath)
	}
}

func TestJobAttachAllowsLiveReviewerJobs(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Reviewer attach task")
	reviewer := startLiveCheckJobForTask(t, fixture, "reviewer-token", "w-review-attach", started.Session.TaskID, started.Change.ID, "head-1", "reviewer", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	if _, err := fixture.Sessions.RegisterJobTerminal(context.Background(), reviewer.Job.ID, reviewer.Lease.ID, "/tmp/flow-job.sock"); err != nil {
		t.Fatalf("register job terminal target: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/jobs/"+reviewer.Job.ID+"/attach", nil, http.StatusForbidden, nil)

	var response attachResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/jobs/"+reviewer.Job.ID+"/attach", nil, http.StatusOK, &response)
	if response.Attach.SessionID != "" || response.Attach.JobID != reviewer.Job.ID || response.Attach.TmuxSession != "flow-"+reviewer.Job.ID {
		t.Fatalf("job attach response = %+v", response.Attach)
	}
	if len(response.Attach.Command) != 6 || response.Attach.Command[0] != "tmux" || response.Attach.Command[1] != "-S" || response.Attach.Command[2] != "/tmp/flow-job.sock" || response.Attach.Command[5] != response.Attach.TmuxSession {
		t.Fatalf("job attach command = %#v", response.Attach.Command)
	}

	if _, err := fixture.Workers.ReleaseLease(context.Background(), reviewer.Lease.ID, flowworker.JobFinished); err != nil {
		t.Fatalf("release reviewer lease: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/jobs/"+reviewer.Job.ID+"/attach", nil, http.StatusBadRequest, nil)
}

func TestSessionAttachRejectsInactiveOrNonLiveSessions(t *testing.T) {
	t.Run("finished", func(t *testing.T) {
		fixture := newTestFixture(t)
		started := startAuthorSessionForStatusTest(t, fixture, "Finished attach task")
		if _, err := fixture.Sessions.ReadyAuthorSession(context.Background(), started.Session.ID); err != nil {
			t.Fatalf("ready session: %v", err)
		}

		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/sessions/"+started.Session.ID+"/attach", nil, http.StatusBadRequest, nil)
	})

	t.Run("crashed", func(t *testing.T) {
		fixture := newTestFixture(t)
		started := startAuthorSessionForStatusTest(t, fixture, "Crashed attach task")
		if _, err := fixture.Sessions.UpdateSessionState(context.Background(), started.Session.ID, coordinator.SessionCrashed); err != nil {
			t.Fatalf("crash session: %v", err)
		}

		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/sessions/"+started.Session.ID+"/attach", nil, http.StatusBadRequest, nil)
	})

	t.Run("released lease", func(t *testing.T) {
		fixture := newTestFixture(t)
		started := startAuthorSessionForStatusTest(t, fixture, "Released lease attach task")
		if _, err := fixture.DB.ExecContext(context.Background(), `
UPDATE leases
SET released_at = ?
WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), started.Session.LeaseID); err != nil {
			t.Fatalf("release lease: %v", err)
		}

		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/sessions/"+started.Session.ID+"/attach", nil, http.StatusBadRequest, nil)
	})
}

func TestReviewThreadAPILifecycle(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Thread API task")

	var created threadResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+started.Change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/app.go",
		Line:            12,
		Context:         "return value",
		Body:            "Please handle nil.",
	}, http.StatusCreated, &created)
	if created.Thread.State != coordinator.ThreadOpen || len(created.Thread.Comments) != 1 {
		t.Fatalf("created thread = %+v", created.Thread)
	}

	var listed threadsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/changes/"+started.Change.ID+"/threads", nil, http.StatusOK, &listed)
	if len(listed.Threads) != 1 || listed.Threads[0].ID != created.Thread.ID {
		t.Fatalf("listed threads = %+v", listed.Threads)
	}

	var replied threadResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body: "I will fix it.",
	}, http.StatusOK, &replied)
	if len(replied.Thread.Comments) != 2 {
		t.Fatalf("replied thread = %+v", replied.Thread)
	}

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/threads/"+created.Thread.ID+"/claims", threadClaimRequest{
		Kind: string(coordinator.ClaimNotWarranted),
	}, http.StatusBadRequest, nil)

	var claimed threadResponse
	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/threads/"+created.Thread.ID+"/claims", threadClaimRequest{
		Kind:           string(coordinator.ClaimFixed),
		ClaimCommitSHA: "def456",
	}, http.StatusOK, &claimed)
	if claimed.Thread.State != coordinator.ThreadClaimed || claimed.Thread.ClaimCommitSHA == nil || *claimed.Thread.ClaimCommitSHA != "def456" {
		t.Fatalf("claimed thread = %+v", claimed.Thread)
	}

	verifier := startLiveWorkerJobForTask(t, fixture, "verifier-token", "w-verifier", started.Session.TaskID, started.Change.ID, flowworker.RoleVerifier)

	var certified threadResponse
	doJSONRequestAs(t, fixture.Server, "verifier-token", http.MethodPost, "/v2/threads/"+created.Thread.ID+"/certify", threadCommentRequest{
		Body:    "Verified.",
		LeaseID: verifier.Lease.ID,
	}, http.StatusOK, &certified)
	if certified.Thread.State != coordinator.ThreadCertified {
		t.Fatalf("certified thread = %+v", certified.Thread)
	}

	var reopened threadResponse
	doJSONRequestAs(t, fixture.Server, "verifier-token", http.MethodPost, "/v2/threads/"+created.Thread.ID+"/reopen", threadCommentRequest{
		Body:    "Still broken.",
		LeaseID: verifier.Lease.ID,
	}, http.StatusOK, &reopened)
	if reopened.Thread.State != coordinator.ThreadReopened {
		t.Fatalf("reopened thread = %+v", reopened.Thread)
	}
}

func TestReviewThreadAPIRestrictsSessionAndWorkerAccess(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Thread auth task")
	other := startAuthorSessionForStatusTestWithWorker(t, fixture, "Other thread auth task", "w-other-author")

	var created threadResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+started.Change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: "abc123",
		FilePath:        "internal/app.go",
		Line:            12,
		Body:            "Please handle nil.",
	}, http.StatusCreated, &created)

	doJSONRequestAs(t, fixture.Server, other.Token, http.MethodPost, "/v2/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body: "Cross-task reply.",
	}, http.StatusForbidden, nil)

	doJSONRequestAs(t, fixture.Server, started.Token, http.MethodPost, "/v2/threads/"+created.Thread.ID+"/certify", threadCommentRequest{
		Body: "Author cannot verify.",
	}, http.StatusForbidden, nil)

	reviewer := startLiveWorkerJobForTask(t, fixture, "reviewer-token", "w-reviewer", started.Session.TaskID, started.Change.ID, flowworker.RoleReviewer)
	doJSONRequestAs(t, fixture.Server, "reviewer-token", http.MethodPost, "/v2/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body:    "Reviewer reply.",
		LeaseID: reviewer.Lease.ID,
	}, http.StatusOK, nil)

	doJSONRequestAs(t, fixture.Server, "reviewer-token", http.MethodPost, "/v2/threads/"+created.Thread.ID+"/certify", threadCommentRequest{
		Body:    "Wrong role.",
		LeaseID: reviewer.Lease.ID,
	}, http.StatusForbidden, nil)

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body: "No live lease.",
	}, http.StatusForbidden, nil)
}

func TestWorkerCheckReportRejectsSourceJobFromStaleHead(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Stale check job task")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-2"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-ci",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &started.Session.TaskID,
		ChangeID:       &started.Change.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"check_name": "unit",
			"change_id":  started.Change.ID,
			"head_sha":   "head-1",
		},
	})
	if err != nil {
		t.Fatalf("enqueue stale job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-ci",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim stale job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	sourceJobID := job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/tasks/"+started.Session.TaskID+"/checks/unit", reportCheckRequest{
		Kind:        string(coordinator.CheckKindCI),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
		ExitCode:    intPointer(0),
	}, http.StatusForbidden, nil)
}

func TestWorkerCheckReportRejectsHeadAdvancedAfterScopeValidation(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Interleaved stale check job task")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-1"); err != nil {
		t.Fatalf("set initial change head: %v", err)
	}
	claimed := startLiveCheckJobForTask(
		t,
		fixture,
		"interleaved-ci-token",
		"w-interleaved-ci",
		started.Session.TaskID,
		started.Change.ID,
		"head-1",
		"unit",
		flowworker.RoleCI,
		flowworker.BucketEphemeral,
	)
	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	request := reportCheckRequest{
		Kind:        string(coordinator.CheckKindCI),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
		ExitCode:    intPointer(0),
	}
	principal := coordinator.Principal{Scope: coordinator.TokenScopeWorker, Subject: "w-interleaved-ci"}
	projectServer := fixture.Server.forBundle(fixture.Bundle)
	if err := projectServer.checkReportScope(
		httptest.NewRequest(http.MethodPost, "/v2/tasks/"+started.Session.TaskID+"/checks/unit", nil),
		started.Session.TaskID,
		"unit",
		request,
		principal,
	); err != nil {
		t.Fatalf("validate worker check scope: %v", err)
	}

	// Reproduce the handler interleaving: the job passed scope validation for
	// head-1, then a new revision became current before ReportCheck wrote.
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-2"); err != nil {
		t.Fatalf("advance change head after scope validation: %v", err)
	}
	required := true
	if _, err := fixture.Checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		TaskID:   started.Session.TaskID,
		Name:     "unit",
		Kind:     coordinator.CheckKindCI,
		Required: &required,
		Verdict:  coordinator.CheckPending,
	}); err != nil {
		t.Fatalf("seed reset pending check: %v", err)
	}

	_, err := fixture.Checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		TaskID:        started.Session.TaskID,
		Name:          "unit",
		Kind:          coordinator.CheckKindCI,
		ExitCode:      intPointer(0),
		SourceJobID:   &sourceJobID,
		Reporter:      "worker:w-interleaved-ci",
		WorkerID:      principal.Subject,
		WorkerLeaseID: leaseID,
	})
	if !errors.Is(err, coordinator.ErrCheckReportLeaseInvalid) {
		t.Fatalf("stale worker report error = %v, want atomic authorization rejection", err)
	}
	check, err := fixture.Checks.GetCheck(ctx, started.Session.TaskID, "unit")
	if err != nil {
		t.Fatalf("get check after stale report: %v", err)
	}
	if check.Verdict != coordinator.CheckPending || check.SourceJobID != nil {
		t.Fatalf("check after stale report = %+v, want pending check without stale source job", check)
	}
}

func TestWorkerCheckReportRejectsSourceJobMissingCheckMetadata(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Missing check metadata task")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-1"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-ci-metadata",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &started.Session.TaskID,
		ChangeID:       &started.Change.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue job without metadata: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-ci-metadata",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	sourceJobID := job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/tasks/"+started.Session.TaskID+"/checks/unit", reportCheckRequest{
		Kind:        string(coordinator.CheckKindCI),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
		ExitCode:    intPointer(0),
	}, http.StatusForbidden, nil)
}

func TestWorkflowAuthorChangeUsesTaskAlignedID(t *testing.T) {
	fixture := newTestFixture(t)
	started := startAuthorSessionForStatusTest(t, fixture, "Friendly workflow change ID")
	want := "ch-" + strings.TrimPrefix(started.Session.TaskID, "t-")
	if started.Change.ID != want {
		t.Fatalf("workflow change ID = %q, want %q", started.Change.ID, want)
	}
}

func TestWorkerReviewerJobCanReportReviewerCheck(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Reviewer report task")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-1"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	reviewer := startLiveCheckJobForTask(t, fixture, "reviewer-token", "w-review-report", started.Session.TaskID, started.Change.ID, "head-1", "reviewer", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	sourceJobID := reviewer.Job.ID
	leaseID := reviewer.Lease.ID
	var response checkResponse
	doJSONRequestAs(t, fixture.Server, "reviewer-token", http.MethodPost, "/v2/tasks/"+started.Session.TaskID+"/checks/reviewer", reportCheckRequest{
		Kind:        string(coordinator.CheckKindReviewer),
		Verdict:     string(coordinator.CheckSatisfied),
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
		ExitCode:    intPointer(0),
	}, http.StatusOK, &response)
	if response.Check.Kind != coordinator.CheckKindReviewer || response.Check.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("reviewer report response = %+v", response)
	}
}

func TestAdvisoryReviewerReportIsVisibleButCannotCreateBlockingThread(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Advisory reviewer task")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-1"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	advisoryJob := startLiveCheckJobForTask(t, fixture, "advisory-token", "w-advisory", started.Session.TaskID, started.Change.ID, "head-1", "performance-review", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	if _, err := fixture.Store.DB().ExecContext(ctx, `
UPDATE jobs SET payload_json = json_set(payload_json, '$.blocking', json('false')) WHERE id = ?`, advisoryJob.Job.ID); err != nil {
		t.Fatalf("mark reviewer job advisory: %v", err)
	}

	advisory := false
	blocking := true
	sourceJobID := advisoryJob.Job.ID
	leaseID := advisoryJob.Lease.ID
	reportPath := "/v2/tasks/" + started.Session.TaskID + "/checks/performance-review"
	doJSONRequestAs(t, fixture.Server, "advisory-token", http.MethodPost, reportPath, reportCheckRequest{
		Kind: string(coordinator.CheckKindReviewer), Required: &blocking,
		Verdict: string(coordinator.CheckBlocked), Details: "Advisory (non-blocking): consider caching.",
		SourceJobID: &sourceJobID, LeaseID: &leaseID,
	}, http.StatusForbidden, nil)
	var reported checkResponse
	doJSONRequestAs(t, fixture.Server, "advisory-token", http.MethodPost, reportPath, reportCheckRequest{
		Kind: string(coordinator.CheckKindReviewer), Required: &advisory,
		Verdict: string(coordinator.CheckBlocked), Details: "Advisory (non-blocking): consider caching.",
		SourceJobID: &sourceJobID, LeaseID: &leaseID,
	}, http.StatusOK, &reported)
	if reported.Check.Required || reported.Check.Verdict != coordinator.CheckBlocked || !strings.Contains(reported.Check.Details, "Advisory (non-blocking)") {
		t.Fatalf("advisory check response = %+v", reported.Check)
	}

	doJSONRequestAs(t, fixture.Server, "advisory-token", http.MethodPost, "/v2/changes/"+started.Change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: "head-1", FilePath: "cache.go", Line: 12,
		Body: "Consider caching this lookup.", LeaseID: advisoryJob.Lease.ID,
	}, http.StatusForbidden, nil)

	blockingJob := startLiveCheckJobForTask(t, fixture, "blocking-token", "w-blocking-review", started.Session.TaskID, started.Change.ID, "head-1", "security-review", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	var created threadResponse
	doJSONRequestAs(t, fixture.Server, "blocking-token", http.MethodPost, "/v2/changes/"+started.Change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: "head-1", FilePath: "auth.go", Line: 7,
		Body: "This authorization check is required.", LeaseID: blockingJob.Lease.ID,
	}, http.StatusCreated, &created)
	if created.Thread.ID == "" || created.Thread.State != coordinator.ThreadOpen {
		t.Fatalf("blocking reviewer thread = %+v", created.Thread)
	}
	discoveryJob := startLiveCheckJobForTask(t, fixture, "discovery-token", "w-discovery-review", started.Session.TaskID, started.Change.ID, "head-1", "parallel-discovery", flowworker.RoleReviewer, flowworker.BucketPersistentAgent)
	if _, err := fixture.Store.DB().ExecContext(ctx, `
UPDATE jobs
SET payload_json = json_set(payload_json, '$.blocking', json('true'), '$.review_discovery', json('true'))
WHERE id = ?`, discoveryJob.Job.ID); err != nil {
		t.Fatalf("mark reviewer job as discovery: %v", err)
	}
	doJSONRequestAs(t, fixture.Server, "discovery-token", http.MethodPost, "/v2/changes/"+started.Change.ID+"/comments", createThreadRequest{
		AnchorCommitSHA: "head-1", FilePath: "auth.go", Line: 8,
		Body: "Discovery must wait for aggregation.", LeaseID: discoveryJob.Lease.ID,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "advisory-token", http.MethodPost, "/v2/threads/"+created.Thread.ID+"/comments", threadCommentRequest{
		Body: "Advisory jobs cannot reply.", LeaseID: advisoryJob.Lease.ID,
	}, http.StatusForbidden, nil)

	advisoryVerifier := startLiveCheckJobForTask(t, fixture, "advisory-verifier-token", "w-advisory-verifier", started.Session.TaskID, started.Change.ID, "head-1", "verification", flowworker.RoleVerifier, flowworker.BucketPersistentAgent)
	if _, err := fixture.Store.DB().ExecContext(ctx, `
UPDATE jobs SET payload_json = json_set(payload_json, '$.blocking', json('false')) WHERE id = ?`, advisoryVerifier.Job.ID); err != nil {
		t.Fatalf("mark verifier job advisory: %v", err)
	}
	for _, action := range []string{"certify", "reopen"} {
		doJSONRequestAs(t, fixture.Server, "advisory-verifier-token", http.MethodPost, "/v2/threads/"+created.Thread.ID+"/"+action, threadCommentRequest{
			Body: "Advisory jobs cannot " + action + ".", LeaseID: advisoryVerifier.Lease.ID,
		}, http.StatusForbidden, nil)
	}
}

func TestReviewAggregationLeaseCreatesOrIdentifiesFollowUpTasks(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Review follow-up source")
	if _, err := fixture.Sessions.UpdateChangeHead(ctx, started.Change.ID, "head-1"); err != nil {
		t.Fatalf("update change head: %v", err)
	}
	existing, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Existing metrics cleanup"})
	if err != nil {
		t.Fatalf("create existing task: %v", err)
	}
	aggregation := startLiveCheckJobForTask(
		t,
		fixture,
		"aggregation-token",
		"w-aggregation",
		started.Session.TaskID,
		started.Change.ID,
		"head-1",
		coordinator.ReviewAggregationCheckName+".node.nr-api",
		flowworker.RoleReviewer,
		flowworker.BucketPersistentAgent,
	)
	if _, err := fixture.Store.DB().ExecContext(ctx, `
UPDATE jobs
SET payload_json = json_set(payload_json, '$.review_aggregation', json('true'), '$.blocking', json('false'))
WHERE id = ?`, aggregation.Job.ID); err != nil {
		t.Fatalf("mark aggregation job: %v", err)
	}

	path := "/v2/tasks/" + started.Session.TaskID + "/review-follow-ups"
	createRequest := contract.ApplyReviewFollowUpRequest{
		LeaseID: aggregation.Lease.ID,
		Finding: contract.ReviewFollowUpFinding{
			SHA: "head-1", File: "internal/cache.go", Line: 42,
			Body: "The legacy cache has no size bound.", Severity: "high",
			IntroducedByChange: false, Requirement: "cache memory remains bounded",
		},
		TaskAction: contract.ReviewFollowUpTaskAction{
			Action: "create_task",
			Title:  "Bound the legacy cache",
			Body:   "Add a configurable cache bound and tests covering eviction.",
		},
	}
	var created contract.ApplyReviewFollowUpResponse
	doJSONRequestAs(t, fixture.Server, "aggregation-token", http.MethodPost, path, createRequest, http.StatusOK, &created)
	if created.Disposition != "created" || created.Task.ID == "" ||
		created.Task.SourceTaskID == nil || *created.Task.SourceTaskID != started.Session.TaskID ||
		created.Task.SourceChangeID == nil || *created.Task.SourceChangeID != started.Change.ID {
		t.Fatalf("created review follow-up = %+v", created)
	}
	var replayed contract.ApplyReviewFollowUpResponse
	doJSONRequestAs(t, fixture.Server, "aggregation-token", http.MethodPost, path, createRequest, http.StatusOK, &replayed)
	if replayed.Task.ID != created.Task.ID || replayed.Disposition != "created" {
		t.Fatalf("replayed review follow-up = %+v, want %s", replayed, created.Task.ID)
	}

	var reused contract.ApplyReviewFollowUpResponse
	doJSONRequestAs(t, fixture.Server, "aggregation-token", http.MethodPost, path, contract.ApplyReviewFollowUpRequest{
		LeaseID: aggregation.Lease.ID,
		Finding: contract.ReviewFollowUpFinding{
			SHA: "head-1", File: "internal/metrics.go", Line: 7,
			Body: "Metric naming is inconsistent.", Severity: "low",
			IntroducedByChange: true, Requirement: "metric names remain stable",
		},
		TaskAction: contract.ReviewFollowUpTaskAction{
			Action: "use_existing_task",
			TaskID: existing.ID,
		},
	}, http.StatusOK, &reused)
	if reused.Disposition != "existing" || reused.Task.ID != existing.ID {
		t.Fatalf("reused review follow-up = %+v", reused)
	}

	relations, err := fixture.Tasks.RelationsForTask(ctx, started.Session.TaskID)
	if err != nil {
		t.Fatalf("list review follow-up relations: %v", err)
	}
	if len(relations) != 2 {
		t.Fatalf("review follow-up relations = %+v, want two", relations)
	}

	ordinary := startLiveCheckJobForTask(
		t,
		fixture,
		"ordinary-reviewer-token",
		"w-ordinary-reviewer",
		started.Session.TaskID,
		started.Change.ID,
		"head-1",
		"ordinary-reviewer",
		flowworker.RoleReviewer,
		flowworker.BucketPersistentAgent,
	)
	createRequest.LeaseID = ordinary.Lease.ID
	doJSONRequestAs(t, fixture.Server, "ordinary-reviewer-token", http.MethodPost, path, createRequest, http.StatusForbidden, nil)
}

func startAuthorSessionForStatusTest(t *testing.T, fixture testFixture, title string) coordinator.StartAuthorSessionResult {
	return startAuthorSessionForStatusTestWithWorker(t, fixture, title, "w-local")
}

func startAuthorSessionForStatusTestWithWorker(t *testing.T, fixture testFixture, title string, workerID string) coordinator.StartAuthorSessionResult {
	t.Helper()

	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: title})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := fixture.Bundle.WorkflowRuns.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule workflow: %v", err)
	}
	if err := fixture.Bundle.WorkflowExecutor.Advance(ctx, run.ID); err != nil {
		t.Fatalf("advance workflow: %v", err)
	}
	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list author jobs: %v", err)
	}
	var authorJob flowworker.Job
	for _, job := range jobs {
		if job.TaskID != nil && *job.TaskID == task.ID && job.Role == flowworker.RoleAuthor {
			authorJob = job
			break
		}
	}
	if authorJob.ID == "" {
		t.Fatal("workflow did not enqueue an author job")
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	claimed := claimSpecificJob(t, fixture, workerID, authorJob.ID, []flowworker.CapacityBucket{flowworker.BucketPersistentAgent})
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if claimed.Job.NodeRunID != nil {
		if _, err := fixture.Bundle.WorkflowRuns.MarkNodeRunning(ctx, *claimed.Job.NodeRunID); err != nil {
			t.Fatalf("mark workflow node running: %v", err)
		}
	}
	started, err := fixture.Sessions.StartAuthorSession(ctx, coordinator.StartAuthorSessionInput{
		JobID:    claimed.Job.ID,
		LeaseID:  claimed.Lease.ID,
		WorkerID: workerID,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	return started
}

func liveAuthorJobsForTask(t *testing.T, fixture testFixture, taskID string) []flowworker.Job {
	t.Helper()

	jobs, err := fixture.Workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var live []flowworker.Job
	for _, job := range jobs {
		if job.TaskID == nil || *job.TaskID != taskID || job.Role != flowworker.RoleAuthor {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			live = append(live, job)
		}
	}
	return live
}

func startLiveWorkerJobForTask(t *testing.T, fixture testFixture, token string, workerID string, taskID string, changeID string, role flowworker.JobRole) flowworker.ClaimedJob {
	t.Helper()

	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   token,
		Scope:   coordinator.TokenScopeWorker,
		Subject: workerID,
	}); err != nil {
		t.Fatalf("store worker token: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true", "worker_id": workerID},
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker %s: %v", workerID, err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &taskID,
		ChangeID:       &changeID,
		Role:           role,
		CapacityBucket: flowworker.BucketPersistentAgent,
		RunsOn:         map[string]string{"worker_id": workerID},
	})
	if err != nil {
		t.Fatalf("enqueue %s job: %v", role, err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      workerID,
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim %s job: %v", role, err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	running, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("mark %s running: %v", role, err)
	}
	claimed.Job = running
	return claimed
}

func createCheckConfigExchange(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	runAPIGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "README.md", "initial\n")
	runAPIGit(t, repoPath, "add", "README.md")
	runAPIGit(t, repoPath, "commit", "-m", "initial")
	runAPIGit(t, "", "init", "--bare", exchangePath)
	runAPIGit(t, repoPath, "checkout", "-b", "task/t-api-0001", "main")
	writeAPIFile(t, repoPath, ".flow/checks/unit.yaml", `
name: unit
kind: ci
entrypoint:
  argv: ["go", "test", "./..."]
`)
	writeAPIFile(t, repoPath, ".flow/checks/reviewer.yaml", `
name: reviewer
kind: reviewer
entrypoint:
  argv: ['harness -p "$(flow fetch-prompt --harness harness)"']
  shell: true
requires: ["agent.harness.harness"]
`)
	writeAPIFile(t, repoPath, ".flow/checks/verifier.yaml", `
name: verifier
kind: verifier
entrypoint:
  argv: ['harness -p "$(flow fetch-prompt --harness harness)"']
  shell: true
requires: ["agent.harness.harness"]
`)
	runAPIGit(t, repoPath, "add", ".flow/checks")
	runAPIGit(t, repoPath, "commit", "-m", "add checks")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, "task/t-api-0001:task/t-api-0001")

	return exchangePath, headSHA
}

func createInvalidCheckConfigExchange(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	exchangePath := filepath.Join(root, "exchange.git")
	runAPIGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "README.md", "initial\n")
	runAPIGit(t, repoPath, "add", "README.md")
	runAPIGit(t, repoPath, "commit", "-m", "initial")
	runAPIGit(t, "", "init", "--bare", exchangePath)
	runAPIGit(t, repoPath, "checkout", "-b", "task/t-api-0001", "main")
	writeAPIFile(t, repoPath, ".flow/checks/bad.yaml", `
name: bad
kind: ci
`)
	runAPIGit(t, repoPath, "add", ".flow/checks")
	runAPIGit(t, repoPath, "commit", "-m", "add bad check")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, repoPath, "push", exchangePath, "task/t-api-0001:task/t-api-0001")

	return exchangePath, headSHA
}

func repointFixtureExchange(t *testing.T, fixture testFixture, exchangePath string) {
	t.Helper()

	bundle := fixture.Bundle
	project := bundle.Project
	project.ExchangePath = exchangePath
	bundle.Project = project

	// Keep the global registry row in sync so the by-exchange drain route can
	// resolve this project from its exchange path.
	if _, err := fixture.GlobalDB.ExecContext(context.Background(), `
UPDATE projects
SET exchange_path = ?
WHERE id = ?`, exchangePath, project.ID); err != nil {
		t.Fatalf("repoint project exchange row: %v", err)
	}

	db := bundle.Store.DB()
	bundle.Merges = coordinator.NewMergeService(db, bundle.Tasks, bundle.Sessions, project)
	bundle.CheckConfigs = coordinator.NewCheckConfigServiceWithOptions(db, bundle.Checks, bundle.Queue, bundle.Threads, project, coordinator.CheckConfigServiceOptions{})
	bundle.GitEventConsumer = coordinator.NewGitEventConsumer(db, project)
}

func startLiveCheckJobForTask(t *testing.T, fixture testFixture, token string, workerID string, taskID string, changeID string, headSHA string, checkName string, role flowworker.JobRole, bucket flowworker.CapacityBucket) flowworker.ClaimedJob {
	t.Helper()

	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   token,
		Scope:   coordinator.TokenScopeWorker,
		Subject: workerID,
	}); err != nil {
		t.Fatalf("store worker token: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      workerID,
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true", "worker_id": workerID},
		CapacityPersistentAgent: 1,
		CapacityEphemeral:       1,
	}); err != nil {
		t.Fatalf("register worker %s: %v", workerID, err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &taskID,
		ChangeID:       &changeID,
		Role:           role,
		CapacityBucket: bucket,
		RunsOn:         map[string]string{"worker_id": workerID},
		Payload: map[string]any{
			"check_name": checkName,
			"change_id":  changeID,
			"head_sha":   headSHA,
		},
	})
	if err != nil {
		t.Fatalf("enqueue %s check job: %v", role, err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      workerID,
		Buckets:       []flowworker.CapacityBucket{bucket},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim %s check job: %v", role, err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed = %+v ok=%t, want %s", claimed.Job, ok, job.ID)
	}
	running, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID)
	if err != nil {
		t.Fatalf("mark %s check running: %v", role, err)
	}
	claimed.Job = running

	return claimed
}

func claimSpecificJob(t *testing.T, fixture testFixture, workerID string, jobID string, buckets []flowworker.CapacityBucket) flowworker.ClaimedJob {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 20; attempt++ {
		claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
			WorkerID:      workerID,
			Buckets:       buckets,
			LeaseDuration: time.Minute,
		})
		if err != nil {
			t.Fatalf("claim job %s: %v", jobID, err)
		}
		if !ok {
			t.Fatalf("claim job %s: no matching job after %d attempts", jobID, attempt+1)
		}
		if claimed.Job.ID == jobID {
			return claimed
		}
		if _, err := fixture.Workers.ReleaseLease(ctx, claimed.Lease.ID, flowworker.JobCanceled); err != nil {
			t.Fatalf("cancel unrelated claimed job %s while looking for %s: %v", claimed.Job.ID, jobID, err)
		}
	}
	t.Fatalf("job %s was not claimable", jobID)
	return flowworker.ClaimedJob{}
}

func intPointer(value int) *int {
	return &value
}

func satisfyAPICheck(t *testing.T, fixture testFixture, taskID string, name string, kind coordinator.CheckKind) checkResponse {
	t.Helper()
	required := true
	var response checkResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+taskID+"/checks/"+name, reportCheckRequest{
		Kind:     string(kind),
		Required: &required,
		Verdict:  string(coordinator.CheckSatisfied),
	}, http.StatusOK, &response)
	return response
}

func assertAPICheck(t *testing.T, fixture testFixture, taskID string, name string, kind coordinator.CheckKind, verdict coordinator.CheckVerdict) {
	t.Helper()
	check, err := fixture.Checks.GetCheck(context.Background(), taskID, name)
	if err != nil {
		t.Fatalf("get check %s: %v", name, err)
	}
	if check.Kind != kind || check.Verdict != verdict {
		t.Fatalf("check %s = %+v", name, check)
	}
}

func assertAPILiveJobs(t *testing.T, fixture testFixture, taskID string, want map[flowworker.JobRole]int) {
	t.Helper()
	jobs, err := fixture.Workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	counts := map[flowworker.JobRole]int{}
	for _, job := range jobs {
		if job.TaskID == nil || *job.TaskID != taskID {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			counts[job.Role]++
		}
	}
	for role, expected := range want {
		if counts[role] != expected {
			t.Fatalf("live %s jobs = %d, want %d; all counts=%+v", role, counts[role], expected, counts)
		}
	}
}

func writeAPIFile(t *testing.T, repoPath string, relativePath string, contents string) {
	t.Helper()
	path := filepath.Join(repoPath, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relativePath, err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", relativePath, err)
	}
}

func runAPIGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = apiGitOutput(t, dir, args...)
}

func apiGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}

	return strings.TrimSpace(string(output))
}

func TestWorkerEndpointsRequireWorkerScopeAndLeaseOwnership(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token:   "other-worker-token",
		Scope:   coordinator.TokenScopeWorker,
		Subject: "w-other",
	}); err != nil {
		t.Fatalf("store other worker token: %v", err)
	}

	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/workers/register", registerWorkerRequest{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/register", registerWorkerRequest{
		ID:                      "w-other",
		CapacityPersistentAgent: 1,
	}, http.StatusForbidden, nil)

	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Lease ownership task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if _, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok {
		t.Fatal("claim ok=false")
	}

	doJSONRequestAs(t, fixture.Server, "other-worker-token", http.MethodPost, "/v2/workers/renew", renewLeaseRequest{
		LeaseID:              claimed.Lease.ID,
		LeaseDurationSeconds: 60,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "other-worker-token", http.MethodPost, "/v2/workers/release", releaseLeaseRequest{
		LeaseID:    claimed.Lease.ID,
		FinalState: string(flowworker.JobFinished),
	}, http.StatusForbidden, nil)
}

func TestJobEnqueueIdempotencyReplaysCreatedJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Idempotent job task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	request := enqueueJobRequest{
		TaskID:         &task.ID,
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketEphemeral),
		Priority:       3,
	}

	var first jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/jobs", request, http.StatusCreated, &first, idempotencyHeader, "job-key")
	var second jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/jobs", request, http.StatusCreated, &second, idempotencyHeader, "job-key")
	if first.Job.ID == "" || second.Job.ID != first.Job.ID {
		t.Fatalf("idempotent enqueue returned jobs %q then %q", first.Job.ID, second.Job.ID)
	}

	jobs, err := fixture.Workers.ListJobs(ctx)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("job count = %d, want 1: %+v", len(jobs), jobs)
	}
}

func TestWorkerCheckReportingRejectsExpiredLease(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Expired check lease task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue check job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim check job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed check job = %+v, ok=%t; want %s", claimed.Job, ok, job.ID)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark job running: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE leases
SET expires_at = ?
WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claimed.Lease.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	exitFailure := 1
	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/tasks/"+task.ID+"/checks/fake-ci", reportCheckRequest{
		ExitCode:    &exitFailure,
		Details:     "exit status 1",
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	}, http.StatusForbidden, nil)

	swept, err := fixture.Workers.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get swept job: %v", err)
	}
	if swept.State != flowworker.JobCrashed {
		t.Fatalf("expired job state = %q, want crashed", swept.State)
	}
}

func TestWorkerCheckReportingRejectsNonCISourceJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Non-CI check lease task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                      "w-local",
		CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	job, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID:         &task.ID,
		Role:           flowworker.RoleAuthor,
		CapacityBucket: flowworker.BucketPersistentAgent,
	})
	if err != nil {
		t.Fatalf("enqueue author job: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim author job: %v", err)
	}
	if !ok || claimed.Job.ID != job.ID {
		t.Fatalf("claimed author job = %+v, ok=%t; want %s", claimed.Job, ok, job.ID)
	}

	exitZero := 0
	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/tasks/"+task.ID+"/checks/fake-ci", reportCheckRequest{
		ExitCode:    &exitZero,
		Details:     "exit status 0",
		SourceJobID: &sourceJobID,
		LeaseID:     &leaseID,
	}, http.StatusForbidden, nil)
}

func TestWorkerClaimCanWaitForJob(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	enqueued := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
			Role:           flowworker.RoleCI,
			CapacityBucket: flowworker.BucketEphemeral,
		})
		enqueued <- err
	}()

	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDurationSeconds: 60,
		WaitSeconds:          1,
	}, http.StatusOK, &claim)
	if err := <-enqueued; err != nil {
		t.Fatalf("enqueue delayed job: %v", err)
	}
	if !claim.Claimed || claim.Job == nil || claim.Job.Role != flowworker.RoleCI || claim.Lease == nil {
		t.Fatalf("claim response = %+v", claim)
	}
}

func TestWorkerClaimSweepsExpiredLeases(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID:                "w-local",
		CapacityEphemeral: 1,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	expiredJob, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       100,
	})
	if err != nil {
		t.Fatalf("enqueue expired job: %v", err)
	}
	now := time.Now().UTC()
	if _, err := fixture.DB.ExecContext(ctx, `
UPDATE jobs
SET state = ?
WHERE id = ?`, string(flowworker.JobClaimed), expiredJob.ID); err != nil {
		t.Fatalf("mark expired job claimed: %v", err)
	}
	if _, err := fixture.DB.ExecContext(ctx, `
INSERT INTO leases (
	id,
	job_id,
	worker_id,
	capacity_bucket,
	leased_at,
	expires_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		"l-expired",
		expiredJob.ID,
		"w-local",
		string(flowworker.BucketEphemeral),
		now.Add(-2*time.Minute).Format(time.RFC3339Nano),
		now.Add(-time.Minute).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert expired lease: %v", err)
	}
	queuedJob, err := fixture.Workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Priority:       1,
	})
	if err != nil {
		t.Fatalf("enqueue queued job: %v", err)
	}

	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketEphemeral},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != queuedJob.ID {
		t.Fatalf("claim response = %+v, want queued job %s", claim, queuedJob.ID)
	}

	swept, err := fixture.Workers.GetJob(ctx, expiredJob.ID)
	if err != nil {
		t.Fatalf("get swept job: %v", err)
	}
	if swept.State != flowworker.JobCrashed {
		t.Fatalf("expired job state = %q, want crashed", swept.State)
	}
}

func newTestServer(t *testing.T) *Server {
	return newTestFixture(t).Server
}

// newTestRegistryInDir builds a registry rooted at dataDir so callers (e.g. the
// coordinator-restart test) can reopen the same data directory. It also returns
// the dataDir and the global store handle (tokens and web sessions live there).
func newTestRegistryInDir(t *testing.T, dataDir string, projectNames ...string) (*Registry, []*ProjectBundle, string, *flowdb.Store) {
	t.Helper()
	ctx := context.Background()

	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })

	registry, err := NewRegistry(RegistryOptions{DataDir: dataDir, Global: global})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	bundles := make([]*ProjectBundle, 0, len(projectNames))
	for _, name := range projectNames {
		project, err := registry.CreateProject(ctx, coordinator.Project{Name: name, BaseBranch: "main"})
		if err != nil {
			t.Fatalf("create project %q: %v", name, err)
		}
		bundle, ok := registry.Bundle(project.ID)
		if !ok {
			t.Fatalf("bundle for project %q not found after create", name)
		}
		bundles = append(bundles, bundle)
	}

	return registry, bundles, dataDir, global
}

// reopenTestRegistry reopens an existing data directory, simulating a
// coordinator restart: it opens every persisted project bundle from disk.
func reopenTestRegistry(t *testing.T, dataDir string) (*Registry, []*ProjectBundle, *flowdb.Store) {
	t.Helper()
	ctx := context.Background()

	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("reopen global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })

	registry, err := NewRegistry(RegistryOptions{DataDir: dataDir, Global: global})
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	if err := registry.OpenAll(ctx); err != nil {
		t.Fatalf("open all projects: %v", err)
	}

	return registry, registry.All(), global
}

// testWorkers wraps a project's queue service with the worker-directory and
// cross-project claim operations the registry now owns, so existing tests can
// keep calling RegisterWorker/ClaimNextJob on the fixture's "Workers".
type testWorkers struct {
	*flowworker.Service
	registry *Registry
}

func (w testWorkers) RegisterWorker(ctx context.Context, input flowworker.RegisterWorkerInput) (flowworker.Worker, error) {
	return w.registry.Directory().RegisterWorker(ctx, input)
}

func (w testWorkers) ClaimNextJob(ctx context.Context, input flowworker.ClaimInput) (flowworker.ClaimedJob, bool, error) {
	claim, ok, err := w.registry.Claim(ctx, input)
	if err != nil || !ok {
		return flowworker.ClaimedJob{}, ok, err
	}
	return flowworker.ClaimedJob{Job: claim.Job, Lease: claim.Lease}, true, nil
}

type testFixture struct {
	Registry    *Registry
	Bundle      *ProjectBundle
	Project     coordinator.Project
	DataDir     string
	Store       *flowdb.Store
	GlobalStore *flowdb.Store
	Server      *Server
	DB          *sql.DB
	GlobalDB    *sql.DB
	Tasks       *coordinator.TaskService
	Checks      *coordinator.CheckService
	Credentials *coordinator.CredentialService
	GitEvents   *coordinator.GitEventService
	Workers     testWorkers
	Sessions    *coordinator.SessionService
	Status      *coordinator.StatusService
	Reconciler  *coordinator.ReconcileService
	Threads     *coordinator.ThreadService
	CheckConfig *coordinator.CheckConfigService
	Merges      *coordinator.MergeService
	WebSessions *coordinator.WebSessionService
}

func newTestFixture(t *testing.T) testFixture {
	t.Helper()
	registry, bundles, dataDir, global := newTestRegistryInDir(t, t.TempDir(), "api")
	return fixtureFromRegistry(t, registry, bundles[0], dataDir, global, true)
}

// boardPath returns the project-scoped board response; the unscoped route
// returns an aggregate across projects.
func (f testFixture) boardPath() string {
	return "/v2/projects/" + f.Project.ID + "/board"
}

// gitEventsPath returns the project-scoped hook git-events route.
func (f testFixture) gitEventsPath() string {
	return "/v2/projects/" + f.Project.ID + "/git/events"
}

// reopenTestFixture rebuilds a fixture from an existing data directory without
// re-minting tokens (they persist in the global database).
func reopenTestFixture(t *testing.T, dataDir string) testFixture {
	t.Helper()
	registry, bundles, global := reopenTestRegistry(t, dataDir)
	if len(bundles) == 0 {
		t.Fatalf("reopened registry has no project bundles")
	}
	return fixtureFromRegistry(t, registry, bundles[0], dataDir, global, false)
}

// fixtureFromRegistry wires a testFixture from a registry and its primary
// bundle. When mintTokens is set it seeds the standard owner/hook/worker
// tokens; reopened fixtures reuse the persisted ones.
func fixtureFromRegistry(t *testing.T, registry *Registry, bundle *ProjectBundle, dataDir string, global *flowdb.Store, mintTokens bool) testFixture {
	t.Helper()

	credentials := registry.Credentials()
	ctx := context.Background()
	if mintTokens {
		if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
			Token: "owner-token",
			Scope: coordinator.TokenScopeOwner,
		}); err != nil {
			t.Fatalf("store owner token: %v", err)
		}
		if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
			Token: "hook-token",
			Scope: coordinator.TokenScopeHook,
		}); err != nil {
			t.Fatalf("store hook token: %v", err)
		}
		if err := credentials.EnsureToken(ctx, coordinator.CredentialInput{
			Token:   "worker-token",
			Scope:   coordinator.TokenScopeWorker,
			Subject: "w-local",
		}); err != nil {
			t.Fatalf("store worker token: %v", err)
		}
	}

	server, err := NewServer(ServerOptions{
		Registry:   registry,
		OwnerToken: "owner-token",
		HookToken:  "hook-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	return testFixture{
		Registry:    registry,
		Bundle:      bundle,
		Project:     bundle.Project,
		DataDir:     dataDir,
		Store:       bundle.Store,
		GlobalStore: global,
		Server:      server,
		DB:          bundle.Store.DB(),
		GlobalDB:    global.DB(),
		Tasks:       bundle.Tasks,
		Checks:      bundle.Checks,
		Credentials: credentials,
		GitEvents:   bundle.GitEvents,
		Workers:     testWorkers{Service: bundle.Queue, registry: registry},
		Sessions:    bundle.Sessions,
		Status:      bundle.Status,
		Reconciler:  bundle.Reconciler,
		Threads:     bundle.Threads,
		CheckConfig: bundle.CheckConfigs,
		Merges:      bundle.Merges,
		WebSessions: registry.WebSessions(),
	}
}

func authorizedRequest(method string, path string, body any) *http.Request {
	var requestBody bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&requestBody).Encode(body)
	}
	request := httptest.NewRequest(method, path, &requestBody)
	request.Header.Set("Authorization", "Bearer owner-token")
	request.Header.Set(protocolHeader, contract.ProtocolVersion)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	return request
}

func doJSONRequest(t *testing.T, server *Server, method string, path string, body any, wantStatus int, target any) {
	t.Helper()
	doJSONRequestAs(t, server, "owner-token", method, path, body, wantStatus, target)
}

func doJSONRequestAs(t *testing.T, server *Server, token string, method string, path string, body any, wantStatus int, target any, extraHeaders ...string) {
	t.Helper()

	response := httptest.NewRecorder()
	request := authorizedRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(extraHeaders); i += 2 {
		request.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}
	server.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, path, response.Code, wantStatus, response.Body.String())
	}
	if target != nil {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
}

func harnessOptionNames(options []contract.HarnessOption) map[string]bool {
	names := map[string]bool{}
	for _, option := range options {
		names[option.Name] = true
	}
	return names
}

// newMultiProjectServer builds a registry with the named projects, mints an
// owner token, and returns the server alongside the project bundles in the
// order the names were given.
func newMultiProjectServer(t *testing.T, projectNames ...string) (*Server, []*ProjectBundle) {
	t.Helper()

	registry, bundles, _, _ := newTestRegistryInDir(t, t.TempDir(), projectNames...)
	if err := registry.Credentials().EnsureToken(context.Background(), coordinator.CredentialInput{
		Token: "owner-token",
		Scope: coordinator.TokenScopeOwner,
	}); err != nil {
		t.Fatalf("store owner token: %v", err)
	}
	server, err := NewServer(ServerOptions{Registry: registry, OwnerToken: "owner-token"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	return server, bundles
}

func TestListProjectsEndpoint(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, authorizedRequest(http.MethodGet, "/v2/projects", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v2/projects status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "exchange_url") {
		t.Fatalf("project response advertises an exchange URL: %s", recorder.Body.String())
	}
	var response projectsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode projects response: %v", err)
	}
	if len(response.Projects) != 2 {
		t.Fatalf("projects = %+v, want 2", response.Projects)
	}

	byID := map[string]uiProject{}
	for _, project := range response.Projects {
		byID[project.ID] = project
	}
	for _, bundle := range bundles {
		got, ok := byID[bundle.Project.ID]
		if !ok {
			t.Fatalf("project %s missing from listing %+v", bundle.Project.ID, response.Projects)
		}
		if got.Name != bundle.Project.Name {
			t.Fatalf("project %s name = %q, want %q", bundle.Project.ID, got.Name, bundle.Project.Name)
		}
	}
}

func TestCreateProjectUsesNormalizedIDAndRejectsKeyCollision(t *testing.T) {
	registry, _, _, _ := newTestRegistryInDir(t, t.TempDir())
	ctx := context.Background()

	project, err := registry.CreateProject(ctx, coordinator.Project{
		Name: "Flow App", RepoPath: "/tmp/flow-app-a", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create first project: %v", err)
	}
	if project.ID != "p-flow-app" || project.Name != "Flow App" {
		t.Fatalf("project = %+v, want id p-flow-app and preserved display name", project)
	}

	_, err = registry.CreateProject(ctx, coordinator.Project{
		Name: "flow_app", RepoPath: "/tmp/flow-app-b", BaseBranch: "main",
	})
	if !errors.Is(err, coordinator.ErrProjectIDExists) {
		t.Fatalf("normalized key collision err = %v, want ErrProjectIDExists", err)
	}
	if err == nil || !strings.Contains(err.Error(), "choose a distinct --name") {
		t.Fatalf("collision err = %v, want actionable --name guidance", err)
	}
}

func TestAggregateBoardMergesProjects(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	ctx := context.Background()

	for _, bundle := range bundles {
		task, err := bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Work in " + bundle.Project.Name})
		if err != nil {
			t.Fatalf("create task in %s: %v", bundle.Project.Name, err)
		}
		wantID := "t-" + bundle.Project.Name + "-0001"
		if task.ID != wantID {
			t.Fatalf("first task id in %s = %q, want %s", bundle.Project.Name, task.ID, wantID)
		}
	}

	var response aggregateBoardResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v2/board", nil, http.StatusOK, &response)
	if len(response.Boards) != 2 {
		t.Fatalf("boards = %+v, want 2", response.Boards)
	}

	seenProjects := map[string]bool{}
	for _, board := range response.Boards {
		if seenProjects[board.ProjectID] {
			t.Fatalf("duplicate project id %s in aggregate board", board.ProjectID)
		}
		seenProjects[board.ProjectID] = true
		wantID := "t-" + board.ProjectName + "-0001"
		if len(board.Board.Unscheduled) != 1 || board.Board.Unscheduled[0].ID != wantID {
			t.Fatalf("board for %s unscheduled = %+v, want [%s]", board.ProjectName, board.Board.Unscheduled, wantID)
		}
	}
	for _, bundle := range bundles {
		if !seenProjects[bundle.Project.ID] {
			t.Fatalf("aggregate board missing project %s; boards = %+v", bundle.Project.ID, response.Boards)
		}
	}
}

func TestProjectScopedTaskRouteIsolation(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")
	projectA, projectB := bundles[0], bundles[1]

	task, err := projectA.Tasks.CreateTask(context.Background(), coordinator.CreateTaskInput{Title: "Only in alpha"})
	if err != nil {
		t.Fatalf("create task in alpha: %v", err)
	}
	if task.ID != "t-alpha-0001" {
		t.Fatalf("task id = %q, want t-alpha-0001", task.ID)
	}

	doJSONRequestAs(t, server, "owner-token", http.MethodGet,
		"/v2/projects/"+projectB.Project.ID+"/tasks/t-alpha-0001", nil, http.StatusNotFound, nil)

	var found taskResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet,
		"/v2/projects/"+projectA.Project.ID+"/tasks/t-alpha-0001", nil, http.StatusOK, &found)
	if found.Task.ID != "t-alpha-0001" || found.Task.Title != "Only in alpha" {
		t.Fatalf("alpha task = %+v", found.Task)
	}
}

func TestUnscopedTaskRouteResolvesEmbeddedProjectWithMultipleProjects(t *testing.T) {
	server, bundles := newMultiProjectServer(t, "alpha", "beta")

	created, err := bundles[0].Tasks.CreateTask(context.Background(), coordinator.CreateTaskInput{Title: "Globally addressable"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	var found taskResponse
	doJSONRequestAs(t, server, "owner-token", http.MethodGet, "/v2/tasks/"+created.ID, nil, http.StatusOK, &found)
	if found.Task.ID != "t-alpha-0001" || found.Task.Title != "Globally addressable" {
		t.Fatalf("unscoped task = %+v", found.Task)
	}
}

func TestSessionProcessExitRejectsConsoleSession(t *testing.T) {
	fixture := newTestFixture(t)

	var startedConsole consoleResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/console", consoleRequest{
		Harness: "harness",
	}, http.StatusCreated, &startedConsole)
	if startedConsole.Job == nil {
		t.Fatalf("started console response = %+v", startedConsole)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/register", registerWorkerRequest{
		Labels:                  map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"},
		CapacityPersistentAgent: 1,
		HeartbeatTTLSeconds:     60,
	}, http.StatusOK, nil)
	var claim claimJobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/claim", claimJobRequest{
		Buckets:              []flowworker.CapacityBucket{flowworker.BucketPersistentAgent},
		LeaseDurationSeconds: 60,
	}, http.StatusOK, &claim)
	if !claim.Claimed || claim.Job == nil || claim.Job.ID != startedConsole.Job.ID {
		t.Fatalf("claim console = %+v, want job %s", claim, startedConsole.Job.ID)
	}
	var running jobResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/workers/running", markJobRunningRequest{
		LeaseID: claim.Lease.ID,
	}, http.StatusOK, &running)
	if running.Session == nil || running.Session.Role != flowworker.RoleConsole {
		t.Fatalf("running console = %+v", running)
	}

	// A console session must be released through /v2/console, never the generic
	// persistent-session process-exit path.
	var body errorResponse
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/sessions/"+running.Session.ID+"/process-exit", sessionProcessExitRequest{
		LeaseID:  claim.Lease.ID,
		ExitCode: 0,
	}, http.StatusBadRequest, &body)
	if !strings.Contains(body.Error.Message, "console sessions are released through console release") {
		t.Fatalf("console process-exit error = %q, want console release rejection", body.Error.Message)
	}
	// The console session and its lease are untouched by the rejected call.
	session, err := fixture.Sessions.GetSession(context.Background(), running.Session.ID)
	if err != nil {
		t.Fatalf("get console session: %v", err)
	}
	if session.RuntimeState == coordinator.SessionCrashed {
		t.Fatalf("console session = %+v, want unchanged (not crashed)", session)
	}
}

func TestWorkflowRetryAPIRefreshesCurrentAuthorRuntime(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Refresh blocked author runtime")
	taskID := started.Session.TaskID
	runID := started.Session.WorkflowRunID

	before, err := fixture.Bundle.WorkflowRuns.Get(ctx, runID)
	if err != nil {
		t.Fatalf("load workflow before crash: %v", err)
	}
	authorNode, ok := before.Snapshot.Node(before.CurrentNodeKey)
	if !ok || authorNode.Config.Agent == nil {
		t.Fatalf("current author node = %+v, found=%t", authorNode, ok)
	}
	frozen := authorNode.Config.Agent.Agent
	override, err := fixture.Bundle.AgentDefs.Update(ctx, frozen.ID, coordinator.AgentDefInput{
		Name: frozen.Name, Harness: flowharness.Harness, Model: "api-refreshed-model", ReasoningEffort: "high",
		Prompt: "Updated live prompt must remain outside the frozen workflow.",
	})
	if err != nil {
		t.Fatalf("create project author override: %v", err)
	}
	if override.ID == frozen.ID {
		t.Fatalf("project override id = frozen global id %q, want a local definition", override.ID)
	}

	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/sessions/"+started.Session.ID+"/process-exit", sessionProcessExitRequest{
		LeaseID: started.Session.LeaseID, ExitCode: 1,
	}, http.StatusOK, nil)

	var retry workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+taskID+"/workflow/retry", workflowRetryRequest{
		RefreshAgentRuntime: true,
	}, http.StatusOK, &retry)
	refreshedNode, ok := retry.Run.Snapshot.Node(retry.Run.CurrentNodeKey)
	if !ok || refreshedNode.Config.Agent == nil {
		t.Fatalf("retried author node = %+v, found=%t", refreshedNode, ok)
	}
	refreshed := refreshedNode.Config.Agent.Agent
	if refreshed.Harness != override.Harness || refreshed.Model != override.Model || refreshed.ReasoningEffort != override.ReasoningEffort {
		t.Fatalf("refreshed runtime = %+v, want override %+v", refreshed, override)
	}
	if refreshed.ID != frozen.ID || refreshed.Name != frozen.Name || refreshed.Prompt != frozen.Prompt {
		t.Fatalf("refreshed frozen fields = %+v, want id/name/prompt from %+v", refreshed, frozen)
	}
}

func TestWorkflowAuthorProcessExitPausesUntilHumanRetry(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Bounded workflow author crashes")
	taskID := started.Session.TaskID
	runID := started.Session.WorkflowRunID

	crash := func(session coordinator.Session) {
		t.Helper()
		doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, "/v2/sessions/"+session.ID+"/process-exit", sessionProcessExitRequest{
			LeaseID:  session.LeaseID,
			ExitCode: 1,
		}, http.StatusOK, nil)
	}

	crash(started.Session)
	live := liveAuthorJobsForTask(t, fixture, taskID)
	if len(live) != 0 {
		t.Fatalf("live author jobs after crash = %+v, want no automatic restart", live)
	}
	detail, err := fixture.Bundle.WorkflowRuns.Detail(ctx, runID)
	if err != nil {
		t.Fatalf("load crash-held workflow: %v", err)
	}
	if detail.Run.State != coordinator.WorkflowRunWaiting ||
		detail.OpenWait == nil ||
		detail.OpenWait.Kind != coordinator.WorkflowWaitOperatorIntervention ||
		detail.OpenWait.Reason != coordinator.WorkflowWaitReasonExecutionFailed {
		t.Fatalf("workflow after crash = %+v, want execution-failure wait", detail)
	}
	if !strings.Contains(detail.OpenWait.Message, "ended in state crashed") {
		t.Fatalf("crash wait message = %q, want crash explanation", detail.OpenWait.Message)
	}

	for i := 0; i < 3; i++ {
		if err := fixture.Bundle.WorkflowExecutor.Advance(ctx, runID); err != nil {
			t.Fatalf("advance crash-held workflow: %v", err)
		}
	}
	if live := liveAuthorJobsForTask(t, fixture, taskID); len(live) != 0 {
		t.Fatalf("repeated advances re-enqueued author jobs: %+v", live)
	}

	var retry workflowRunResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks/"+taskID+"/workflow/retry", map[string]any{}, http.StatusOK, &retry)
	if retry.Run.State != coordinator.WorkflowRunRunning {
		t.Fatalf("retried workflow = %+v, want running", retry.Run)
	}
	live = liveAuthorJobsForTask(t, fixture, taskID)
	if len(live) != 1 {
		t.Fatalf("live author jobs after human retry = %+v, want one", live)
	}
	node, ok, err := fixture.Bundle.WorkflowRuns.GetNodeRun(ctx, retry.Run.CurrentNodeRunID)
	if err != nil || !ok {
		t.Fatalf("load retried node: ok=%t err=%v", ok, err)
	}
	if node.Attempt != 2 || node.State != coordinator.WorkflowNodeQueued || node.Error != "" {
		t.Fatalf("retried node = %+v, want queued attempt 2 with cleared error", node)
	}
}

func TestWorkflowAuthorCompletionRetiresPriorAutoMergeConflictCheck(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Resolve prior merge conflict")

	required := true
	exitCode := 1
	reportConflict := func() {
		t.Helper()
		if _, err := fixture.Checks.ReportCheck(ctx, coordinator.ReportCheckInput{
			TaskID: started.Session.TaskID, Name: coordinator.AutoMergeCheckName, Kind: coordinator.CheckKindCI,
			Required: &required, Verdict: coordinator.CheckBlocked, ExitCode: &exitCode,
			Details:  coordinator.AutoMergeConflictDetailsPrefix + " resolve the prior conflict",
			Reporter: "coordinator",
		}); err != nil {
			t.Fatalf("report auto-merge conflict: %v", err)
		}
	}
	reportConflict()

	payload, err := json.Marshal(map[string]string{
		"change_id": started.Change.ID,
		"head_sha":  "resolved-head",
	})
	if err != nil {
		t.Fatalf("marshal change artifact: %v", err)
	}
	artifact, _, err := fixture.Bundle.WorkflowArtifacts.Create(ctx, coordinator.CreateWorkflowArtifactInput{
		WorkflowRunID:   started.Session.WorkflowRunID,
		NodeRunID:       started.Session.NodeRunID,
		SessionID:       started.Session.ID,
		CreatorKey:      "test-author",
		Kind:            coordinator.ArtifactChange,
		SummaryMarkdown: "Resolved the merge conflict.",
		Payload:         payload,
		ClientKey:       "resolved-merge-conflict",
	})
	if err != nil {
		t.Fatalf("create change artifact: %v", err)
	}

	// Keep this API regression focused on completion. Node execution is covered
	// by coordinator tests and would require a real exchange revision here.
	fixture.Bundle.WorkflowExecutor = nil
	path := "/v2/tasks/" + started.Session.TaskID + "/workflow/complete"
	request := workflowCompleteRequest{NodeRunID: started.Session.NodeRunID, ArtifactID: artifact.ID}
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, request, http.StatusOK, nil)

	check, err := fixture.Checks.GetCheck(ctx, started.Session.TaskID, coordinator.AutoMergeCheckName)
	if err != nil {
		t.Fatalf("load retired auto-merge check: %v", err)
	}
	if check.Required || check.Verdict != coordinator.CheckSkipped || check.Details != "reset after new author revision" {
		t.Fatalf("auto-merge check after author completion = %+v, want retired", check)
	}

	// A replay is not a new author revision and must not erase a conflict that
	// was reported after the original completion.
	reportConflict()
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, path, request, http.StatusOK, nil)
	check, err = fixture.Checks.GetCheck(ctx, started.Session.TaskID, coordinator.AutoMergeCheckName)
	if err != nil {
		t.Fatalf("reload auto-merge check after replay: %v", err)
	}
	if !check.Required || check.Verdict != coordinator.CheckBlocked {
		t.Fatalf("auto-merge check after completion replay = %+v, want current conflict preserved", check)
	}
}

func TestWorkflowFailedStepCanBeSkippedByOwner(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()
	started := startAuthorSessionForStatusTest(t, fixture, "Skippable workflow review failure")
	taskID := started.Session.TaskID
	runID := started.Session.WorkflowRunID

	repoPath := t.TempDir()
	runAPIGit(t, repoPath, "init", "-b", "main")
	runAPIGit(t, repoPath, "config", "user.name", "Flow Test")
	runAPIGit(t, repoPath, "config", "user.email", "flow-test@example.com")
	writeAPIFile(t, repoPath, "README.md", "skip API test")
	runAPIGit(t, repoPath, "add", "README.md")
	runAPIGit(t, repoPath, "commit", "-m", "test: seed skip workflow")
	headSHA := apiGitOutput(t, repoPath, "rev-parse", "HEAD")
	runAPIGit(t, "", "--git-dir", fixture.Project.ExchangePath, "fetch", repoPath, "HEAD:refs/heads/main")
	runAPIGit(t, "", "--git-dir", fixture.Project.ExchangePath, "update-ref", "refs/heads/"+started.Change.Branch, headSHA)
	if _, err := fixture.DB.ExecContext(ctx, `UPDATE changes SET head_sha = ? WHERE id = ?`, headSHA, started.Change.ID); err != nil {
		t.Fatalf("pin test change head: %v", err)
	}

	payload, err := json.Marshal(map[string]string{
		"change_id": started.Change.ID,
		"head_sha":  headSHA,
	})
	if err != nil {
		t.Fatalf("marshal change artifact: %v", err)
	}
	artifact, _, err := fixture.Bundle.WorkflowArtifacts.Create(ctx, coordinator.CreateWorkflowArtifactInput{
		WorkflowRunID:   runID,
		NodeRunID:       started.Session.NodeRunID,
		SessionID:       started.Session.ID,
		CreatorKey:      "test-skip-session",
		Kind:            coordinator.ArtifactChange,
		SummaryMarkdown: "Author work completed for the skip API test.",
		Payload:         payload,
		ClientKey:       "skip-api-change",
	})
	if err != nil {
		t.Fatalf("create change artifact: %v", err)
	}
	if _, err := fixture.Sessions.ReadyAuthorSession(ctx, started.Session.ID); err != nil {
		t.Fatalf("ready author session: %v", err)
	}
	if _, err := fixture.Bundle.WorkflowRuns.CompleteNode(ctx, coordinator.CompleteWorkflowNodeInput{
		NodeRunID:  started.Session.NodeRunID,
		Outcome:    "completed",
		ArtifactID: artifact.ID,
		Actor:      coordinator.ActorAgent,
	}); err != nil {
		t.Fatalf("complete author node: %v", err)
	}
	if err := fixture.Bundle.WorkflowExecutor.Advance(ctx, runID); err != nil {
		t.Fatalf("advance workflow to review: %v", err)
	}
	beforeFailure, err := fixture.Bundle.WorkflowRuns.Detail(ctx, runID)
	if err != nil {
		t.Fatalf("load workflow at review: %v", err)
	}
	failedNodeRunID := beforeFailure.Run.CurrentNodeRunID
	failedNode, ok, err := fixture.Bundle.WorkflowRuns.GetNodeRun(ctx, failedNodeRunID)
	if err != nil || !ok {
		t.Fatalf("load review node: ok=%t err=%v", ok, err)
	}
	node, ok := beforeFailure.Run.Snapshot.Node(failedNode.NodeKey)
	if !ok || node.Kind != coordinator.NodeChangeReview {
		t.Fatalf("active workflow node = %+v, want change review", node)
	}
	checks, err := fixture.Checks.ListChecks(ctx, taskID)
	if err != nil {
		t.Fatalf("list review checks: %v", err)
	}
	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token: "skip-reviewer-token", Scope: coordinator.TokenScopeWorker, Subject: "w-skip-reviewer",
	}); err != nil {
		t.Fatalf("store skip reviewer token: %v", err)
	}
	if _, err := fixture.Workers.RegisterWorker(ctx, flowworker.RegisterWorkerInput{
		ID: "w-skip-reviewer", Labels: map[string]string{flowharness.AgentHarnessLabel(flowharness.Harness): "true"}, CapacityPersistentAgent: 1,
	}); err != nil {
		t.Fatalf("register skip reviewer: %v", err)
	}
	claimed, ok, err := fixture.Workers.ClaimNextJob(ctx, flowworker.ClaimInput{
		WorkerID: "w-skip-reviewer", Buckets: []flowworker.CapacityBucket{flowworker.BucketPersistentAgent}, LeaseDuration: time.Minute,
	})
	if err != nil || !ok {
		jobs, listErr := fixture.Workers.ListJobs(ctx)
		t.Fatalf("claim review job: ok=%t err=%v jobs=%+v list_err=%v", ok, err, jobs, listErr)
	}
	if _, err := fixture.Workers.MarkJobRunning(ctx, claimed.Lease.ID); err != nil {
		t.Fatalf("mark review job running: %v", err)
	}
	checkName, _ := claimed.Job.Payload["check_name"].(string)
	var failedCheck coordinator.Check
	for _, check := range checks {
		if check.Required && check.Name == checkName && strings.HasSuffix(check.Name, ".node."+failedNodeRunID) {
			failedCheck = check
			break
		}
	}
	if failedCheck.ID == 0 {
		t.Fatalf("claimed job = %+v checks = %+v, want a required node-scoped review check", claimed.Job, checks)
	}
	required := true
	sourceJobID := claimed.Job.ID
	leaseID := claimed.Lease.ID
	reportPath := "/v2/tasks/" + taskID + "/checks/" + failedCheck.Name
	doJSONRequestAs(t, fixture.Server, "skip-reviewer-token", http.MethodPost, reportPath, reportCheckRequest{
		Kind: string(failedCheck.Kind), Required: &required, Verdict: string(coordinator.CheckErrored),
		Details: "review harness failed", SourceJobID: &sourceJobID, LeaseID: &leaseID,
	}, http.StatusOK, nil)
	if err := fixture.Bundle.WorkflowExecutor.Advance(ctx, runID); err != nil {
		t.Fatalf("pause failed review: %v", err)
	}
	before, err := fixture.Bundle.WorkflowRuns.Detail(ctx, runID)
	if err != nil || before.OpenWait == nil || before.OpenWait.NodeRunID != failedNodeRunID {
		t.Fatalf("load failed workflow before skip: detail=%+v err=%v", before, err)
	}

	skipPath := "/v2/tasks/" + taskID + "/workflow/skip"
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, skipPath, workflowSkipRequest{
		NodeRunID: started.Session.NodeRunID,
	}, http.StatusConflict, nil)
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodPost, skipPath, workflowSkipRequest{
		NodeRunID: failedNodeRunID,
	}, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, skipPath, workflowSkipRequest{}, http.StatusBadRequest, nil)

	var skipped coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, skipPath, workflowSkipRequest{
		NodeRunID: failedNodeRunID,
	}, http.StatusOK, &skipped)
	if skipped.Run.CurrentNodeKey == failedNode.NodeKey {
		t.Fatalf("skipped workflow = %+v, want workflow advanced beyond failed review", skipped.Run)
	}
	nextNode, ok := skipped.Run.Snapshot.Node(skipped.Run.CurrentNodeKey)
	if !ok || nextNode.Kind != coordinator.NodeVerifyChange {
		t.Fatalf("workflow node after skipped review = %+v found=%t, want verification", nextNode, ok)
	}
	failedNode, ok, err = fixture.Bundle.WorkflowRuns.GetNodeRun(ctx, failedNodeRunID)
	if err != nil || !ok || failedNode.State != coordinator.WorkflowNodeSucceeded || failedNode.Outcome != "approved" {
		t.Fatalf("skipped node = %+v ok=%t err=%v", failedNode, ok, err)
	}
	retired, err := fixture.Checks.GetCheck(ctx, taskID, failedCheck.Name)
	if err != nil || retired.Required || retired.Verdict != coordinator.CheckSkipped {
		t.Fatalf("retired failed check = %+v err=%v", retired, err)
	}
	doJSONRequestAs(t, fixture.Server, "skip-reviewer-token", http.MethodPost, reportPath, reportCheckRequest{
		Kind: string(failedCheck.Kind), Required: &required, Verdict: string(coordinator.CheckSatisfied),
		Details: "late worker result", SourceJobID: &sourceJobID, LeaseID: &leaseID,
	}, http.StatusForbidden, nil)
	retired, err = fixture.Checks.GetCheck(ctx, taskID, failedCheck.Name)
	if err != nil || retired.Required || retired.Verdict != coordinator.CheckSkipped {
		t.Fatalf("retired check after late report = %+v err=%v", retired, err)
	}
	waiver, err := fixture.Checks.GetCheck(ctx, taskID, "workflow-step-skipped.node."+failedNodeRunID)
	if err != nil || !waiver.Required || waiver.Verdict != coordinator.CheckSatisfied {
		t.Fatalf("skip waiver = %+v err=%v", waiver, err)
	}
	if reviewState, err := fixture.Checks.ReviewState(ctx, taskID); err != nil || reviewState != coordinator.ReviewInReview {
		t.Fatalf("review state after skipped review = %s err=%v, want in_review while verification runs", reviewState, err)
	}
	after, err := fixture.Bundle.WorkflowRuns.Detail(ctx, runID)
	if err != nil {
		t.Fatalf("load skipped workflow: %v", err)
	}
	foundSkip := false
	for _, transition := range after.Transitions {
		if transition.EventKind == "node_skipped" && transition.FromNodeKey == failedNode.NodeKey && transition.Outcome == "approved" {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Fatalf("workflow transitions = %+v, want node_skipped audit event", after.Transitions)
	}

	var replay coordinator.CompleteWorkflowNodeResult
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, skipPath, workflowSkipRequest{
		NodeRunID: failedNodeRunID,
	}, http.StatusOK, &replay)
	if !replay.Replayed {
		t.Fatalf("duplicate skip = %+v, want idempotent replay", replay)
	}
}
