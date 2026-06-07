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
)

// Project is a row in the coordinator-wide projects registry. Each project
// owns a data directory with its own SQLite database and exchange remote.
type Project struct {
	ID           string
	Name         string
	RepoPath     string
	BaseBranch   string
	ExchangeName string
	ExchangeURL  string
	ExchangePath string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewProjectID allocates a random project identifier. Project IDs are not
// derived from the repo path: the repo may live on another machine than the
// coordinator, so the path is advisory metadata rather than identity.
func NewProjectID() (string, error) {
	return randomPrefixedID("p")
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

// Insert stores a new project. The requested name is deduplicated with a
// numeric suffix when already taken; the stored project is returned.
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

	baseName := project.Name
	for attempt := 2; ; attempt++ {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO projects (
	id,
	name,
	repo_path,
	base_branch,
	exchange_name,
	exchange_url,
	exchange_path,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			project.ID,
			project.Name,
			sqlitex.NullableNonEmptyString(project.RepoPath),
			project.BaseBranch,
			project.ExchangeName,
			project.ExchangeURL,
			sqlitex.NullableNonEmptyString(project.ExchangePath),
			formatTime(project.CreatedAt),
			formatTime(project.UpdatedAt),
		)
		if err == nil {
			return project, nil
		}
		if strings.Contains(err.Error(), "projects.name") {
			project.Name = fmt.Sprintf("%s-%d", baseName, attempt)
			continue
		}
		if strings.Contains(err.Error(), "projects.repo_path") {
			return Project{}, ErrProjectRepoPathExists
		}
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
}

func (s *ProjectService) List(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	id,
	name,
	COALESCE(repo_path, ''),
	base_branch,
	exchange_name,
	exchange_url,
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
	exchange_url,
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
	exchange_url,
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
	exchange_url,
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
	exchange_url,
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
		&project.ExchangeURL,
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
	project.ExchangeURL = strings.TrimSpace(project.ExchangeURL)
	project.ExchangePath = strings.TrimSpace(project.ExchangePath)

	if project.ID == "" {
		return Project{}, errors.New("project id is required")
	}
	if project.Name == "" {
		return Project{}, errors.New("project name is required")
	}
	if project.BaseBranch == "" {
		return Project{}, errors.New("project base branch is required")
	}
	if project.ExchangeURL == "" {
		return Project{}, errors.New("project exchange url is required")
	}
	if project.ExchangeName == "" {
		project.ExchangeName = "flow"
	}

	return project, nil
}

// stampProjectPayload records the owning project and its exchange location
// on a job payload so the worker can clone the right exchange and expose the
// project to agent sessions.
func stampProjectPayload(payload map[string]any, project Project) {
	if payload == nil {
		return
	}
	payload["project_id"] = project.ID
	payload["project_name"] = project.Name
	payload["exchange_url"] = project.ExchangeURL
}
