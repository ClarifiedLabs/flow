package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestDefaultAgentChecksUseSelectedHarnessAndArgs(t *testing.T) {
	suite, err := withDefaultAgentChecks(CheckSuite{}, flowharness.Claude, flowharness.Args{
		Claude: []string{"--model", "sonnet"},
		Codex:  []string{"--model", "gpt-5", "-c", "model_reasoning_effort=high"},
	})
	if err != nil {
		t.Fatalf("default agent checks: %v", err)
	}
	if len(suite.Definitions) != 2 {
		t.Fatalf("default definitions = %+v, want reviewer and verifier", suite.Definitions)
	}
	for _, definition := range suite.Definitions {
		if definition.Entrypoint == nil || len(definition.Entrypoint.Argv) != 1 {
			t.Fatalf("%s entrypoint = %+v", definition.Name, definition.Entrypoint)
		}
		command := definition.Entrypoint.Argv[0]
		for _, want := range []string{"claude --dangerously-skip-permissions --permission-mode bypassPermissions", "'--model' 'sonnet'", "--harness claude"} {
			if !strings.Contains(command, want) {
				t.Fatalf("%s default command missing %q:\n%s", definition.Name, want, command)
			}
		}
		if got := definition.Requires; len(got) != 1 || got[0] != flowharness.AgentHarnessLabel(flowharness.Claude) {
			t.Fatalf("%s requires = %#v, want claude harness label", definition.Name, got)
		}
	}
}

func TestCheckConfigInvalidEntrypointFailsClearly(t *testing.T) {
	t.Parallel()
	_, err := parseCheckDefinition(".flow/checks/bad.yaml", `
name: bad
kind: ci
`)
	if err == nil || !strings.Contains(err.Error(), "entrypoint is required") {
		t.Fatalf("parse err = %v, want missing entrypoint", err)
	}
}

func TestCheckConfigRetiresRemovedAutomatedChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	issues := NewIssueService(store.DB())
	checks := NewCheckService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, flowworker.NewService(store.DB()), nil, Project{}, CheckConfigServiceOptions{})
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Removed check issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	required := true
	if _, err := checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issue.ID,
		Name:     "removed",
		Kind:     CheckKindCI,
		Required: &required,
		Verdict:  CheckPending,
	}); err != nil {
		t.Fatalf("seed removed check: %v", err)
	}
	if err := checkConfig.retireAbsentAutomatedChecks(ctx, issue.ID, CheckSuite{
		Configured: true,
		Definitions: []CheckDefinition{{
			Name: "unit",
			Kind: CheckKindCI,
		}},
	}); err != nil {
		t.Fatalf("retire absent: %v", err)
	}
	removed, err := checks.GetCheck(ctx, issue.ID, "removed")
	if err != nil {
		t.Fatalf("get removed: %v", err)
	}
	if removed.Verdict != CheckSkipped || removed.Required {
		t.Fatalf("removed check = %+v, want skipped optional", removed)
	}
}

func TestLiveCheckJobExistsIsScopedToHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), nil, workers, nil, Project{}, CheckConfigServiceOptions{})
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Head scoped job issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	changeID := "ch-head-scoped-1"
	otherChangeID := "ch-head-scoped-2"
	for _, change := range []struct {
		id     string
		branch string
	}{
		{id: changeID, branch: "issue/head-scoped-1"},
		{id: otherChangeID, branch: "issue/head-scoped-2"},
	} {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO changes (id, issue_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, 'main', 'head-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			change.id, issue.ID, change.branch); err != nil {
			t.Fatalf("insert change %s: %v", change.id, err)
		}
	}
	if _, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issue.ID,
		ChangeID:       &changeID,
		Role:           flowworker.RoleCI,
		CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{
			"check_name": "unit",
			"head_sha":   "head-1",
		},
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	exists, err := checkConfig.liveCheckJobExists(ctx, issue.ID, changeID, flowworker.RoleCI, "unit", "head-1")
	if err != nil {
		t.Fatalf("lookup matching head: %v", err)
	}
	if !exists {
		t.Fatal("live job not found for matching head")
	}
	exists, err = checkConfig.liveCheckJobExists(ctx, issue.ID, changeID, flowworker.RoleCI, "unit", "head-2")
	if err != nil {
		t.Fatalf("lookup different head: %v", err)
	}
	if exists {
		t.Fatal("live job matched different head")
	}
	exists, err = checkConfig.liveCheckJobExists(ctx, issue.ID, otherChangeID, flowworker.RoleCI, "unit", "head-1")
	if err != nil {
		t.Fatalf("lookup different change: %v", err)
	}
	if exists {
		t.Fatal("live job matched different change")
	}
}

func TestCheckConfigValidationRejectsEscapingCWDAndReservedEnv(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"cwd": `
name: bad-cwd
kind: ci
entrypoint:
  argv: ["go", "test"]
  cwd: "../outside"
`,
		"env": `
name: bad-env
kind: ci
entrypoint:
  argv: ["go", "test"]
  env:
    flow_token: secret
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCheckDefinition(".flow/checks/"+name+".yaml", config); err == nil {
				t.Fatal("parse succeeded, want validation failure")
			}
		})
	}
}

func writeCheckConfig(t *testing.T, repoPath string, relativePath string, contents string) {
	t.Helper()
	writeReconcileFile(t, repoPath, relativePath, strings.TrimSpace(contents)+"\n")
}

// checkConfigServices bundles the services wired together for the
// check-config tests so call sites can pull out only what they need.
type checkConfigServices struct {
	issues      *IssueService
	workers     *flowworker.Service
	sessions    *SessionService
	checks      *CheckService
	threads     *ThreadService
	checkConfig *CheckConfigService
}

// wireCheckConfigServices constructs the standard service graph used across the
// check-config tests, sharing the same DB handle and project.
func wireCheckConfigServices(store *flowdb.Store, project Project) checkConfigServices {
	issues := NewIssueService(store.DB())
	workers := flowworker.NewService(store.DB())
	sessions := NewSessionService(store.DB(), issues, workers)
	checks := NewCheckService(store.DB())
	threads := NewThreadService(store.DB())
	checkConfig := NewCheckConfigServiceWithOptions(store.DB(), checks, workers, threads, project, CheckConfigServiceOptions{})
	return checkConfigServices{
		issues:      issues,
		workers:     workers,
		sessions:    sessions,
		checks:      checks,
		threads:     threads,
		checkConfig: checkConfig,
	}
}

// checkConfigFile is an ordered (path, content) pair seeded into .flow/checks.
type checkConfigFile struct {
	path    string
	content string
}

// seedReadyChangeWithConfig creates the issue branch, writes the given check
// config files, commits and pushes them to the exchange, and advances the
// change head to the pushed commit. It returns the updated change and the
// pushed head SHA. commitMessage is the message used for the seed commit.
func seedReadyChangeWithConfig(t *testing.T, ctx context.Context, repoPath string, project Project, sessions *SessionService, issue Issue, ensured EnsureAuthorJobResult, commitMessage string, configs []checkConfigFile) (Change, string) {
	t.Helper()
	branch := "issue/" + issue.ID
	if err := runReconcileGit(repoPath, nil, "checkout", "-b", branch, "main"); err != nil {
		t.Fatalf("checkout issue branch: %v", err)
	}
	for _, config := range configs {
		writeCheckConfig(t, repoPath, config.path, config.content)
	}
	if err := runReconcileGit(repoPath, nil, "add", ".flow/checks"); err != nil {
		t.Fatalf("git add checks: %v", err)
	}
	if err := runReconcileGit(repoPath, nil, "commit", "-m", commitMessage); err != nil {
		t.Fatalf("commit checks: %v", err)
	}
	head, err := reconcileGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse head: %v", err)
	}
	if err := runReconcileGit(repoPath, []string{"FLOW_GIT_PRINCIPAL=worker:w-local"}, "push", project.ExchangeURL, branch+":"+branch); err != nil {
		t.Fatalf("push issue branch: %v", err)
	}
	change, err := sessions.UpdateChangeHead(ctx, ensured.Change.ID, head)
	if err != nil {
		t.Fatalf("update change head: %v", err)
	}
	return change, head
}

func assertCheckPending(t *testing.T, checks *CheckService, issueID string, name string, kind CheckKind) {
	t.Helper()
	check, err := checks.GetCheck(context.Background(), issueID, name)
	if err != nil {
		t.Fatalf("get check %s: %v", name, err)
	}
	if check.Kind != kind || check.Verdict != CheckPending || !check.Required {
		t.Fatalf("check %s = %+v", name, check)
	}
}

func assertLiveCheckJobs(t *testing.T, workers *flowworker.Service, issueID string, want map[flowworker.JobRole]int) {
	t.Helper()
	jobs, err := workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	counts := map[flowworker.JobRole]int{}
	for _, job := range jobs {
		if job.IssueID == nil || *job.IssueID != issueID {
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

func assertLiveCheckJobEntrypointContains(t *testing.T, workers *flowworker.Service, issueID string, role flowworker.JobRole, checkName string, snippet string) {
	t.Helper()
	jobs, err := workers.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, job := range jobs {
		if job.IssueID == nil || *job.IssueID != issueID || job.Role != role {
			continue
		}
		switch job.State {
		case flowworker.JobQueued, flowworker.JobClaimed, flowworker.JobRunning:
			if payloadString(job.Payload, "check_name") != checkName {
				continue
			}
			entrypoint, ok := job.Payload["entrypoint"].(map[string]any)
			if !ok {
				t.Fatalf("%s job entrypoint = %#v", checkName, job.Payload["entrypoint"])
			}
			if !argvContains(entrypoint["argv"], snippet) {
				t.Fatalf("%s job argv = %#v, want snippet %q", checkName, entrypoint["argv"], snippet)
			}
			return
		}
	}
	t.Fatalf("live %s job %q not found", role, checkName)
}

func argvContains(value any, snippet string) bool {
	switch argv := value.(type) {
	case []any:
		for _, arg := range argv {
			if text, ok := arg.(string); ok && strings.Contains(text, snippet) {
				return true
			}
		}
	case []string:
		for _, arg := range argv {
			if strings.Contains(arg, snippet) {
				return true
			}
		}
	}
	return false
}
