package historyarchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RestoreWorkspace validates and stages the archive before mutating repo. It
// requires repo to be a clean checkout at the manifest HEAD, reconstructs the
// index/worktree boundary, then installs untracked entries without following
// links or overwriting any existing or tracked path. If any operation after
// mutation begins fails, the checkout is returned to its original clean state.
func RestoreWorkspace(ctx context.Context, src io.Reader, repo string, limits Limits, gitPath string) (_ WorkspaceManifest, returnErr error) {
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	placeholder, err := os.MkdirTemp(filepath.Dir(repo), ".history-workspace-stage-*")
	if err != nil {
		return WorkspaceManifest{}, err
	}
	if err := os.Remove(placeholder); err != nil {
		return WorkspaceManifest{}, err
	}
	stage := placeholder
	inspection, err := Extract(ctx, src, stage, limits)
	if err != nil {
		return WorkspaceManifest{}, err
	}
	defer os.RemoveAll(stage)
	if inspection.Workspace == nil {
		return WorkspaceManifest{}, fmt.Errorf("%w: expected workspace archive", ErrInvalidArchive)
	}
	manifest := *inspection.Workspace
	if gitPath == "" {
		gitPath = "git"
	}
	runner := gitRunner{ctx: ctx, repo: repo, git: gitPath, max: limits.MaxLogicalBytes}
	head, err := runner.run("rev-parse", "--verify", "HEAD")
	if err != nil {
		return WorkspaceManifest{}, err
	}
	if strings.TrimSpace(string(head)) != manifest.HeadCommit {
		return WorkspaceManifest{}, fmt.Errorf("%w: workspace HEAD differs from archive", ErrInvalidArchive)
	}
	status, err := runner.run("status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return WorkspaceManifest{}, err
	}
	if len(status) != 0 {
		return WorkspaceManifest{}, fmt.Errorf("%w: workspace restore requires a clean checkout", ErrDestinationExists)
	}
	staged, err := os.ReadFile(filepath.Join(stage, "patches", "staged.patch"))
	if err != nil {
		return WorkspaceManifest{}, err
	}
	unstaged, err := os.ReadFile(filepath.Join(stage, "patches", "unstaged.patch"))
	if err != nil {
		return WorkspaceManifest{}, err
	}
	if err := preflightWorkspacePatches(ctx, repo, gitPath, limits, staged, unstaged, manifest); err != nil {
		return WorkspaceManifest{}, err
	}

	root, err := os.OpenRoot(repo)
	if err != nil {
		return WorkspaceManifest{}, err
	}
	defer root.Close()
	missingDirs := make(map[string]struct{})
	for _, file := range manifest.Untracked {
		_, ok, err := runner.optional("ls-files", "--error-unmatch", "--", file.Path)
		if err != nil {
			return WorkspaceManifest{}, err
		}
		if ok {
			return WorkspaceManifest{}, fmt.Errorf("%w: untracked path is tracked: %q", ErrInvalidArchive, file.Path)
		}
		if err := noSymlinkParents(repo, file.Path); err != nil {
			return WorkspaceManifest{}, err
		}
		destination := filepath.FromSlash(file.Path)
		if _, err := root.Lstat(destination); err == nil {
			return WorkspaceManifest{}, fmt.Errorf("%w: refusing to overwrite %q", ErrDestinationExists, file.Path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return WorkspaceManifest{}, err
		}
		parts := strings.Split(file.Path, "/")
		for i := 1; i < len(parts); i++ {
			directory := filepath.FromSlash(strings.Join(parts[:i], "/"))
			if _, err := root.Lstat(directory); errors.Is(err, os.ErrNotExist) {
				missingDirs[directory] = struct{}{}
			} else if err != nil {
				return WorkspaceManifest{}, err
			}
		}
		if file.Type == FileRegular {
			input, err := os.Open(filepath.Join(stage, "untracked", destination))
			if err != nil {
				return WorkspaceManifest{}, err
			}
			if err := input.Close(); err != nil {
				return WorkspaceManifest{}, err
			}
		}
	}

	mutated := false
	installed := make([]string, 0, len(manifest.Untracked))
	defer func() {
		if returnErr == nil || !mutated {
			return
		}
		var rollbackErrs []error
		if _, err := runner.run("reset", "--hard", "HEAD"); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("reset tracked workspace: %w", err))
		}
		for i := len(installed) - 1; i >= 0; i-- {
			if err := root.Remove(installed[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove restored path %q: %w", installed[i], err))
			}
		}
		directories := make([]string, 0, len(missingDirs))
		for directory := range missingDirs {
			directories = append(directories, directory)
		}
		sort.Slice(directories, func(i, j int) bool {
			return strings.Count(directories[i], string(filepath.Separator)) > strings.Count(directories[j], string(filepath.Separator))
		})
		for _, directory := range directories {
			if err := root.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrExist) {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove restored directory %q: %w", directory, err))
			}
		}
		if rollbackErr := errors.Join(rollbackErrs...); rollbackErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("workspace restore rollback: %w", rollbackErr))
		}
	}()

	if len(staged) != 0 {
		mutated = true
		if _, err := runner.runInput(staged, false, "apply", "--cached", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return WorkspaceManifest{}, err
		}
		if _, err := runner.runInput(staged, false, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return WorkspaceManifest{}, err
		}
	}
	if len(unstaged) != 0 {
		mutated = true
		if _, err := runner.runInput(unstaged, false, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return WorkspaceManifest{}, err
		}
	}
	for _, file := range manifest.Untracked {
		destination := filepath.FromSlash(file.Path)
		mutated = true
		if err := root.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return WorkspaceManifest{}, err
		}
		source := filepath.Join(stage, "untracked", destination)
		if file.Type == FileSymlink {
			if err := root.Symlink(file.LinkTarget, destination); err != nil {
				return WorkspaceManifest{}, err
			}
			installed = append(installed, destination)
			continue
		}
		input, err := os.Open(source)
		if err != nil {
			return WorkspaceManifest{}, err
		}
		output, err := root.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(file.Mode))
		if err != nil {
			input.Close()
			return WorkspaceManifest{}, err
		}
		installed = append(installed, destination)
		_, copyErr := io.Copy(output, input)
		inputErr, outputErr := input.Close(), output.Close()
		if copyErr != nil {
			return WorkspaceManifest{}, copyErr
		}
		if inputErr != nil {
			return WorkspaceManifest{}, inputErr
		}
		if outputErr != nil {
			return WorkspaceManifest{}, outputErr
		}
	}
	index, err := runner.run("ls-files", "--stage", "-z")
	if err != nil {
		return WorkspaceManifest{}, err
	}
	stagedNames, err := runner.run("diff", "--cached", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return WorkspaceManifest{}, err
	}
	unstagedNames, err := runner.run("diff", "--name-only", "-z", "--")
	if err != nil {
		return WorkspaceManifest{}, err
	}
	if got := inventoryDigest(manifest.HeadCommit, index, stagedNames, unstagedNames, manifest.Untracked); got != manifest.InventoryDigest {
		return WorkspaceManifest{}, fmt.Errorf("%w: restored inventory", ErrDigestMismatch)
	}
	return manifest, nil
}

func preflightWorkspacePatches(ctx context.Context, repo, gitPath string, limits Limits, staged, unstaged []byte, manifest WorkspaceManifest) error {
	preflightRoot, err := os.MkdirTemp(filepath.Dir(repo), ".history-workspace-preflight-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(preflightRoot)
	worktree := filepath.Join(preflightRoot, "worktree")
	if err := os.Mkdir(worktree, 0700); err != nil {
		return err
	}
	discovery := gitRunner{ctx: ctx, repo: repo, git: gitPath, max: int64(limits.MaxPathBytes)}
	gitDirBytes, err := discovery.run("rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	gitDir := strings.TrimSpace(string(gitDirBytes))
	if gitDir == "" {
		return fmt.Errorf("%w: repository has no Git directory", ErrInvalidArchive)
	}
	runner := gitRunner{
		ctx: ctx, git: gitPath, dir: worktree, max: limits.MaxLogicalBytes,
		env: []string{"GIT_INDEX_FILE=" + filepath.Join(preflightRoot, "index")},
		global: []string{
			"--git-dir=" + gitDir,
			"--work-tree=" + worktree,
		},
	}
	if _, err := runner.run("read-tree", "HEAD"); err != nil {
		return err
	}
	if _, err := runner.run("checkout-index", "--all", "--force"); err != nil {
		return err
	}
	if len(staged) != 0 {
		if _, err := runner.runInput(staged, false, "apply", "--cached", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return err
		}
		if _, err := runner.runInput(staged, false, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return err
		}
	}
	if len(unstaged) != 0 {
		if _, err := runner.runInput(unstaged, false, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return err
		}
	}
	index, err := runner.run("ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	stagedNames, err := runner.run("diff", "--cached", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return err
	}
	unstagedNames, err := runner.run("diff", "--name-only", "-z", "--")
	if err != nil {
		return err
	}
	if got := inventoryDigest(manifest.HeadCommit, index, stagedNames, unstagedNames, manifest.Untracked); got != manifest.InventoryDigest {
		return fmt.Errorf("%w: preflight workspace inventory", ErrDigestMismatch)
	}
	return nil
}

func noSymlinkParents(root, name string) error {
	parts := strings.Split(name, "/")
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: unsafe destination parent %q", ErrUnsafePath, part)
		}
	}
	return nil
}
