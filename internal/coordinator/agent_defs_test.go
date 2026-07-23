package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestProjectAgentDefsInheritGlobalsAndOverrideByName(t *testing.T) {
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
		Name: "shared-author", Harness: "codex", Model: "gpt-global", Prompt: "global prompt",
	})
	if err != nil {
		t.Fatalf("create global author: %v", err)
	}
	globalReviewer, err := globals.Create(ctx, AgentDefInput{
		Name: "shared-reviewer", Harness: "claude", Prompt: "global review",
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
		Name: "shared-author", Harness: "codex", Model: "gpt-project", Prompt: "project prompt",
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
	if _, err := globals.Update(ctx, globalAuthor.ID, AgentDefInput{Name: "shared-author", Harness: "codex", Model: "gpt-new", Prompt: "updated global"}); err != nil {
		t.Fatalf("update shadowed global author: %v", err)
	}
	stillLocal, err := projectDefs.GetByName(ctx, "shared-author")
	if err != nil || stillLocal.ID != localAuthor.ID || stillLocal.Prompt != "project prompt" {
		t.Fatalf("shadowed author after global update = %+v, err=%v", stillLocal, err)
	}

	forkedReviewer, err := projectDefs.Update(ctx, globalReviewer.ID, AgentDefInput{
		Name: "shared-reviewer", Harness: "claude", Model: "sonnet", Prompt: "project review",
	})
	if err != nil {
		t.Fatalf("override inherited reviewer through update: %v", err)
	}
	if forkedReviewer.ID == globalReviewer.ID || forkedReviewer.Inherited || forkedReviewer.Prompt != "project review" {
		t.Fatalf("forked reviewer = %+v, want a local override", forkedReviewer)
	}
	if _, err := projectDefs.Update(ctx, globalAuthor.ID, AgentDefInput{Name: "renamed", Harness: "codex"}); err == nil {
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

func TestSeedDefaultsUsesMatchingGlobalAgentDefinition(t *testing.T) {
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
	globalAuthor, err := globals.Create(ctx, AgentDefInput{Name: "author", Harness: "claude", Model: "sonnet", Prompt: "shared author"})
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
	if len(globalDefs) != 5 {
		t.Fatalf("global defaults = %d, want 5", len(globalDefs))
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
	if node.Config.Agent == nil || node.Config.Agent.Agent.Prompt != "shared author" || node.Config.Agent.Agent.Model != "sonnet" {
		t.Fatalf("seeded coding snapshot author = %+v, want global author", node.Config.Agent)
	}
}

func TestFlowResolvesProjectOverrideForGlobalAgentReference(t *testing.T) {
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
	global, err := globals.Create(ctx, AgentDefInput{Name: "shared", Harness: "codex", Prompt: "global"})
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

	if _, err := projectDefs.Create(ctx, AgentDefInput{Name: "shared", Harness: "claude", Model: "sonnet", Prompt: "project"}); err != nil {
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
	if node.Config.Agent.Agent.Harness != "claude" || node.Config.Agent.Agent.Model != "sonnet" || node.Config.Agent.Agent.Prompt != "project" {
		t.Fatalf("snapshot agent = %+v, want project override", node.Config.Agent.Agent)
	}
}
