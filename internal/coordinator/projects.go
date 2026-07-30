package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrProjectNotFound       = errors.New("project not found")
	ErrProjectRepoPathExists = errors.New("a project is already registered for this repo path")
	ErrProjectNameExists     = errors.New("a project already uses this name")
	ErrProjectIDExists       = errors.New("a project already uses this normalized key")
)

const maxProjectKeyLength = 48

// Project is a row in the coordinator-wide projects registry. Each project
// owns a data directory with its own SQLite database and exchange remote.
type Project struct {
	ID           string
	Name         string
	RepoPath     string
	BaseBranch   string
	ExchangeName string
	ExchangePath string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ProjectIDFromName derives the stable, human-readable project identifier
// used by projects, tasks, URLs, and Git refs. Display names remain unchanged;
// only ASCII letters and digits are retained in the normalized key.
func ProjectIDFromName(name string) (string, error) {
	key, err := projectKeyFromName(name)
	if err != nil {
		return "", err
	}

	return "p-" + key, nil
}

func projectKeyFromName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("project name is required")
	}

	var builder strings.Builder
	pendingSeparator := false
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if pendingSeparator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			builder.WriteByte(byte(r + ('a' - 'A')))
			pendingSeparator = false
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingSeparator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			builder.WriteRune(r)
			pendingSeparator = false
		default:
			pendingSeparator = builder.Len() > 0
		}
	}

	key := builder.String()
	if key == "" {
		return "", errors.New("project name must contain an ASCII letter or digit")
	}
	if len(key) > maxProjectKeyLength {
		return "", fmt.Errorf("normalized project key %q is %d characters; maximum is %d", key, len(key), maxProjectKeyLength)
	}

	return key, nil
}

func projectKeyFromID(projectID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	if !strings.HasPrefix(projectID, "p-") {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	key := strings.TrimPrefix(projectID, "p-")
	normalized, err := projectKeyFromName(key)
	if err != nil || normalized != key {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}

	return key, nil
}

// ProjectIDFromTaskID returns the project encoded in a canonical task ID.
func ProjectIDFromTaskID(taskID string) (string, bool) {
	taskID = strings.TrimSpace(taskID)
	if !strings.HasPrefix(taskID, "t-") {
		return "", false
	}
	rest := strings.TrimPrefix(taskID, "t-")
	separator := strings.LastIndexByte(rest, '-')
	if separator <= 0 {
		return "", false
	}
	key, sequence := rest[:separator], rest[separator+1:]
	if len(sequence) < 4 {
		return "", false
	}
	for _, r := range sequence {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	projectID := "p-" + key
	if _, err := projectKeyFromID(projectID); err != nil {
		return "", false
	}

	return projectID, true
}

// ProjectIDFromFeatureID returns the project encoded in a canonical feature ID.
func ProjectIDFromFeatureID(featureID string) (string, bool) {
	featureID = strings.TrimSpace(featureID)
	if !strings.HasPrefix(featureID, "f-") {
		return "", false
	}
	rest := strings.TrimPrefix(featureID, "f-")
	separator := strings.LastIndexByte(rest, '-')
	if separator <= 0 {
		return "", false
	}
	key, sequence := rest[:separator], rest[separator+1:]
	if len(sequence) < 4 {
		return "", false
	}
	for _, r := range sequence {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	projectID := "p-" + key
	if _, err := projectKeyFromID(projectID); err != nil {
		return "", false
	}

	return projectID, true
}

// ProjectService manages the projects registry in the coordinator's global
// database.
type ProjectService struct {
	db  *sql.DB
	now func() time.Time
}

func NewProjectService(database *sql.DB) *ProjectService {
	return &ProjectService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

// Insert stores a new project. Names and their normalized identifiers must be
// unique; callers surface collisions so users can choose an intentional name.
func (s *ProjectService) Insert(ctx context.Context, project Project) (Project, error) {
	project, err := normalizeProject(project)
	if err != nil {
		return Project{}, err
	}

	if project.RepoPath != "" {
		_, err := s.GetByRepoPath(ctx, project.RepoPath)
		switch {
		case err == nil:
			return Project{}, ErrProjectRepoPathExists
		case !errors.Is(err, ErrProjectNotFound):
			return Project{}, err
		}
	}

	now := s.now().UTC()
	project.CreatedAt = now
	project.UpdatedAt = now

	_, err = s.db.ExecContext(ctx, `
INSERT INTO projects (
	id,
	name,
	repo_path,
	base_branch,
	exchange_name,
	exchange_path,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		project.ID,
		project.Name,
		sqlitex.NullableNonEmptyString(project.RepoPath),
		project.BaseBranch,
		project.ExchangeName,
		sqlitex.NullableNonEmptyString(project.ExchangePath),
		formatTime(project.CreatedAt),
		formatTime(project.UpdatedAt),
	)
	if err == nil {
		return project, nil
	}
	if strings.Contains(err.Error(), "projects.id") {
		return Project{}, ErrProjectIDExists
	}
	if strings.Contains(err.Error(), "projects.name") {
		return Project{}, ErrProjectNameExists
	}
	if strings.Contains(err.Error(), "projects.repo_path") {
		return Project{}, ErrProjectRepoPathExists
	}
	return Project{}, fmt.Errorf("insert project: %w", err)
}

func (s *ProjectService) List(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	id,
	name,
	COALESCE(repo_path, ''),
	base_branch,
	exchange_name,
	COALESCE(exchange_path, ''),
	created_at,
	updated_at
FROM projects
ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		project, err := scanProject(rows.Scan)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}

	return projects, nil
}

func (s *ProjectService) Get(ctx context.Context, id string) (Project, error) {
	return s.getOne(ctx, `
SELECT
	id,
	name,
	COALESCE(repo_path, ''),
	base_branch,
	exchange_name,
	COALESCE(exchange_path, ''),
	created_at,
	updated_at
FROM projects
WHERE id = ?`, strings.TrimSpace(id))
}

func (s *ProjectService) GetByName(ctx context.Context, name string) (Project, error) {
	return s.getOne(ctx, `
SELECT
	id,
	name,
	COALESCE(repo_path, ''),
	base_branch,
	exchange_name,
	COALESCE(exchange_path, ''),
	created_at,
	updated_at
FROM projects
WHERE name = ?`, strings.TrimSpace(name))
}

func (s *ProjectService) GetByRepoPath(ctx context.Context, repoPath string) (Project, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return Project{}, ErrProjectNotFound
	}

	return s.getOne(ctx, `
SELECT
	id,
	name,
	COALESCE(repo_path, ''),
	base_branch,
	exchange_name,
	COALESCE(exchange_path, ''),
	created_at,
	updated_at
FROM projects
WHERE repo_path = ?`, repoPath)
}

func (s *ProjectService) GetByExchangePath(ctx context.Context, exchangePath string) (Project, error) {
	exchangePath = strings.TrimSpace(exchangePath)
	if exchangePath == "" {
		return Project{}, ErrProjectNotFound
	}

	return s.getOne(ctx, `
SELECT
	id,
	name,
	COALESCE(repo_path, ''),
	base_branch,
	exchange_name,
	COALESCE(exchange_path, ''),
	created_at,
	updated_at
FROM projects
WHERE exchange_path = ?`, exchangePath)
}

func (s *ProjectService) getOne(ctx context.Context, query string, value string) (Project, error) {
	row := s.db.QueryRowContext(ctx, query, value)

	project, err := scanProject(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrProjectNotFound
	}
	if err != nil {
		return Project{}, err
	}

	return project, nil
}

func scanProject(scan func(...any) error) (Project, error) {
	var project Project
	var createdAt string
	var updatedAt string
	if err := scan(
		&project.ID,
		&project.Name,
		&project.RepoPath,
		&project.BaseBranch,
		&project.ExchangeName,
		&project.ExchangePath,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, err
		}
		return Project{}, fmt.Errorf("scan project: %w", err)
	}

	var err error
	if project.CreatedAt, err = parseTime(createdAt); err != nil {
		return Project{}, err
	}
	if project.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Project{}, err
	}

	return project, nil
}

func normalizeProject(project Project) (Project, error) {
	project.ID = strings.TrimSpace(project.ID)
	project.Name = strings.TrimSpace(project.Name)
	project.RepoPath = strings.TrimSpace(project.RepoPath)
	project.BaseBranch = strings.TrimSpace(project.BaseBranch)
	project.ExchangeName = strings.TrimSpace(project.ExchangeName)
	project.ExchangePath = strings.TrimSpace(project.ExchangePath)

	if project.ID == "" {
		return Project{}, errors.New("project id is required")
	}
	if project.Name == "" {
		return Project{}, errors.New("project name is required")
	}
	expectedID, err := ProjectIDFromName(project.Name)
	if err != nil {
		return Project{}, err
	}
	if project.ID != expectedID {
		return Project{}, fmt.Errorf("project id %q does not match normalized name id %q", project.ID, expectedID)
	}
	if project.BaseBranch == "" {
		return Project{}, errors.New("project base branch is required")
	}
	if project.ExchangeName == "" {
		project.ExchangeName = "flow"
	}

	return project, nil
}

// stampProjectPayload records the owning project on a job payload so the
// worker can derive its network-local exchange URL and expose the project to
// agent sessions.
func stampProjectPayload(payload map[string]any, project Project) {
	if payload == nil {
		return
	}
	payload["project_id"] = project.ID
	payload["project_name"] = project.Name
}
