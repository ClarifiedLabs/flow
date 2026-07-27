package coordinator

import (
	"context"
	"testing"
)

func TestWorkflowReviewAuthorCyclesWaitAtConfiguredLimit(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	agents := NewAgentDefService(flows.db)
	author, err := agents.Create(ctx, AgentDefInput{
		Name: "cycle author", Harness: "codex", Prompt: "Implement requested changes.",
	})
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	reviewer, err := agents.Create(ctx, AgentDefInput{
		Name: "cycle reviewer", Harness: "codex", Prompt: "Review the change.",
	})
	if err != nil {
		t.Fatalf("create reviewer: %v", err)
	}
	flow, err := flows.Create(ctx, FlowInput{
		Name:             "bounded review loop",
		StartNode:        "implement",
		TransitionBudget: 100,
		Nodes: []FlowNodeInput{
			{
				Key: "implement", Name: "Implement", Kind: NodeAgent,
				Config: FlowNodeConfig{Agent: &AgentNodeConfig{
					AgentDefID: author.ID, Workspace: WorkspaceChange, Artifact: ArtifactChange,
				}},
			},
			{
				Key: "review", Name: "Review", Kind: NodeChangeReview,
				Config: FlowNodeConfig{ChangeReview: &ChangeReviewNodeConfig{
					Agents: []ReviewAgentConfig{{AgentDefID: reviewer.ID}},
				}},
			},
			{
				Key: "done", Name: "Done", Kind: NodeTerminal,
				Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}},
			},
		},
		Edges: []FlowEdgeInput{
			{From: "implement", Outcome: "completed", To: "review"},
			{From: "review", Outcome: "changes_requested", To: "implement"},
			{From: "review", Outcome: "approved", To: "done"},
		},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Bounded task", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule run: %v", err)
	}
	if run.ReviewCycleBudget != DefaultReviewAuthorCycleLimit {
		t.Fatalf("review cycle budget = %d, want %d", run.ReviewCycleBudget, DefaultReviewAuthorCycleLimit)
	}
	current, created, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil {
		t.Fatalf("create first author node: %v", err)
	}
	if !created {
		t.Fatal("first author node was not created")
	}

	complete := func(node WorkflowNodeRun, outcome string) CompleteWorkflowNodeResult {
		t.Helper()
		currentNode, found, err := runs.GetNodeRun(ctx, node.ID)
		if err != nil {
			t.Fatalf("load %s: %v", node.NodeKey, err)
		}
		if !found {
			t.Fatalf("node %s was not found", node.ID)
		}
		if currentNode.State == WorkflowNodeQueued {
			if _, err := runs.MarkNodeRunning(ctx, node.ID); err != nil {
				t.Fatalf("mark %s running: %v", node.NodeKey, err)
			}
		}
		result, err := runs.CompleteNode(ctx, CompleteWorkflowNodeInput{
			NodeRunID:         node.ID,
			Outcome:           outcome,
			Actor:             ActorSystem,
			OperatorSatisfied: node.NodeKey == "implement",
		})
		if err != nil {
			t.Fatalf("complete %s with %s: %v", node.NodeKey, outcome, err)
		}
		return result
	}

	for cycle := 1; cycle <= DefaultReviewAuthorCycleLimit; cycle++ {
		result := complete(current, "completed")
		if result.Next == nil || result.Next.NodeKey != "review" {
			t.Fatalf("cycle %d author result next = %+v, want review", cycle, result.Next)
		}
		result = complete(*result.Next, "changes_requested")
		if result.Next == nil || result.Next.NodeKey != "implement" {
			t.Fatalf("cycle %d review result next = %+v, want implement", cycle, result.Next)
		}
		if result.Run.ReviewCyclesUsed != cycle {
			t.Fatalf("cycle %d used count = %d", cycle, result.Run.ReviewCyclesUsed)
		}
		current = *result.Next
	}

	result := complete(current, "completed")
	if result.Next == nil || result.Next.NodeKey != "review" {
		t.Fatalf("over-budget review node = %+v", result.Next)
	}
	blockedReview := *result.Next
	result = complete(blockedReview, "changes_requested")
	if result.Run.State != WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting", result.Run.State)
	}
	if result.Run.ReviewCyclesUsed != DefaultReviewAuthorCycleLimit {
		t.Fatalf("used cycles = %d, want %d", result.Run.ReviewCyclesUsed, DefaultReviewAuthorCycleLimit)
	}
	wantTransitions := 2*DefaultReviewAuthorCycleLimit + 1
	if result.Run.TransitionsUsed != wantTransitions {
		t.Fatalf("transitions used = %d, want %d before the blocked send-back", result.Run.TransitionsUsed, wantTransitions)
	}
	blockedNode, found, err := runs.GetNodeRun(ctx, blockedReview.ID)
	if err != nil {
		t.Fatalf("load blocked review: %v", err)
	}
	if !found || blockedNode.State != WorkflowNodeWaiting || blockedNode.Outcome != "" {
		t.Fatalf("blocked review = %+v, want an incomplete waiting node", blockedNode)
	}
	detail, err := runs.Detail(ctx, run.ID)
	if err != nil {
		t.Fatalf("load blocked run detail: %v", err)
	}
	if detail.OpenWait == nil ||
		detail.OpenWait.Kind != WorkflowWaitOperatorIntervention ||
		detail.OpenWait.Reason != WorkflowWaitReasonReviewCycleLimit ||
		detail.OpenWait.NodeRunID != blockedReview.ID {
		t.Fatalf("open wait = %+v, want review-cycle operator wait", detail.OpenWait)
	}

	replayed := complete(blockedReview, "changes_requested")
	if !replayed.Replayed || replayed.Run.ReviewCyclesUsed != DefaultReviewAuthorCycleLimit {
		t.Fatalf("replayed blocked completion = %+v", replayed)
	}

	extended, err := runs.ExtendBudget(ctx, task.ID, 2, ActorHuman)
	if err != nil {
		t.Fatalf("extend review cycle budget: %v", err)
	}
	if extended.State != WorkflowRunRunning || extended.ReviewCycleBudget != DefaultReviewAuthorCycleLimit+2 {
		t.Fatalf("extended run = %+v", extended)
	}
	result = complete(blockedReview, "changes_requested")
	if result.Next == nil || result.Next.NodeKey != "implement" || result.Run.ReviewCyclesUsed != DefaultReviewAuthorCycleLimit+1 {
		t.Fatalf("resumed review send-back = %+v", result)
	}
}

func TestAutomatedReviewAuthorCycleClassification(t *testing.T) {
	changeAuthor := FlowNodeSnapshot{
		Kind:   NodeAgent,
		Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{Workspace: WorkspaceChange}},
	}
	baseAgent := FlowNodeSnapshot{
		Kind:   NodeAgent,
		Config: FlowNodeSnapshotConfig{Agent: &AgentNodeSnapshotConfig{Workspace: WorkspaceBase}},
	}
	tests := []struct {
		name    string
		source  FlowNodeSnapshot
		target  FlowNodeSnapshot
		outcome string
		want    bool
	}{
		{name: "change review send-back", source: FlowNodeSnapshot{Kind: NodeChangeReview}, target: changeAuthor, outcome: "changes_requested", want: true},
		{name: "verification send-back", source: FlowNodeSnapshot{Kind: NodeVerifyChange}, target: changeAuthor, outcome: "changes_requested", want: true},
		{name: "human requested changes", source: FlowNodeSnapshot{Kind: NodeHumanGate}, target: changeAuthor, outcome: "changes_requested"},
		{name: "check failure", source: FlowNodeSnapshot{Kind: NodeAutomatedChecks}, target: changeAuthor, outcome: "failed"},
		{name: "review approval", source: FlowNodeSnapshot{Kind: NodeChangeReview}, target: changeAuthor, outcome: "approved"},
		{name: "base workspace", source: FlowNodeSnapshot{Kind: NodeChangeReview}, target: baseAgent, outcome: "changes_requested"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isAutomatedReviewAuthorCycle(test.source, test.target, test.outcome); got != test.want {
				t.Fatalf("isAutomatedReviewAuthorCycle() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWorkflowReviewCycleBudgetUsesConfiguredLimit(t *testing.T) {
	ctx := context.Background()
	flows, tasks, _ := newWorkflowModelServices(t)
	flow, err := flows.Create(ctx, FlowInput{
		Name:      "configured cycle limit",
		StartNode: "done",
		Nodes: []FlowNodeInput{{
			Key: "done", Name: "Done", Kind: NodeTerminal,
			Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}},
		}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Configured limit task", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	runs := NewWorkflowRunServiceWithOptions(flows.db, flows, tasks, WorkflowRunServiceOptions{
		ReviewAuthorCycleLimit: 3,
	})
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule run: %v", err)
	}
	if run.ReviewCycleBudget != 3 {
		t.Fatalf("review cycle budget = %d, want configured limit 3", run.ReviewCycleBudget)
	}
}
