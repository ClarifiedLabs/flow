package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

// Directory manages worker registration and liveness in the coordinator's
// global database. Job and lease state stays in each project's database and
// is managed by the per-project Service.
type Directory struct {
	db  *sql.DB
	now func() time.Time
}

func NewDirectory(database *sql.DB) *Directory {
	return &Directory{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (d *Directory) RegisterWorker(ctx context.Context, input RegisterWorkerInput) (Worker, error) {
	input.ID = strings.TrimSpace(input.ID)
	if input.ID == "" {
		return Worker{}, errors.New("worker id is required")
	}
	if input.CapacityPersistentAgent < 0 || input.CapacityEphemeral < 0 {
		return Worker{}, errors.New("worker capacity cannot be negative")
	}
	labels, err := normalizeLabelsJSON(input.Labels)
	if err != nil {
		return Worker{}, err
	}
	taints, err := encodeTaints(input.Taints)
	if err != nil {
		return Worker{}, err
	}
	harnessModels, err := encodeHarnessModels(input.HarnessModels)
	if err != nil {
		return Worker{}, err
	}

	now := d.now().UTC()
	var expiresAt *time.Time
	if input.HeartbeatTTL > 0 {
		value := now.Add(input.HeartbeatTTL)
		expiresAt = &value
	}

	if _, err := d.db.ExecContext(ctx, `
INSERT INTO workers (
	id,
	labels_json,
	taints_json,
	harness_models_json,
	capacity_persistent_agent,
	capacity_ephemeral,
	status,
	created_at,
	updated_at,
	last_heartbeat_at,
	expires_at
) VALUES (?, ?, ?, ?, ?, ?, 'registered', ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	labels_json = excluded.labels_json,
	taints_json = excluded.taints_json,
	harness_models_json = excluded.harness_models_json,
	capacity_persistent_agent = excluded.capacity_persistent_agent,
	capacity_ephemeral = excluded.capacity_ephemeral,
	status = 'registered',
	updated_at = excluded.updated_at,
	last_heartbeat_at = excluded.last_heartbeat_at,
	expires_at = excluded.expires_at`,
		input.ID,
		labels,
		taints,
		harnessModels,
		input.CapacityPersistentAgent,
		input.CapacityEphemeral,
		formatTime(now),
		formatTime(now),
		formatTime(now),
		nullableTime(expiresAt),
	); err != nil {
		return Worker{}, fmt.Errorf("register worker: %w", err)
	}

	return d.GetWorker(ctx, input.ID)
}

func (d *Directory) HeartbeatWorker(ctx context.Context, workerID string, ttl time.Duration) (Worker, error) {
	now := d.now().UTC()
	var expiresAt *time.Time
	if ttl > 0 {
		value := now.Add(ttl)
		expiresAt = &value
	}

	result, err := d.db.ExecContext(ctx, `
UPDATE workers
SET status = 'registered',
	updated_at = ?,
	last_heartbeat_at = ?,
	expires_at = ?
WHERE id = ?`,
		formatTime(now),
		formatTime(now),
		nullableTime(expiresAt),
		workerID,
	)
	if err != nil {
		return Worker{}, fmt.Errorf("heartbeat worker: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Worker{}, fmt.Errorf("read heartbeat rows affected: %w", err)
	}
	if rows == 0 {
		return Worker{}, sql.ErrNoRows
	}

	return d.GetWorker(ctx, workerID)
}

func (d *Directory) GetWorker(ctx context.Context, workerID string) (Worker, error) {
	row := d.db.QueryRowContext(ctx, `
SELECT
	id,
	labels_json,
	taints_json,
	harness_models_json,
	capacity_persistent_agent,
	capacity_ephemeral,
	status,
	created_at,
	updated_at,
	last_heartbeat_at,
	expires_at
FROM workers
WHERE id = ?`, workerID)

	return scanWorker(row)
}

func (d *Directory) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT
	id,
	labels_json,
	taints_json,
	harness_models_json,
	capacity_persistent_agent,
	capacity_ephemeral,
	status,
	created_at,
	updated_at,
	last_heartbeat_at,
	expires_at
FROM workers
ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()

	var workers []Worker
	for rows.Next() {
		worker, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workers: %w", err)
	}

	return workers, nil
}
