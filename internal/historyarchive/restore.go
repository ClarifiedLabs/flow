package historyarchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RestoreWorkspace validates and stages the archive before mutating repo. It
// requires repo to be at the manifest HEAD, reconstructs the index/worktree
// boundary, then installs untracked entries without following links or
// overwriting any existing or tracked path.
func RestoreWorkspace(ctx context.Context, src io.Reader, repo string, limits Limits, gitPath string) (WorkspaceManifest, error) {
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
	staged, err := os.ReadFile(filepath.Join(stage, "patches", "staged.patch"))
	if err != nil {
		return WorkspaceManifest{}, err
	}
	unstaged, err := os.ReadFile(filepath.Join(stage, "patches", "unstaged.patch"))
	if err != nil {
		return WorkspaceManifest{}, err
	}
	if len(staged) != 0 {
		if _, err := runner.runInput(staged, false, "apply", "--cached", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return WorkspaceManifest{}, err
		}
		if _, err := runner.runInput(staged, false, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return WorkspaceManifest{}, err
		}
	}
	if len(unstaged) != 0 {
		if _, err := runner.runInput(unstaged, false, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return WorkspaceManifest{}, err
		}
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return WorkspaceManifest{}, err
	}
	defer root.Close()
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
		if err := root.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return WorkspaceManifest{}, err
		}
		source := filepath.Join(stage, "untracked", destination)
		if file.Type == FileSymlink {
			if err := root.Symlink(file.LinkTarget, destination); err != nil {
				return WorkspaceManifest{}, err
			}
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
