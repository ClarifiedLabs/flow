package coordinator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	sqlite3 "github.com/mattn/go-sqlite3"
)

func TestProjectAgentDefsInheritGlobalsAndOverrideByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	projectStore, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "project.db"))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })

	globals := NewGlobalAgentDefService(globalStore.DB())
	globalAuthor, err := globals.Create(ctx, AgentDefInput{
		Name: "shared-author", Harness: "harness", Model: "gpt-global", Prompt: "global prompt",
	})
	if err != nil {
		t.Fatalf("create global author: %v", err)
	}
	globalReviewer, err := globals.Create(ctx, AgentDefInput{
		Name: "shared-reviewer", Harness: "harness", Prompt: "global review",
	})
	if err != nil {
		t.Fatalf("create global reviewer: %v", err)
	}

	projectDefs := NewInheritedAgentDefService(projectStore.DB(), globals)
	inherited, err := projectDefs.List(ctx)
	if err != nil {
		t.Fatalf("list inherited definitions: %v", err)
	}
	if len(inherited) != 2 || !inherited[0].Inherited || !inherited[1].Inherited {
		t.Fatalf("inherited definitions = %+v, want two inherited rows", inherited)
	}

	localAuthor, err := projectDefs.Create(ctx, AgentDefInput{
		Name: "shared-author", Harness: "harness", Model: "gpt-project", Prompt: "project prompt",
	})
	if err != nil {
		t.Fatalf("create local override: %v", err)
	}
	effective, err := projectDefs.List(ctx)
	if err != nil {
		t.Fatalf("list effective definitions: %v", err)
	}
	if len(effective) != 2 || effective[0].ID != localAuthor.ID || effective[0].Inherited || effective[1].ID != globalReviewer.ID || !effective[1].Inherited {
		t.Fatalf("effective definitions = %+v, want local author and inherited reviewer", effective)
	}
	resolved, err := projectDefs.Resolve(ctx, globalAuthor.ID)
	if err != nil {
		t.Fatalf("resolve global author reference: %v", err)
	}
	if resolved.ID != localAuthor.ID || resolved.Model != "gpt-project" || resolved.Prompt != "project prompt" {
		t.Fatalf("resolved global reference = %+v, want local override %+v", resolved, localAuthor)
	}
	if _, err := globals.Update(ctx, globalAuthor.ID, AgentDefInput{Name: "shared-author", Harness: "harness", Model: "gpt-new", Prompt: "updated global"}); err != nil {
		t.Fatalf("update shadowed global author: %v", err)
	}
	stillLocal, err := projectDefs.GetByName(ctx, "shared-author")
	if err != nil || stillLocal.ID != localAuthor.ID || stillLocal.Prompt != "project prompt" {
		t.Fatalf("shadowed author after global update = %+v, err=%v", stillLocal, err)
	}

	forkedReviewer, err := projectDefs.Update(ctx, globalReviewer.ID, AgentDefInput{
		Name: "shared-reviewer", Harness: "harness", Model: "sonnet", Prompt: "project review",
	})
	if err != nil {
		t.Fatalf("override inherited reviewer through update: %v", err)
	}
	if forkedReviewer.ID == globalReviewer.ID || forkedReviewer.Inherited || forkedReviewer.Prompt != "project review" {
		t.Fatalf("forked reviewer = %+v, want a local override", forkedReviewer)
	}
	if _, err := projectDefs.Update(ctx, globalAuthor.ID, AgentDefInput{Name: "renamed", Harness: "harness"}); err == nil {
		t.Fatal("renaming an inherited definition while overriding it should fail")
	}
	if err := projectDefs.Delete(ctx, globalAuthor.ID); !errors.Is(err, ErrAgentDefNotFound) {
		t.Fatalf("delete inherited definition error = %v, want ErrAgentDefNotFound", err)
	}
	if err := projectDefs.Delete(ctx, localAuthor.ID); err != nil {
		t.Fatalf("delete local override: %v", err)
	}
	resurfaced, err := projectDefs.GetByName(ctx, "shared-author")
	if err != nil || resurfaced.ID != globalAuthor.ID || !resurfaced.Inherited || resurfaced.Prompt != "updated global" {
		t.Fatalf("resurfaced global author = %+v, err=%v", resurfaced, err)
	}
}

func TestSeedDefaultsStampsConfiguredDefaultAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })

	configured := flowharness.AgentSelection{Harness: flowharness.Harness, Model: "anthropic:claude-sonnet-4-6", ReasoningEffort: "high"}
	globals := NewGlobalAgentDefServiceWithOptions(globalStore.DB(), AgentDefServiceOptions{DefaultAgent: configured})
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global defaults: %v", err)
	}
	defs, err := globals.List(ctx)
	if err != nil {
		t.Fatalf("list global defaults: %v", err)
	}
	if len(defs) != 9 {
		t.Fatalf("global defaults = %d, want 9", len(defs))
	}
	for _, def := range defs {
		if def.Harness != flowharness.Harness || def.Model != "anthropic:claude-sonnet-4-6" || def.ReasoningEffort != "high" {
			t.Errorf("seeded %q = harness %q model %q effort %q, want harness/anthropic:claude-sonnet-4-6/high",
				def.Name, def.Harness, def.Model, def.ReasoningEffort)
		}
		if !def.Builtin {
			t.Errorf("seeded %q Builtin = false, want true", def.Name)
		}
	}

	// Re-seeding with a different configured default preserves the existing
	// rows: the config shapes fresh seeds only, never rewrites operator state.
	reseed := NewGlobalAgentDefServiceWithOptions(globalStore.DB(), AgentDefServiceOptions{
		DefaultAgent: flowharness.AgentSelection{Harness: flowharness.Harness},
	})
	if err := reseed.SeedDefaults(ctx); err != nil {
		t.Fatalf("re-seed global defaults: %v", err)
	}
	preserved, err := globals.List(ctx)
	if err != nil {
		t.Fatalf("list preserved defaults: %v", err)
	}
	for i, def := range preserved {
		if def.ID != defs[i].ID || def.Harness != flowharness.Harness || def.Model != "anthropic:claude-sonnet-4-6" {
			t.Errorf("re-seeded %q = %+v, want preserved %+v", def.Name, def, defs[i])
		}
	}
}

func TestSeedDefaultsUsesMatchingGlobalAgentDefinition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	projectStore, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "project.db"))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })

	globals := NewGlobalAgentDefService(globalStore.DB())
	globalAuthor, err := globals.Create(ctx, AgentDefInput{Name: "author", Harness: "harness", Model: "anthropic:claude-sonnet-4-6", Prompt: "shared author"})
	if err != nil {
		t.Fatalf("create global author: %v", err)
	}
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global defaults: %v", err)
	}
	if err := globals.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed global defaults idempotently: %v", err)
	}
	globalDefs, err := globals.List(ctx)
	if err != nil {
		t.Fatalf("list global defaults: %v", err)
	}
	if len(globalDefs) != 9 {
		t.Fatalf("global defaults = %d, want 9", len(globalDefs))
	}
	preservedAuthor, err := globals.GetByName(ctx, "author")
	if err != nil || preservedAuthor.ID != globalAuthor.ID || preservedAuthor.Builtin || preservedAuthor.Prompt != "shared author" {
		t.Fatalf("preserved global author = %+v, err=%v; want custom definition %+v", preservedAuthor, err, globalAuthor)
	}

	projectDefs := NewInheritedAgentDefService(projectStore.DB(), globals)
	flows := NewFlowServiceWithAgentDefs(projectStore.DB(), projectDefs)
	if err := flows.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}

	var localDefs int
	if err := projectStore.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_defs`).Scan(&localDefs); err != nil {
		t.Fatalf("count local agent definitions: %v", err)
	}
	if localDefs != 0 {
		t.Fatalf("local agent definition count = %d, want all defaults to remain inherited", localDefs)
	}
	coding, err := flows.GetByName(ctx, "coding")
	if err != nil {
		t.Fatalf("get coding flow: %v", err)
	}
	if got := coding.Nodes[0].Config.Agent.AgentDefID; got != globalAuthor.ID {
		t.Fatalf("coding author id = %q, want global id %q", got, globalAuthor.ID)
	}
	snapshot, err := flows.ResolveSnapshot(ctx, coding.ID)
	if err != nil {
		t.Fatalf("resolve coding snapshot: %v", err)
	}
	node, _ := snapshot.Node("implement")
	if node.Config.Agent == nil || node.Config.Agent.Agent.Prompt != "shared author" || node.Config.Agent.Agent.Model != "anthropic:claude-sonnet-4-6" {
		t.Fatalf("seeded coding snapshot author = %+v, want global author", node.Config.Agent)
	}
}

func TestFlowResolvesProjectOverrideForGlobalAgentReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	globalStore, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	projectStore, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "project.db"))
	if err != nil {
		t.Fatalf("open project database: %v", err)
	}
	t.Cleanup(func() { _ = projectStore.Close() })

	globals := NewGlobalAgentDefService(globalStore.DB())
	global, err := globals.Create(ctx, AgentDefInput{Name: "shared", Harness: "harness", Prompt: "global"})
	if err != nil {
		t.Fatalf("create global definition: %v", err)
	}
	projectDefs := NewInheritedAgentDefService(projectStore.DB(), globals)
	flows := NewFlowServiceWithAgentDefs(projectStore.DB(), projectDefs)
	flow, err := flows.Create(ctx, FlowInput{
		Name: "global-agent-flow", StartNode: "implement",
		Nodes: []FlowNodeInput{
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: global.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "implement", Outcome: "completed", To: "done"}},
	})
	if err != nil {
		t.Fatalf("create flow with global definition: %v", err)
	}

	if _, err := projectDefs.Create(ctx, AgentDefInput{Name: "shared", Harness: "harness", Model: "anthropic:claude-sonnet-4-6", Prompt: "project"}); err != nil {
		t.Fatalf("create project override: %v", err)
	}
	snapshot, err := flows.ResolveSnapshot(ctx, flow.ID)
	if err != nil {
		t.Fatalf("resolve flow snapshot: %v", err)
	}
	node, ok := snapshot.Node("implement")
	if !ok || node.Config.Agent == nil {
		t.Fatalf("implement snapshot node = %+v, ok=%v", node, ok)
	}
	if node.Config.Agent.Agent.Harness != "harness" || node.Config.Agent.Agent.Model != "anthropic:claude-sonnet-4-6" || node.Config.Agent.Agent.Prompt != "project" {
		t.Fatalf("snapshot agent = %+v, want project override", node.Config.Agent.Agent)
	}
}

type agentDefDeleteRaceGate struct {
	scanArmed   atomic.Bool
	scanEntered chan struct{}
	scanRelease chan struct{}
	releaseOnce sync.Once
}

func newAgentDefDeleteRaceGate() *agentDefDeleteRaceGate {
	return &agentDefDeleteRaceGate{
		scanEntered: make(chan struct{}),
		scanRelease: make(chan struct{}),
	}
}

func (g *agentDefDeleteRaceGate) releaseScan() {
	g.releaseOnce.Do(func() { close(g.scanRelease) })
}

type agentDefDeleteRaceDriver struct {
	driver.Driver
	gate *agentDefDeleteRaceGate
}

func (d agentDefDeleteRaceDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return agentDefDeleteRaceConn{Conn: conn, gate: d.gate}, nil
}

type agentDefDeleteRaceConn struct {
	driver.Conn
	gate *agentDefDeleteRaceGate
}

func (c agentDefDeleteRaceConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil || !strings.Contains(query, "SELECT config_json FROM flow_nodes") {
		return rows, err
	}
	return agentDefDeleteRaceRows{Rows: rows, gate: c.gate}, nil
}

type agentDefDeleteRaceRows struct {
	driver.Rows
	gate *agentDefDeleteRaceGate
}

func (r agentDefDeleteRaceRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if errors.Is(err, io.EOF) && r.gate.scanArmed.CompareAndSwap(true, false) {
		close(r.gate.scanEntered)
		<-r.gate.scanRelease
	}
	return err
}

var agentDefDeleteRaceDriverSequence atomic.Int64

func registerAgentDefDeleteRaceDriver(gate *agentDefDeleteRaceGate) string {
	name := fmt.Sprintf("sqlite3-agent-def-delete-race-%d", agentDefDeleteRaceDriverSequence.Add(1))
	sql.Register(name, agentDefDeleteRaceDriver{Driver: &sqlite3.SQLiteDriver{}, gate: gate})
	return name
}

func TestProjectAgentDefDeleteSerializesConcurrentFlowReferenceWrites(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			gate := newAgentDefDeleteRaceGate()
			defer gate.releaseScan()
			path := filepath.Join(t.TempDir(), "project.db")

			deleteStore, err := flowdb.OpenWithDriver(ctx, registerAgentDefDeleteRaceDriver(gate), path)
			if err != nil {
				t.Fatalf("open deleting database handle: %v", err)
			}
			t.Cleanup(func() { _ = deleteStore.Close() })
			writeStore, err := flowdb.Open(ctx, path)
			if err != nil {
				t.Fatalf("open flow-writing database handle: %v", err)
			}
			t.Cleanup(func() { _ = writeStore.Close() })

			defs := NewAgentDefService(deleteStore.DB())
			agent, err := defs.Create(ctx, AgentDefInput{Name: "concurrent-agent", Harness: "harness"})
			if err != nil {
				t.Fatalf("create agent definition: %v", err)
			}
			flows := NewFlowServiceWithAgentDefs(writeStore.DB(), NewAgentDefService(writeStore.DB()))
			flowName := "concurrent-" + operation
			var existing Flow
			if operation == "update" {
				existing, err = flows.Create(ctx, FlowInput{
					Name: flowName, StartNode: "done",
					Nodes: []FlowNodeInput{{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}}},
				})
				if err != nil {
					t.Fatalf("create flow to update: %v", err)
				}
			}

			writeFlow := func() error {
				input := agentDefDeleteRaceFlowInput(flowName, agent.ID)
				if operation == "update" {
					_, err := flows.Update(ctx, existing.ID, input)
					return err
				}
				_, err := flows.Create(ctx, input)
				return err
			}
			if _, err := writeStore.DB().ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
				t.Fatalf("disable writer busy timeout: %v", err)
			}

			gate.scanArmed.Store(true)
			deleteDone := make(chan error, 1)
			go func() { deleteDone <- defs.Delete(ctx, agent.ID) }()
			waitForAgentDefDeleteRaceSignal(t, ctx, gate.scanEntered, "deletion reference scan")

			writeErr := writeFlow()
			var sqliteErr sqlite3.Error
			if !errors.As(writeErr, &sqliteErr) || sqliteErr.Code != sqlite3.ErrBusy {
				t.Fatalf("concurrent flow %s error = %v, want SQLite busy while deletion holds the writer lock", operation, writeErr)
			}

			gate.releaseScan()
			if err := waitForAgentDefDeleteRaceResult(t, ctx, deleteDone, "delete agent definition"); err != nil {
				t.Fatalf("delete agent definition: %v", err)
			}
			writeErr = writeFlow()
			if writeErr == nil || !strings.Contains(writeErr.Error(), "references unknown agent definition") {
				t.Fatalf("flow %s after deletion error = %v, want unknown agent definition", operation, writeErr)
			}
			if referenced, err := NewAgentDefService(writeStore.DB()).IsReferenced(ctx, agent.ID); err != nil {
				t.Fatalf("inspect references after race: %v", err)
			} else if referenced {
				t.Fatal("concurrent flow write orphaned the deleted agent definition")
			}
		})
	}
}

func agentDefDeleteRaceFlowInput(name, agentDefID string) FlowInput {
	return FlowInput{
		Name: name, StartNode: "work",
		Nodes: []FlowNodeInput{
			{Key: "work", Name: "Work", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: agentDefID, Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "done", Name: "Done", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
		},
		Edges: []FlowEdgeInput{{From: "work", Outcome: "completed", To: "done"}},
	}
}

func waitForAgentDefDeleteRaceSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", operation, ctx.Err())
	}
}

func waitForAgentDefDeleteRaceResult(t *testing.T, ctx context.Context, result <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", operation, ctx.Err())
		return nil
	}
}
