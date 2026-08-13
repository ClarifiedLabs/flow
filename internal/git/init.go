package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultExchangeName = "flow"

type HookCommand struct {
	Path string
	Args []string
	Env  map[string]string
}

// SeedOptions configures the client-side half of project onboarding: the
// worktree git work that flow init performs after the coordinator has
// registered the project and created its exchange.
type SeedOptions struct {
	RepoPath     string
	BaseBranch   string
	ExchangeName string
	ExchangeURL  string
	Token        string
}

type SeedResult struct {
	RepoRoot   string
	BaseBranch string
	Seeded     bool
	Warning    string
}

type AttachOptions struct {
	RepoPath      string
	ExchangeName  string
	ExchangeURL   string
	Token         string
	ReplaceRemote bool
}

type AttachResult struct {
	RepoRoot string
}

// AttachExchangeWorktree verifies an existing project's exchange and adds its
// remote to a local worktree. It does not inspect or push local refs, so it is
// safe for fresh clones and repositories without commits.
func AttachExchangeWorktree(ctx context.Context, opts AttachOptions) (AttachResult, error) {
	if strings.TrimSpace(opts.ExchangeURL) == "" {
		return AttachResult{}, errors.New("exchange url is required")
	}
	if strings.TrimSpace(opts.ExchangeName) == "" {
		opts.ExchangeName = DefaultExchangeName
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		opts.RepoPath = "."
	}

	repoRoot, err := gitOutput(ctx, opts.RepoPath, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return AttachResult{}, fmt.Errorf("verify git worktree: %w", err)
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return AttachResult{}, fmt.Errorf("resolve git worktree: %w", err)
	}

	if _, err := gitOutput(ctx, repoRoot, gitHTTPAuthEnv(opts.Token), "ls-remote", opts.ExchangeURL); err != nil {
		return AttachResult{}, fmt.Errorf("verify exchange remote: %w", err)
	}
	if err := ensureRemote(ctx, repoRoot, opts.ExchangeName, opts.ExchangeURL, opts.ReplaceRemote); err != nil {
		return AttachResult{}, err
	}

	return AttachResult{RepoRoot: repoRoot}, nil
}

// SeedExchangeFromWorktree verifies the worktree, adds the exchange remote,
// and pushes the base branch into the exchange when it is not there yet. It
// only talks to the exchange over its URL, so the coordinator may live on a
// different machine. The seed push runs as the owner principal; the exchange
// pre-receive hook permits creating (never updating) the protected base ref.
func SeedExchangeFromWorktree(ctx context.Context, opts SeedOptions) (SeedResult, error) {
	if strings.TrimSpace(opts.ExchangeURL) == "" {
		return SeedResult{}, errors.New("exchange url is required")
	}
	if strings.TrimSpace(opts.ExchangeName) == "" {
		opts.ExchangeName = DefaultExchangeName
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		opts.RepoPath = "."
	}

	repoRoot, err := gitOutput(ctx, opts.RepoPath, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return SeedResult{}, fmt.Errorf("verify git worktree: %w", err)
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return SeedResult{}, fmt.Errorf("resolve git worktree: %w", err)
	}

	if _, err := gitOutput(ctx, repoRoot, nil, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return SeedResult{}, fmt.Errorf("verify repository has at least one commit: %w", err)
	}

	baseBranch := strings.TrimSpace(opts.BaseBranch)
	if baseBranch == "" {
		baseBranch, err = detectBaseBranch(ctx, repoRoot)
		if err != nil {
			return SeedResult{}, err
		}
	}
	if err := validateRefName(ctx, repoRoot, baseBranch); err != nil {
		return SeedResult{}, err
	}
	if _, err := gitOutput(ctx, repoRoot, nil, "show-ref", "--verify", "refs/heads/"+baseBranch); err != nil {
		return SeedResult{}, fmt.Errorf("base branch %q does not exist locally: %w", baseBranch, err)
	}

	// Uncommitted files only narrow what the seed contains; the push ships
	// committed HEAD.
	var warning string
	dirty, err := worktreeDirty(ctx, repoRoot)
	if err != nil {
		return SeedResult{}, err
	}
	if dirty {
		warning = "worktree has uncommitted changes; exchange remote was seeded from committed HEAD only"
	}

	if err := ensureRemote(ctx, repoRoot, opts.ExchangeName, opts.ExchangeURL, false); err != nil {
		return SeedResult{}, err
	}

	seeded := false
	authEnv := gitHTTPAuthEnv(opts.Token)
	exists, err := remoteRefExists(ctx, repoRoot, opts.ExchangeURL, authEnv, "refs/heads/"+baseBranch)
	if err != nil {
		return SeedResult{}, err
	}
	if !exists {
		// Seeding pushes the full base-branch history (possibly to a network
		// remote), so it gets the transfer budget instead of the routine one.
		env := append([]string{"FLOW_GIT_PRINCIPAL=" + ownerActor}, authEnv...)
		if err := gitRunTransfer(ctx, repoRoot, env, "push", opts.ExchangeURL, "refs/heads/"+baseBranch+":refs/heads/"+baseBranch); err != nil {
			return SeedResult{}, fmt.Errorf("seed exchange remote: %w", err)
		}
		seeded = true
	}

	return SeedResult{
		RepoRoot:   repoRoot,
		BaseBranch: baseBranch,
		Seeded:     seeded,
		Warning:    warning,
	}, nil
}

// remoteRefExists checks a ref on the remote over its URL, so it works for
// any transport the client can push to.
func remoteRefExists(ctx context.Context, repoPath string, remoteURL string, env []string, ref string) (bool, error) {
	output, err := gitOutput(ctx, repoPath, env, "ls-remote", remoteURL, ref)
	if err != nil {
		return false, fmt.Errorf("list remote refs: %w", err)
	}

	return strings.TrimSpace(output) != "", nil
}

func gitHTTPAuthEnv(token string) []string {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}

	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer " + token,
	}
}

func detectBaseBranch(ctx context.Context, repoPath string) (string, error) {
	branch, err := gitOutput(ctx, repoPath, nil, "branch", "--show-current")
	if err == nil && branch != "" {
		return branch, nil
	}

	return "", fmt.Errorf("detect base branch: %w", err)
}

func validateRefName(ctx context.Context, repoPath string, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("base branch is required")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid base branch %q", name)
	}
	if err := gitRun(ctx, repoPath, nil, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("invalid base branch %q: %w", name, err)
	}

	return nil
}

func worktreeDirty(ctx context.Context, repoPath string) (bool, error) {
	status, err := gitOutput(ctx, repoPath, nil, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check worktree status: %w", err)
	}

	return strings.TrimSpace(status) != "", nil
}

func ensureBareExchange(ctx context.Context, exchangePath string) error {
	if _, err := os.Stat(exchangePath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(exchangePath), 0o755); err != nil {
			return fmt.Errorf("create exchange parent directory: %w", err)
		}
		if err := gitRun(ctx, "", nil, "init", "--bare", exchangePath); err != nil {
			return fmt.Errorf("initialize bare exchange remote: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("stat exchange remote: %w", err)
	}

	isBare, err := gitBareOutput(ctx, exchangePath, nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("verify bare exchange remote: %w", err)
	}
	if isBare != "true" {
		return fmt.Errorf("exchange path %s is not a bare git repository", exchangePath)
	}

	return nil
}

func ensureRemote(ctx context.Context, repoPath string, name string, remoteURL string, replace bool) error {
	pushURLs, err := explicitRemotePushURLs(ctx, repoPath, name)
	if err != nil {
		return err
	}
	currentURL, err := gitOutput(ctx, repoPath, nil, "remote", "get-url", name)
	if err != nil {
		if len(pushURLs) > 0 && !replace {
			return fmt.Errorf("git remote %q has conflicting push URL configuration; remove it or rerun with --repair", name)
		}
		if err := gitRun(ctx, repoPath, nil, "remote", "add", name, remoteURL); err != nil {
			return fmt.Errorf("add %s remote: %w", name, err)
		}
		if len(pushURLs) > 0 {
			if err := gitRun(ctx, repoPath, nil, "config", "--unset-all", "remote."+name+".pushurl"); err != nil {
				return fmt.Errorf("clear %s remote push URLs: %w", name, err)
			}
		}
		return nil
	}
	urlsMatch := currentURL == remoteURL
	for _, pushURL := range pushURLs {
		if pushURL != remoteURL {
			urlsMatch = false
			break
		}
	}
	if urlsMatch && (!replace || len(pushURLs) == 0) {
		return nil
	}
	if !replace {
		if currentURL != remoteURL {
			return fmt.Errorf("git remote %q already exists with URL %q; remove or rename it, or rerun with --repair", name, currentURL)
		}
		return fmt.Errorf("git remote %q has conflicting push URL configuration; remove it or rerun with --repair", name)
	}
	if currentURL != remoteURL {
		if err := gitRun(ctx, repoPath, nil, "remote", "set-url", name, remoteURL); err != nil {
			return fmt.Errorf("replace %s remote URL: %w", name, err)
		}
	}
	// An explicit remote.<name>.pushurl overrides the fetch URL for pushes. Clear
	// all such overrides during repair so `git push <name>` cannot keep targeting
	// a stale or unrelated repository.
	if len(pushURLs) > 0 {
		if err := gitRun(ctx, repoPath, nil, "config", "--unset-all", "remote."+name+".pushurl"); err != nil {
			return fmt.Errorf("clear %s remote push URLs: %w", name, err)
		}
	}

	return nil
}

func explicitRemotePushURLs(ctx context.Context, repoPath string, name string) ([]string, error) {
	result, err := runGit(ctx, repoPath, "", nil, "config", "--get-all", "remote."+name+".pushurl")
	if err != nil {
		if result.exitCode == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s remote push URLs: %w", name, err)
	}

	// A successful command means at least one value exists. Preserve empty
	// values: an explicit empty pushurl still overrides normal push resolution
	// and must be rejected or cleared just like a stale non-empty URL.
	lines := strings.Split(strings.TrimSuffix(result.stdout, "\n"), "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(lines[index])
	}
	return lines, nil
}

type ServerProjectOptions struct {
	DataDir     string
	ProjectID   string
	BaseBranch  string
	HookCommand HookCommand
}

// ServerProject reports the locations the coordinator created for a project.
type ServerProject struct {
	Dir          string
	DatabasePath string
	ExchangePath string
}

// CreateServerProject creates the project directory and its bare exchange
// remote with hooks installed. It is idempotent: re-running against an
// existing project directory verifies the exchange instead of recreating it.
func CreateServerProject(ctx context.Context, opts ServerProjectOptions) (ServerProject, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return ServerProject{}, errors.New("data dir is required")
	}
	if strings.TrimSpace(opts.ProjectID) == "" {
		return ServerProject{}, errors.New("project id is required")
	}
	baseBranch := strings.TrimSpace(opts.BaseBranch)
	if baseBranch == "" {
		return ServerProject{}, errors.New("base branch is required")
	}
	if strings.HasPrefix(baseBranch, "-") {
		return ServerProject{}, fmt.Errorf("invalid base branch %q", baseBranch)
	}
	if err := gitRun(ctx, "", nil, "check-ref-format", "--branch", baseBranch); err != nil {
		return ServerProject{}, fmt.Errorf("invalid base branch %q: %w", baseBranch, err)
	}
	if opts.HookCommand.Path == "" {
		executable, err := os.Executable()
		if err != nil {
			return ServerProject{}, fmt.Errorf("resolve executable for git hooks: %w", err)
		}
		opts.HookCommand.Path = executable
	}

	projectDir := ProjectDir(opts.DataDir, opts.ProjectID)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return ServerProject{}, fmt.Errorf("create project directory: %w", err)
	}

	exchangePath := filepath.Join(projectDir, "exchange.git")
	if err := ensureBareExchange(ctx, exchangePath); err != nil {
		return ServerProject{}, err
	}
	if err := InstallHooks(exchangePath, HookInstallOptions{
		BaseBranch:  baseBranch,
		HookCommand: opts.HookCommand,
	}); err != nil {
		return ServerProject{}, err
	}

	return ServerProject{
		Dir:          projectDir,
		DatabasePath: ProjectDatabasePath(opts.DataDir, opts.ProjectID),
		ExchangePath: exchangePath,
	}, nil
}

// ProjectDir returns the per-project data directory under the coordinator's
// data dir.
func ProjectDir(dataDir string, projectID string) string {
	return filepath.Join(dataDir, "projects", projectID)
}

// ProjectDatabasePath returns the per-project SQLite database path under the
// coordinator's data dir.
func ProjectDatabasePath(dataDir string, projectID string) string {
	return filepath.Join(ProjectDir(dataDir, projectID), "flow.db")
}
