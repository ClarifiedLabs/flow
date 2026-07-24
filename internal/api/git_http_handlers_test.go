package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/config"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

func TestGitHTTPExchangeAuthAndHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed")
	}
	// Workers authenticate their repository clone through process-wide Git
	// config. Test Git commands must not send that credential to this server.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Bearer inherited-worker-token")

	fixture := newGitHTTPServerFixture(t)
	project := fixture.project
	httpServer := fixture.httpServer

	exchangeURL := httpServer.URL + "/git/projects/" + project.ID + "/exchange.git"
	ownerURL := urlWithCredentials(exchangeURL, "flow", "owner-token")
	workerURL := urlWithCredentials(exchangeURL, "flow", "worker-token")

	repoPath := newGitHTTPWorktree(t)
	runGitHTTPTestGit(t, repoPath, "push", ownerURL, "refs/heads/main:refs/heads/main")
	mainSHA := gitHTTPTestOutput(t, repoPath, "rev-parse", "refs/heads/main")
	exchangeMainSHA := gitHTTPTestBareOutput(t, project.ExchangePath, "rev-parse", "refs/heads/main")
	if exchangeMainSHA != mainSHA {
		t.Fatalf("exchange main = %q, want %q", exchangeMainSHA, mainSHA)
	}
	assertGitHTTPProtocolV2(t, httpServer, exchangeURL)

	if err := runGitHTTPTestGitErr(t, repoPath, "ls-remote", exchangeURL); err == nil {
		t.Fatal("unauthenticated ls-remote succeeded")
	}

	basic := base64.StdEncoding.EncodeToString([]byte("flow:owner-token"))
	runGitHTTPTestGitWithEnv(t, repoPath, []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}, "ls-remote", exchangeURL, "refs/heads/main")

	bearer := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer owner-token",
	}
	runGitHTTPTestGitWithEnv(t, repoPath, bearer, "ls-remote", exchangeURL, "refs/heads/main")

	runGitHTTPTestGit(t, repoPath, "checkout", "-b", "task/t-demo-0001")
	if err := os.WriteFile(filepath.Join(repoPath, "task.txt"), []byte("task\n"), 0o644); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	runGitHTTPTestGit(t, repoPath, "add", "task.txt")
	runGitHTTPTestGit(t, repoPath, "commit", "-m", "task work")
	runGitHTTPTestGit(t, repoPath, "push", workerURL, "refs/heads/task/t-demo-0001:refs/heads/task/t-demo-0001")

	runGitHTTPTestGit(t, repoPath, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("forbidden\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGitHTTPTestGit(t, repoPath, "add", "README.md")
	runGitHTTPTestGit(t, repoPath, "commit", "-m", "forbidden base update")
	runGitHTTPTestGit(t, repoPath, "push", ownerURL, "refs/heads/main:refs/heads/main")
	ownerMainSHA := gitHTTPTestOutput(t, repoPath, "rev-parse", "refs/heads/main")
	exchangeMainSHA = gitHTTPTestBareOutput(t, project.ExchangePath, "rev-parse", "refs/heads/main")
	if exchangeMainSHA != ownerMainSHA {
		t.Fatalf("exchange main after owner push = %q, want %q", exchangeMainSHA, ownerMainSHA)
	}

	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("worker update\n"), 0o644); err != nil {
		t.Fatalf("write worker readme: %v", err)
	}
	runGitHTTPTestGit(t, repoPath, "add", "README.md")
	runGitHTTPTestGit(t, repoPath, "commit", "-m", "worker base update")
	if err := runGitHTTPTestGitErr(t, repoPath, "push", workerURL, "refs/heads/main:refs/heads/main"); err == nil {
		t.Fatal("worker base update succeeded, want protected-base rejection")
	}
}

func TestGitHTTPExchangeCompressedFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed")
	}

	fixture := newGitHTTPServerFixture(t)
	exchangeURL := fixture.httpServer.URL + "/git/projects/" + fixture.project.ID + "/exchange.git"
	ownerURL := urlWithCredentials(exchangeURL, "flow", "owner-token")

	sourcePath := newGitHTTPWorktree(t)
	for i := 0; i < 32; i++ {
		runGitHTTPTestGit(t, sourcePath, "commit", "--allow-empty", "-m", fmt.Sprintf("common %d", i+1))
	}
	commonSHA := gitHTTPTestOutput(t, sourcePath, "rev-parse", "HEAD")
	runGitHTTPTestGit(t, sourcePath, "push", ownerURL, "refs/heads/main:refs/heads/main")

	clientPath := filepath.Join(t.TempDir(), "client")
	runGitHTTPTestGit(t, "", "clone", "--branch", "main", ownerURL, clientPath)

	runGitHTTPTestGit(t, sourcePath, "commit", "--allow-empty", "-m", "remote main")
	wantRefs := map[string]string{
		"refs/remotes/origin/main": gitHTTPTestOutput(t, sourcePath, "rev-parse", "HEAD"),
	}
	pushArgs := []string{"push", ownerURL, "refs/heads/main:refs/heads/main"}
	for i := 1; i <= 6; i++ {
		branch := fmt.Sprintf("task/t-demo-%04d/run-1", i)
		runGitHTTPTestGit(t, sourcePath, "checkout", "-b", branch, commonSHA)
		runGitHTTPTestGit(t, sourcePath, "commit", "--allow-empty", "-m", fmt.Sprintf("remote task %d", i))
		pushArgs = append(pushArgs, "refs/heads/"+branch+":refs/heads/"+branch)
		wantRefs["refs/remotes/origin/"+branch] = gitHTTPTestOutput(t, sourcePath, "rev-parse", "HEAD")
	}
	runGitHTTPTestGit(t, sourcePath, pushArgs...)

	trace, err := runGitHTTPTestGitOutputWithEnv(t, clientPath, []string{
		"GIT_TRACE_CURL=1",
		"GIT_TRACE_CURL_NO_DATA=1",
	}, "-c", "protocol.version=0", "fetch", "origin")
	if err != nil {
		t.Fatalf("fetch gzip-compressed negotiation: %v", err)
	}
	if !strings.Contains(trace, "Content-Encoding: gzip") {
		t.Fatalf("fetch did not exercise gzip request encoding:\n%s", trace)
	}
	for ref, wantSHA := range wantRefs {
		gotSHA := gitHTTPTestOutput(t, clientPath, "rev-parse", ref)
		if gotSHA != wantSHA {
			t.Errorf("%s = %q, want %q", ref, gotSHA, wantSHA)
		}
	}
}

type gitHTTPServerFixture struct {
	httpServer *httptest.Server
	project    coordinator.Project
}

func newGitHTTPServerFixture(t *testing.T) gitHTTPServerFixture {
	t.Helper()

	ctx := context.Background()
	dataDir := t.TempDir()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })

	registry, err := NewRegistry(RegistryOptions{DataDir: dataDir, Global: global})
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
	if err := registry.Credentials().EnsureToken(ctx, coordinator.CredentialInput{
		Token:   "worker-token",
		Scope:   coordinator.TokenScopeWorker,
		Subject: "w-test",
	}); err != nil {
		t.Fatalf("store worker token: %v", err)
	}

	project, err := registry.CreateProject(ctx, coordinator.Project{Name: "demo", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	installGitHTTPTestHooks(t, project)

	server, err := NewServer(ServerOptions{Registry: registry, ProtocolVersion: config.DefaultProtocolVersion})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	return gitHTTPServerFixture{httpServer: httpServer, project: project}
}

func assertGitHTTPProtocolV2(t *testing.T, httpServer *httptest.Server, exchangeURL string) {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, exchangeURL+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("new protocol v2 request: %v", err)
	}
	request.SetBasicAuth("flow", "owner-token")
	request.Header.Set("Git-Protocol", "version=2")
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("request protocol v2 advertisement: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read protocol v2 advertisement: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("protocol v2 advertisement status = %d, want %d: %s", response.StatusCode, http.StatusOK, strings.TrimSpace(string(body)))
	}
	if !strings.Contains(string(body), "version 2") {
		t.Fatalf("protocol v2 advertisement did not negotiate version 2: %q", body)
	}
}

func TestGitHTTPHookHelper(t *testing.T) {
	if os.Getenv("FLOW_GIT_HTTP_HOOK_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}
	code := 2
	if len(args) >= 5 && args[0] == "git-hook" {
		hookName := args[1]
		repoPath := ""
		baseBranch := "main"
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--repo":
				i++
				if i < len(args) {
					repoPath = args[i]
				}
			case "--base":
				i++
				if i < len(args) {
					baseBranch = args[i]
				}
			}
		}
		opts := flowgit.HookOptions{
			ExchangeRepoPath: repoPath,
			BaseBranch:       baseBranch,
			Stdin:            os.Stdin,
			Stdout:           os.Stdout,
			Stderr:           os.Stderr,
		}
		var err error
		switch hookName {
		case "pre-receive":
			err = flowgit.HandlePreReceive(context.Background(), opts)
		case "post-receive":
			err = flowgit.HandlePostReceive(context.Background(), opts)
		}
		if err == nil {
			code = 0
		} else {
			code = 1
		}
	}
	os.Exit(code)
}

func installGitHTTPTestHooks(t *testing.T, project coordinator.Project) {
	t.Helper()
	if err := flowgit.InstallHooks(project.ExchangePath, flowgit.HookInstallOptions{
		BaseBranch: project.BaseBranch,
		HookCommand: flowgit.HookCommand{
			Path: os.Args[0],
			Args: []string{"-test.run=TestGitHTTPHookHelper", "--"},
			Env:  map[string]string{"FLOW_GIT_HTTP_HOOK_HELPER": "1"},
		},
	}); err != nil {
		t.Fatalf("install test hooks: %v", err)
	}
}

func newGitHTTPWorktree(t *testing.T) string {
	t.Helper()
	repoPath := t.TempDir()
	runGitHTTPTestGit(t, "", "-c", "init.defaultBranch=main", "init", repoPath)
	runGitHTTPTestGit(t, repoPath, "config", "user.email", "flow@example.com")
	runGitHTTPTestGit(t, repoPath, "config", "user.name", "Flow Test")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGitHTTPTestGit(t, repoPath, "add", "README.md")
	runGitHTTPTestGit(t, repoPath, "commit", "-m", "seed")
	return repoPath
}

func urlWithCredentials(rawURL string, username string, password string) string {
	return strings.Replace(rawURL, "://", "://"+username+":"+password+"@", 1)
}

func runGitHTTPTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runGitHTTPTestGitWithEnv(t, dir, nil, args...)
}

func runGitHTTPTestGitWithEnv(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	if err := runGitHTTPTestGitErrWithEnv(t, dir, env, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

func runGitHTTPTestGitErr(t *testing.T, dir string, args ...string) error {
	t.Helper()
	return runGitHTTPTestGitErrWithEnv(t, dir, nil, args...)
}

func runGitHTTPTestGitErrWithEnv(t *testing.T, dir string, env []string, args ...string) error {
	t.Helper()
	_, err := runGitHTTPTestGitOutputWithEnv(t, dir, env, args...)
	return err
}

func runGitHTTPTestGitOutputWithEnv(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	fullArgs := append([]string{"-c", "credential.helper="}, args...)
	cmd := exec.Command("git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Flow workers authenticate their own Git operations through an inherited
	// GIT_CONFIG_* extra header. Do not let that header override the credentials
	// this integration test is explicitly exercising.
	cmd.Env = append(gitHTTPTestEnvironment(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", errWithOutput{err: err, output: strings.TrimSpace(string(output))}
	}
	return string(output), nil
}

func gitHTTPTestEnvironment(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func TestGitHTTPTestEnvironmentRemovesInjectedGitConfig(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer leaked-token",
		"HOME=/tmp/home",
	}
	got := gitHTTPTestEnvironment(environ)
	want := []string{"PATH=/usr/bin", "HOME=/tmp/home"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filtered environment = %q, want %q", got, want)
	}
}

func gitHTTPTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
	}
	return strings.TrimSpace(string(output))
}

func gitHTTPTestBareOutput(t *testing.T, gitDir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"--git-dir", gitDir}, args...)
	return gitHTTPTestOutput(t, "", fullArgs...)
}

type errWithOutput struct {
	err    error
	output string
}

func (e errWithOutput) Error() string {
	if e.output == "" {
		return e.err.Error()
	}
	return e.output + ": " + e.err.Error()
}
