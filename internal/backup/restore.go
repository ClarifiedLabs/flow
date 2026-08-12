package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
)

// RestoreResult describes one project restored from a backup.
type RestoreResult struct {
	ProjectID string
	Dir       string
	// Restored lists the components written ("flow.db", "exchange.git",
	// "attachments").
	Restored []string
}

// RestoreAllResult describes a full-backup restore.
type RestoreAllResult struct {
	Projects []RestoreResult
	// GlobalDatabase is the path global.db was restored to, empty when the
	// backup did not contain one.
	GlobalDatabase string
}

// RestoreProject restores a single-project backup (inputDir) into
// <dataDir>/projects/<projectID>. When projectID is empty the manifest's
// project_id is used. Restore is offline: flow-server must be stopped. A
// non-empty target project directory is refused unless force is set, in which
// case it is replaced wholesale.
func RestoreProject(ctx context.Context, inputDir, dataDir, projectID string, force bool) (RestoreResult, error) {
	manifest, err := LoadManifest(inputDir)
	if err != nil {
		return RestoreResult{}, err
	}
	if manifest.FlowFormat != FormatVersion {
		return RestoreResult{}, fmt.Errorf("backup flow_format %d is not supported by this flow build (supports %d); restore with the flow version that wrote the backup", manifest.FlowFormat, FormatVersion)
	}
	if manifest.Kind == KindFull {
		return RestoreResult{}, fmt.Errorf("%s is a full backup; restore the whole data dir from it instead of a single project", inputDir)
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = manifest.ProjectID
	}
	if projectID == "" {
		return RestoreResult{}, fmt.Errorf("project id is required: the manifest in %s does not name one", inputDir)
	}
	if !slices.Contains(knownProjectSchemaVersions, manifest.SchemaVersion) {
		return RestoreResult{}, fmt.Errorf("backup schema_version %q is not supported by this flow build (known: %s); the backup was written by an incompatible flow version — restore it with the flow version that wrote it, or upgrade flow", manifest.SchemaVersion, strings.Join(knownProjectSchemaVersions, ", "))
	}
	if _, err := os.Stat(filepath.Join(inputDir, "flow.db")); err != nil {
		return RestoreResult{}, fmt.Errorf("backup project database: %w", err)
	}

	targetDir := flowgit.ProjectDir(dataDir, projectID)
	empty, err := dirIsEmptyOrMissing(targetDir)
	if err != nil {
		return RestoreResult{}, err
	}
	if !empty && !force {
		return RestoreResult{}, fmt.Errorf("project directory %s already exists and is not empty; pass --force to replace it", targetDir)
	}
	if !empty {
		if err := os.RemoveAll(targetDir); err != nil {
			return RestoreResult{}, fmt.Errorf("replace project directory %s: %w", targetDir, err)
		}
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return RestoreResult{}, fmt.Errorf("create project directory: %w", err)
	}
	succeed := false
	defer func() {
		if !succeed {
			_ = os.RemoveAll(targetDir)
		}
	}()

	result := RestoreResult{ProjectID: projectID, Dir: targetDir}
	if err := copyFile(filepath.Join(inputDir, "flow.db"), filepath.Join(targetDir, "flow.db")); err != nil {
		return RestoreResult{}, fmt.Errorf("restore project database: %w", err)
	}
	result.Restored = append(result.Restored, "flow.db")

	bundlePath := filepath.Join(inputDir, "exchange.bundle")
	if _, err := os.Stat(bundlePath); err == nil {
		exchangeDir := filepath.Join(targetDir, "exchange.git")
		// git clone --bare accepts a bundle as the remote and materializes every
		// bundled ref under refs/heads. The bundle does not carry the flow
		// exchange hooks; re-run flow init after restore to reinstall them.
		if err := runGit(ctx, "", "clone", "--bare", "--", bundlePath, exchangeDir); err != nil {
			return RestoreResult{}, fmt.Errorf("restore exchange repository: %w", err)
		}
		result.Restored = append(result.Restored, "exchange.git")
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, fmt.Errorf("stat exchange bundle: %w", err)
	}

	tarPath := filepath.Join(inputDir, "attachments.tar")
	if _, err := os.Stat(tarPath); err == nil {
		attachmentsDir := filepath.Join(targetDir, "attachments")
		if err := untarDirectory(tarPath, attachmentsDir); err != nil {
			return RestoreResult{}, fmt.Errorf("restore attachments: %w", err)
		}
		result.Restored = append(result.Restored, "attachments")
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, fmt.Errorf("stat attachments archive: %w", err)
	}

	succeed = true
	return result, nil
}

// RestoreAll restores a full backup (BackupAll output) into dataDir: every
// project under projects/<id> and, when present, global.db. Restore is
// offline: flow-server must be stopped. Existing content is refused unless
// force is set.
func RestoreAll(ctx context.Context, inputDir, dataDir string, force bool) (RestoreAllResult, error) {
	manifest, err := LoadManifest(inputDir)
	if err != nil {
		return RestoreAllResult{}, err
	}
	if manifest.FlowFormat != FormatVersion {
		return RestoreAllResult{}, fmt.Errorf("backup flow_format %d is not supported by this flow build (supports %d); restore with the flow version that wrote the backup", manifest.FlowFormat, FormatVersion)
	}
	if manifest.Kind != KindFull {
		return RestoreAllResult{}, fmt.Errorf("%s is a single-project backup; restore it with a project target instead", inputDir)
	}

	result := RestoreAllResult{}
	globalSrc := filepath.Join(inputDir, "global.db")
	if _, err := os.Stat(globalSrc); err == nil {
		if !slices.Contains(knownGlobalSchemaVersions, manifest.SchemaVersion) {
			return RestoreAllResult{}, fmt.Errorf("backup global schema_version %q is not supported by this flow build (known: %s); the backup was written by an incompatible flow version — restore it with the flow version that wrote it, or upgrade flow", manifest.SchemaVersion, strings.Join(knownGlobalSchemaVersions, ", "))
		}
		globalDst := filepath.Join(dataDir, "global.db")
		if _, err := os.Stat(globalDst); err == nil && !force {
			return RestoreAllResult{}, fmt.Errorf("global database %s already exists; pass --force to replace it", globalDst)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return RestoreAllResult{}, fmt.Errorf("stat global database: %w", err)
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return RestoreAllResult{}, fmt.Errorf("create data directory: %w", err)
		}
		if err := copyFile(globalSrc, globalDst); err != nil {
			return RestoreAllResult{}, fmt.Errorf("restore global database: %w", err)
		}
		result.GlobalDatabase = globalDst
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreAllResult{}, fmt.Errorf("stat backup global database: %w", err)
	}

	for _, projectID := range manifest.Projects {
		projectDir := filepath.Join(inputDir, "projects", projectID)
		project, err := RestoreProject(ctx, projectDir, dataDir, projectID, force)
		if err != nil {
			return RestoreAllResult{}, err
		}
		result.Projects = append(result.Projects, project)
	}
	return result, nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// untarDirectory extracts a tar written by tarDirectory into destDir,
// rejecting absolute paths and parent-directory escapes.
func untarDirectory(tarPath, destDir string) error {
	in, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	reader := tar.NewReader(in)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(header.Name)
		if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
			return fmt.Errorf("archive entry %q escapes the target directory", header.Name)
		}
		target := filepath.Join(destDir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0o755)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, reader)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type %d", header.Name, header.Typeflag)
		}
	}
}
