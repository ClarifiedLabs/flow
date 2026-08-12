// Package backup implements operator-facing backup and restore of a Flow
// coordinator data directory. A project backup holds a consistent SQLite
// snapshot (VACUUM INTO), a git bundle of the bare exchange repository, a tar
// of task attachments, and a manifest. Backups are crash-consistent rather
// than atomic across components: the database snapshot is consistent on its
// own, while the exchange refs and attachments may have advanced past it.
package backup

import (
	"archive/tar"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/version"
)

// FormatVersion is the backup layout version recorded as flow_format in every
// manifest. Restore refuses any other value.
const FormatVersion = 1

// Manifest kinds.
const (
	KindProject = "project"
	KindFull    = "full"
)

// knownProjectSchemaVersions and knownGlobalSchemaVersions mirror the
// storage_format markers internal/db enforces when opening a database
// (project flow.db is "7", coordinator global.db is "6"). A backup written by
// a build with a newer marker is refused with an actionable error.
var (
	knownProjectSchemaVersions = []string{"7"}
	knownGlobalSchemaVersions  = []string{"6"}
)

// gitOpTimeout bounds every git subprocess (bundle create, clone) so a wedged
// repository cannot pin an operator run forever.
const gitOpTimeout = 10 * time.Minute

// Manifest is the manifest.json written into every backup directory.
type Manifest struct {
	FlowFormat  int       `json:"flow_format"`
	Kind        string    `json:"kind"`
	ProjectID   string    `json:"project_id,omitempty"`
	ProjectName string    `json:"project_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	FlowVersion string    `json:"flow_version"`
	// SchemaVersion is the source database's storage_format marker; restore
	// accepts only the values this build can open.
	SchemaVersion string `json:"schema_version"`
	// Projects lists the project IDs in a full (KindFull) backup.
	Projects []string `json:"projects,omitempty"`
}

// ProjectBackup describes one completed project backup.
type ProjectBackup struct {
	Manifest Manifest
	Dir      string
	// Artifacts lists the files written, relative to Dir.
	Artifacts []string
}

// FullBackup describes a completed whole-data-dir backup.
type FullBackup struct {
	Manifest Manifest
	Dir      string
	Projects []ProjectBackup
	// GlobalDatabase reports whether the coordinator global.db was captured.
	GlobalDatabase bool
}

// BackupProject writes a crash-consistent backup of one project to outDir.
// The backup is staged in outDir.tmp and renamed into place, so outDir only
// ever appears complete; an existing non-empty outDir is refused. Safe to run
// against a live server: the SQLite snapshot is taken with VACUUM INTO.
func BackupProject(ctx context.Context, dataDir, projectID, outDir string) (ProjectBackup, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ProjectBackup{}, errors.New("project id is required")
	}
	if _, err := os.Stat(flowgit.ProjectDatabasePath(dataDir, projectID)); err != nil {
		return ProjectBackup{}, fmt.Errorf("project %q database: %w", projectID, err)
	}
	names := lookupProjectNames(ctx, dataDir)

	tmp, err := stageOutputDir(outDir)
	if err != nil {
		return ProjectBackup{}, err
	}
	result, err := backupProjectInto(ctx, dataDir, projectID, tmp, names)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return ProjectBackup{}, err
	}
	if err := finalizeOutputDir(tmp, outDir); err != nil {
		return ProjectBackup{}, err
	}
	result.Dir = outDir
	return result, nil
}

// BackupAll backs up every project under <dataDir>/projects plus the
// coordinator global database into outDir: one projects/<id>/ directory per
// project, global.db, and a top-level manifest.json listing the projects.
func BackupAll(ctx context.Context, dataDir, outDir string) (FullBackup, error) {
	projectsDir := filepath.Join(dataDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return FullBackup{}, fmt.Errorf("list projects: %w", err)
	}
	var projectIDs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(projectsDir, entry.Name(), "flow.db")); statErr == nil {
			projectIDs = append(projectIDs, entry.Name())
		}
	}
	sort.Strings(projectIDs)

	tmp, err := stageOutputDir(outDir)
	if err != nil {
		return FullBackup{}, err
	}
	succeed := false
	defer func() {
		if !succeed {
			_ = os.RemoveAll(tmp)
		}
	}()

	result := FullBackup{}
	names := lookupProjectNames(ctx, dataDir)
	for _, projectID := range projectIDs {
		dest := filepath.Join(tmp, "projects", projectID)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return FullBackup{}, fmt.Errorf("create project backup directory: %w", err)
		}
		project, err := backupProjectInto(ctx, dataDir, projectID, dest, names)
		if err != nil {
			return FullBackup{}, err
		}
		project.Dir = filepath.Join("projects", projectID)
		result.Projects = append(result.Projects, project)
	}

	manifest := Manifest{
		FlowFormat:  FormatVersion,
		Kind:        KindFull,
		CreatedAt:   time.Now().UTC(),
		FlowVersion: version.Current().String(),
		Projects:    projectIDs,
	}
	globalPath := filepath.Join(dataDir, "global.db")
	if _, err := os.Stat(globalPath); err == nil {
		schemaVersion, err := vacuumInto(ctx, globalPath, filepath.Join(tmp, "global.db"), true)
		if err != nil {
			return FullBackup{}, fmt.Errorf("back up global database: %w", err)
		}
		manifest.SchemaVersion = schemaVersion
		result.GlobalDatabase = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return FullBackup{}, fmt.Errorf("stat global database: %w", err)
	}
	if err := writeManifest(tmp, manifest); err != nil {
		return FullBackup{}, err
	}
	if err := finalizeOutputDir(tmp, outDir); err != nil {
		return FullBackup{}, err
	}
	succeed = true
	result.Manifest = manifest
	result.Dir = outDir
	return result, nil
}

// backupProjectInto writes the project backup directly into destDir, which the
// caller owns (a staging directory that is cleaned up on failure).
func backupProjectInto(ctx context.Context, dataDir, projectID, destDir string, names map[string]string) (ProjectBackup, error) {
	projectDir := flowgit.ProjectDir(dataDir, projectID)
	result := ProjectBackup{}

	schemaVersion, err := vacuumInto(ctx, filepath.Join(projectDir, "flow.db"), filepath.Join(destDir, "flow.db"), false)
	if err != nil {
		return ProjectBackup{}, fmt.Errorf("back up project %q database: %w", projectID, err)
	}
	result.Artifacts = append(result.Artifacts, "flow.db")

	// The exchange bundle captures every ref but not the installed hooks; the
	// docs direct operators to re-run flow init after a restore to reinstall
	// them. An exchange with no refs yet produces no bundle (git refuses to
	// create an empty one).
	exchangeDir := filepath.Join(projectDir, "exchange.git")
	if _, err := os.Stat(exchangeDir); err == nil {
		refs, err := gitOutput(ctx, "", "--git-dir", exchangeDir, "for-each-ref", "--format=%(refname)")
		if err != nil {
			return ProjectBackup{}, fmt.Errorf("inspect project %q exchange: %w", projectID, err)
		}
		if refs != "" {
			bundlePath := filepath.Join(destDir, "exchange.bundle")
			if err := runGit(ctx, "", "--git-dir", exchangeDir, "bundle", "create", bundlePath, "--all"); err != nil {
				return ProjectBackup{}, fmt.Errorf("bundle project %q exchange: %w", projectID, err)
			}
			result.Artifacts = append(result.Artifacts, "exchange.bundle")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ProjectBackup{}, fmt.Errorf("stat project %q exchange: %w", projectID, err)
	}

	attachmentsDir := filepath.Join(projectDir, "attachments")
	if entries, err := os.ReadDir(attachmentsDir); err == nil && len(entries) > 0 {
		tarPath := filepath.Join(destDir, "attachments.tar")
		if err := tarDirectory(attachmentsDir, tarPath); err != nil {
			return ProjectBackup{}, fmt.Errorf("archive project %q attachments: %w", projectID, err)
		}
		result.Artifacts = append(result.Artifacts, "attachments.tar")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ProjectBackup{}, fmt.Errorf("read project %q attachments: %w", projectID, err)
	}

	manifest := Manifest{
		FlowFormat:    FormatVersion,
		Kind:          KindProject,
		ProjectID:     projectID,
		ProjectName:   names[projectID],
		CreatedAt:     time.Now().UTC(),
		FlowVersion:   version.Current().String(),
		SchemaVersion: schemaVersion,
	}
	if err := writeManifest(destDir, manifest); err != nil {
		return ProjectBackup{}, err
	}
	result.Manifest = manifest
	result.Artifacts = append(result.Artifacts, "manifest.json")
	return result, nil
}

// vacuumInto snapshots the SQLite database at srcPath to destPath with VACUUM
// INTO and returns the source database's storage_format marker. The source is
// opened through the normal flowdb path so the snapshot is consistent even
// against a live server in WAL mode.
func vacuumInto(ctx context.Context, srcPath, destPath string, global bool) (string, error) {
	var store *flowdb.Store
	var err error
	if global {
		store, err = flowdb.OpenGlobal(ctx, srcPath)
	} else {
		store, err = flowdb.Open(ctx, srcPath)
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = store.Close() }()

	schemaVersion, err := readStorageFormat(ctx, store.DB())
	if err != nil {
		return "", err
	}
	quoted := strings.ReplaceAll(destPath, "'", "''")
	if _, err := store.DB().ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return "", fmt.Errorf("snapshot %s: %w", srcPath, err)
	}
	return schemaVersion, nil
}

func readStorageFormat(ctx context.Context, db *sql.DB) (string, error) {
	var value string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM app_metadata WHERE key = 'storage_format'`).Scan(&value); err != nil {
		return "", fmt.Errorf("read database storage format: %w", err)
	}
	return value, nil
}

// lookupProjectNames best-effort reads project display names from the
// coordinator global database. The global database may not exist (backing up
// a standalone project dir), so any failure yields an empty map and the
// manifest simply omits project_name.
func lookupProjectNames(ctx context.Context, dataDir string) map[string]string {
	names := map[string]string{}
	globalPath := filepath.Join(dataDir, "global.db")
	if _, err := os.Stat(globalPath); err != nil {
		return names
	}
	store, err := flowdb.OpenGlobal(ctx, globalPath)
	if err != nil {
		return names
	}
	defer func() { _ = store.Close() }()
	rows, err := store.DB().QueryContext(ctx, "SELECT id, name FROM projects")
	if err != nil {
		return names
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err == nil {
			names[id] = name
		}
	}
	return names
}

// stageOutputDir prepares outDir.tmp for a backup, refusing a non-empty
// outDir so a previous backup is never overwritten in place.
func stageOutputDir(outDir string) (string, error) {
	outDir = strings.TrimSpace(outDir)
	if outDir == "" {
		return "", errors.New("output directory is required")
	}
	empty, err := dirIsEmptyOrMissing(outDir)
	if err != nil {
		return "", err
	}
	if !empty {
		return "", fmt.Errorf("output directory %s already exists and is not empty; choose a fresh path", outDir)
	}
	tmp := outDir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return "", fmt.Errorf("clear stale staging directory: %w", err)
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	return tmp, nil
}

// finalizeOutputDir atomically publishes the staged backup by renaming it over
// the (missing or empty) output directory.
func finalizeOutputDir(tmp, outDir string) error {
	empty, err := dirIsEmptyOrMissing(outDir)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if !empty {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("output directory %s appeared during backup; refusing to overwrite", outDir)
	}
	if err := os.RemoveAll(outDir); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("clear empty output directory: %w", err)
	}
	if err := os.Rename(tmp, outDir); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("publish backup to %s: %w", outDir, err)
	}
	return nil
}

func dirIsEmptyOrMissing(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read directory %s: %w", path, err)
	}
	return len(entries) == 0, nil
}

func writeManifest(dir string, manifest Manifest) error {
	contents, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(contents, '\n'), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// LoadManifest reads the manifest.json from a backup directory.
func LoadManifest(dir string) (Manifest, error) {
	contents, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse backup manifest: %w", err)
	}
	return manifest, nil
}

// tarDirectory streams srcDir into a tar at destPath. Entry names are sorted
// so identical trees produce identical archives.
func tarDirectory(srcDir, destPath string) (err error) {
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close archive: %w", closeErr)
		}
	}()
	writer := tar.NewWriter(out)
	defer func() {
		if closeErr := writer.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("finish archive: %w", closeErr)
		}
	}()

	var paths []string
	walkErr := filepath.WalkDir(srcDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == srcDir {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	sort.Strings(paths)

	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue // sockets, devices, and symlinks are not restorable state
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			continue
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, in)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	_, err := gitCommand(ctx, dir, args...)
	return err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := gitCommand(ctx, dir, args...)
	return strings.TrimSpace(output), err
}

func gitCommand(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("git %s timed out: %w", strings.Join(args, " "), ctxErr)
		}
		return "", fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
