package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/terminal"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func (s *SessionService) RegisterJobTerminalTarget(ctx context.Context, jobID string, leaseID string, targetURL string, tmuxSocketPaths ...string) (JobTerminal, error) {
	jobID = strings.TrimSpace(jobID)
	leaseID = strings.TrimSpace(leaseID)
	if jobID == "" {
		return JobTerminal{}, errors.New("job id is required")
	}
	if leaseID == "" {
		return JobTerminal{}, errors.New("lease id is required")
	}
	if err := s.validateLiveJobLease(ctx, jobID, leaseID); err != nil {
		return JobTerminal{}, err
	}
	normalized, err := terminal.NormalizeProxyTargetURL(targetURL)
	if err != nil {
		return JobTerminal{}, err
	}
	tmuxSocketPath := firstOptionalString(tmuxSocketPaths)
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO job_terminals (
	job_id,
	lease_id,
	target_url,
	tmux_socket_path,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(job_id) DO UPDATE SET
	lease_id = excluded.lease_id,
	target_url = excluded.target_url,
	tmux_socket_path = excluded.tmux_socket_path,
	updated_at = excluded.updated_at`,
		jobID,
		leaseID,
		normalized,
		tmuxSocketPath,
		formatTime(now),
		formatTime(now),
	); err != nil {
		return JobTerminal{}, fmt.Errorf("register job terminal target: %w", err)
	}

	return s.JobTerminalTarget(ctx, jobID)
}

func (s *SessionService) JobTerminalTarget(ctx context.Context, jobID string) (JobTerminal, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return JobTerminal{}, errors.New("job id is required")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT job_id, lease_id, target_url, tmux_socket_path, created_at, updated_at
FROM job_terminals
WHERE job_id = ?`, jobID)

	var target JobTerminal
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&target.JobID,
		&target.LeaseID,
		&target.TargetURL,
		&target.TmuxSocketPath,
		&createdAt,
		&updatedAt,
	); err != nil {
		return JobTerminal{}, err
	}
	if err := s.validateLiveJobLease(ctx, target.JobID, target.LeaseID); err != nil {
		return JobTerminal{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return JobTerminal{}, err
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return JobTerminal{}, err
	}
	target.CreatedAt = parsedCreatedAt
	target.UpdatedAt = parsedUpdatedAt

	return target, nil
}

func (s *SessionService) JobTerminalAvailable(ctx context.Context, jobID string) (bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return false, errors.New("job id is required")
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM jobs j
JOIN job_terminals jt ON jt.job_id = j.id
JOIN leases l ON l.id = jt.lease_id AND l.job_id = j.id
WHERE j.id = ?
	AND j.state = ?
	AND l.released_at IS NULL
	AND l.expires_at > ?`,
		jobID,
		string(flowworker.JobRunning),
		formatTime(s.now().UTC()),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check job terminal availability: %w", err)
	}

	return count > 0, nil
}

func (s *SessionService) CreateJobTerminalAccess(ctx context.Context, jobID string, ttl time.Duration) (JobTerminalAccess, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return JobTerminalAccess{}, errors.New("job id is required")
	}
	if ttl <= 0 {
		return JobTerminalAccess{}, errors.New("terminal access ttl is required")
	}
	if _, err := s.JobTerminalTarget(ctx, jobID); err != nil {
		return JobTerminalAccess{}, err
	}
	token, err := randomCredentialToken()
	if err != nil {
		return JobTerminalAccess{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(ttl)
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO job_terminal_access_tokens (
	token_hash,
	job_id,
	expires_at,
	created_at
) VALUES (?, ?, ?, ?)`,
		HashToken(token),
		jobID,
		formatTime(expiresAt),
		formatTime(now),
	); err != nil {
		return JobTerminalAccess{}, fmt.Errorf("create job terminal access token: %w", err)
	}

	return JobTerminalAccess{
		JobID:     jobID,
		Token:     token,
		LoginPath: jobTerminalLoginPath(jobID, token),
		ExpiresAt: expiresAt,
	}, nil
}

func (s *SessionService) ValidateJobTerminalAccess(ctx context.Context, jobID string, token string) error {
	jobID = strings.TrimSpace(jobID)
	token = strings.TrimSpace(token)
	if jobID == "" || token == "" {
		return ErrInvalidCredential
	}
	if _, err := s.JobTerminalTarget(ctx, jobID); err != nil {
		return err
	}
	var expiresAtText string
	if err := s.db.QueryRowContext(ctx, `
SELECT expires_at
FROM job_terminal_access_tokens
WHERE token_hash = ?
	AND job_id = ?`,
		HashToken(token),
		jobID,
	).Scan(&expiresAtText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCredential
		}
		return fmt.Errorf("validate job terminal access token: %w", err)
	}
	expiresAt, err := parseTime(expiresAtText)
	if err != nil {
		return err
	}
	if !s.now().UTC().Before(expiresAt) {
		return ErrInvalidCredential
	}

	return nil
}

func (s *SessionService) validateLiveJobLease(ctx context.Context, jobID string, leaseID string) error {
	if s.workers == nil {
		return errors.New("worker service is required")
	}
	job, err := s.workers.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	lease, err := s.workers.GetLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if lease.JobID != job.ID {
		return errors.New("lease does not belong to job")
	}
	if job.State != flowworker.JobRunning || lease.ReleasedAt != nil || !s.now().UTC().Before(lease.ExpiresAt) {
		return errors.New("job terminal is not live")
	}

	return nil
}

func jobTerminalLoginPath(jobID string, token string) string {
	query := url.Values{}
	query.Set("token", token)
	return terminal.JobTerminalProxyPath(jobID) + "-login?" + query.Encode()
}

func firstOptionalString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
