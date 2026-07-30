package execution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorkspaceCleanupResult summarizes one reconciliation of <work_dir>/jobs.
type WorkspaceCleanupResult struct {
	Removed   int
	Remaining int
}

// CleanupFinalizedJob removes one finalized job's lease-scoped workspace. The
// caller must invoke it only after every local artifact has been consumed and
// the coordinator has acknowledged the job's terminal transition.
func CleanupFinalizedJob(workDir string, jobID string) (bool, error) {
	path, err := validatedJobWorkspacePath(workDir, jobID)
	if err != nil {
		return false, err
	}
	if IsActiveJob(jobID) {
		return false, fmt.Errorf("refuse to remove active job workspace %q", jobID)
	}
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat finalized job workspace %q: %w", jobID, err)
	}
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Errorf("remove finalized job workspace %q: %w", jobID, err)
	}
	return true, nil
}

// ReapOrphanedJobWorkspaces removes non-active workspaces for terminal jobs and
// unknown workspaces older than orphanGrace. The coordinator job list is the
// source of truth: queued, claimed, and running jobs are always preserved.
//
// Callers must not invoke this after a failed coordinator list operation.
func ReapOrphanedJobWorkspaces(workDir string, jobs []Job, orphanGrace time.Duration, now time.Time) (WorkspaceCleanupResult, error) {
	jobRoot, err := validatedJobWorkspaceRoot(workDir)
	if err != nil {
		return WorkspaceCleanupResult{}, err
	}
	entries, err := os.ReadDir(jobRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WorkspaceCleanupResult{}, nil
		}
		return WorkspaceCleanupResult{}, fmt.Errorf("scan job workspaces: %w", err)
	}

	jobByID := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		jobByID[job.ID] = job
	}

	result := WorkspaceCleanupResult{}
	var errs []error
	for _, entry := range entries {
		jobID := entry.Name()
		if IsActiveJob(jobID) {
			result.Remaining++
			continue
		}
		job, known := jobByID[jobID]
		if known && !IsTerminalJobState(job.State) {
			result.Remaining++
			continue
		}
		if !known && orphanGrace > 0 {
			info, err := entry.Info()
			if err != nil {
				result.Remaining++
				errs = append(errs, fmt.Errorf("stat unknown job workspace %q: %w", jobID, err))
				continue
			}
			if now.Sub(info.ModTime()) < orphanGrace {
				result.Remaining++
				continue
			}
		}

		removed, err := CleanupFinalizedJob(workDir, jobID)
		if err != nil {
			result.Remaining++
			errs = append(errs, err)
			continue
		}
		if removed {
			result.Removed++
		}
	}
	return result, errors.Join(errs...)
}

// CountJobWorkspaces returns the current number of entries below
// <work_dir>/jobs without recursively scanning their contents.
func CountJobWorkspaces(workDir string) (int, error) {
	jobRoot, err := validatedJobWorkspaceRoot(workDir)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(jobRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("count job workspaces: %w", err)
	}
	return len(entries), nil
}

func validatedJobWorkspacePath(workDir string, jobID string) (string, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || jobID == "." || jobID == ".." || filepath.Base(jobID) != jobID || strings.ContainsAny(jobID, `/\`) {
		return "", fmt.Errorf("invalid job id %q for workspace cleanup", jobID)
	}
	root, err := validatedJobWorkspaceRoot(workDir)
	if err != nil {
		return "", err
	}
	path := filepath.Clean(filepath.Join(root, jobID))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return "", fmt.Errorf("job workspace %q escapes jobs root", jobID)
	}
	return path, nil
}

func validatedJobWorkspaceRoot(workDir string) (string, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return "", errors.New("worker work directory is required")
	}
	root := filepath.Clean(filepath.Join(workDir, "jobs"))
	info, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return root, nil
		}
		return "", fmt.Errorf("stat worker jobs root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("worker jobs root %s must be a directory, not a symlink or file", root)
	}
	return root, nil
}
