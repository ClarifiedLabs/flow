package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

const IdempotencyPendingStatus = 102

// IdempotencyPendingTTL bounds how long a pending (102) reservation may sit
// before an inbound retry is allowed to reclaim it. If the server crashes
// between Reserve and Complete/Cancel, the 102 row would otherwise wedge the
// key forever. The TTL must exceed the slowest legitimate in-flight idempotent
// mutation: an inline auto-merge is bounded by a 120s git-op timeout, so the
// TTL is 150s (not lower) to leave headroom and avoid reclaiming a reservation
// whose mutation is still legitimately running.
const IdempotencyPendingTTL = 150 * time.Second

type IdempotencyRecord struct {
	PrincipalKey   string
	IdempotencyKey string
	Method         string
	Path           string
	RequestHash    string
	StatusCode     int
	ResponseBody   []byte
	CreatedAt      time.Time
}

type IdempotencyService struct {
	db     *sql.DB
	now    func() time.Time
	lockMu sync.Mutex
	locks  map[string]*sync.Mutex
}

func NewIdempotencyService(database *sql.DB) *IdempotencyService {
	return &IdempotencyService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *IdempotencyService) Lock(principalKey string, idempotencyKey string) (func(), error) {
	principalKey = strings.TrimSpace(principalKey)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if principalKey == "" || idempotencyKey == "" {
		return nil, errors.New("principal key and idempotency key are required")
	}

	lockKey := principalKey + "\x00" + idempotencyKey
	s.lockMu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	lock := s.locks[lockKey]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[lockKey] = lock
	}
	s.lockMu.Unlock()

	lock.Lock()
	return lock.Unlock, nil
}

func (s *IdempotencyService) Lookup(ctx context.Context, principalKey string, idempotencyKey string) (IdempotencyRecord, bool, error) {
	principalKey = strings.TrimSpace(principalKey)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if principalKey == "" || idempotencyKey == "" {
		return IdempotencyRecord{}, false, errors.New("principal key and idempotency key are required")
	}

	var record IdempotencyRecord
	var responseBody string
	var createdAt string
	if err := s.db.QueryRowContext(ctx, `
SELECT principal_key, idempotency_key, method, path, request_hash, status_code, response_body, created_at
FROM idempotency_records
WHERE principal_key = ? AND idempotency_key = ?`,
		principalKey,
		idempotencyKey,
	).Scan(
		&record.PrincipalKey,
		&record.IdempotencyKey,
		&record.Method,
		&record.Path,
		&record.RequestHash,
		&record.StatusCode,
		&responseBody,
		&createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IdempotencyRecord{}, false, nil
		}
		return IdempotencyRecord{}, false, fmt.Errorf("lookup idempotency record: %w", err)
	}

	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	record.ResponseBody = []byte(responseBody)
	record.CreatedAt = parsedCreatedAt
	return record, true, nil
}

func (s *IdempotencyService) Reserve(ctx context.Context, record IdempotencyRecord) (IdempotencyRecord, bool, error) {
	record = normalizeIdempotencyRecord(record)
	if record.PrincipalKey == "" || record.IdempotencyKey == "" || record.Method == "" || record.Path == "" || record.RequestHash == "" {
		return IdempotencyRecord{}, false, errors.New("idempotency record is incomplete")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = s.now().UTC()
	}

	result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO idempotency_records (
	principal_key,
	idempotency_key,
	method,
	path,
	request_hash,
	status_code,
	response_body,
	created_at
) VALUES (?, ?, ?, ?, ?, ?, '', ?)`,
		record.PrincipalKey,
		record.IdempotencyKey,
		record.Method,
		record.Path,
		record.RequestHash,
		IdempotencyPendingStatus,
		formatTime(record.CreatedAt.UTC()),
	)
	if err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("reserve idempotency record: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("read idempotency reservation rows affected: %w", err)
	}
	if rowsAffected == 1 {
		record.StatusCode = IdempotencyPendingStatus
		record.ResponseBody = nil
		return record, true, nil
	}

	existing, found, err := s.Lookup(ctx, record.PrincipalKey, record.IdempotencyKey)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	if !found {
		return IdempotencyRecord{}, false, errors.New("idempotency reservation lost conflict row")
	}

	// Reclaim a stale pending reservation left behind by a crash between
	// Reserve and Complete/Cancel. The API handler holds the in-memory per-key
	// Lock across serveIdempotent, so same-host same-key requests are
	// serialized; the conditional DELETE (matching created_at) is defense in
	// depth against a concurrent fresh reservation reclaimed by another goroutine.
	if existing.StatusCode == IdempotencyPendingStatus && s.now().UTC().Sub(existing.CreatedAt) > IdempotencyPendingTTL {
		if _, err := s.db.ExecContext(ctx, `
DELETE FROM idempotency_records
WHERE principal_key = ?
	AND idempotency_key = ?
	AND status_code = ?
	AND created_at = ?`,
			existing.PrincipalKey,
			existing.IdempotencyKey,
			IdempotencyPendingStatus,
			formatTime(existing.CreatedAt.UTC()),
		); err != nil {
			return IdempotencyRecord{}, false, fmt.Errorf("reclaim stale idempotency reservation: %w", err)
		}

		result, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO idempotency_records (
	principal_key,
	idempotency_key,
	method,
	path,
	request_hash,
	status_code,
	response_body,
	created_at
) VALUES (?, ?, ?, ?, ?, ?, '', ?)`,
			record.PrincipalKey,
			record.IdempotencyKey,
			record.Method,
			record.Path,
			record.RequestHash,
			IdempotencyPendingStatus,
			formatTime(record.CreatedAt.UTC()),
		)
		if err != nil {
			return IdempotencyRecord{}, false, fmt.Errorf("reserve idempotency record: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return IdempotencyRecord{}, false, fmt.Errorf("read idempotency reservation rows affected: %w", err)
		}
		if rowsAffected == 1 {
			record.StatusCode = IdempotencyPendingStatus
			record.ResponseBody = nil
			return record, true, nil
		}

		// Another goroutine reclaimed first; return the now-current row.
		reclaimed, found, err := s.Lookup(ctx, record.PrincipalKey, record.IdempotencyKey)
		if err != nil {
			return IdempotencyRecord{}, false, err
		}
		if !found {
			return IdempotencyRecord{}, false, errors.New("idempotency reservation lost conflict row")
		}
		return reclaimed, false, nil
	}

	return existing, false, nil
}

func (s *IdempotencyService) Complete(ctx context.Context, record IdempotencyRecord) error {
	record = normalizeIdempotencyRecord(record)
	if record.PrincipalKey == "" || record.IdempotencyKey == "" || record.Method == "" || record.Path == "" || record.RequestHash == "" {
		return errors.New("idempotency record is incomplete")
	}
	if record.StatusCode < 200 || record.StatusCode > 299 {
		return fmt.Errorf("idempotency completion requires successful status code: %d", record.StatusCode)
	}

	result, err := s.db.ExecContext(ctx, `
UPDATE idempotency_records
SET status_code = ?,
	response_body = ?
WHERE principal_key = ?
	AND idempotency_key = ?
	AND method = ?
	AND path = ?
	AND request_hash = ?
		AND status_code = ?`,
		record.StatusCode,
		string(record.ResponseBody),
		record.PrincipalKey,
		record.IdempotencyKey,
		record.Method,
		record.Path,
		record.RequestHash,
		IdempotencyPendingStatus,
	)
	if err != nil {
		return fmt.Errorf("complete idempotency record: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read idempotency completion rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return errors.New("idempotency reservation was not completed")
	}

	return nil
}

func (s *IdempotencyService) Cancel(ctx context.Context, principalKey string, idempotencyKey string, method string, path string, requestHash string) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM idempotency_records
WHERE principal_key = ?
	AND idempotency_key = ?
	AND method = ?
	AND path = ?
	AND request_hash = ?
	AND status_code = ?`,
		strings.TrimSpace(principalKey),
		strings.TrimSpace(idempotencyKey),
		strings.TrimSpace(method),
		strings.TrimSpace(path),
		strings.TrimSpace(requestHash),
		IdempotencyPendingStatus,
	); err != nil {
		return fmt.Errorf("cancel idempotency reservation: %w", err)
	}

	return nil
}

func normalizeIdempotencyRecord(record IdempotencyRecord) IdempotencyRecord {
	record.PrincipalKey = strings.TrimSpace(record.PrincipalKey)
	record.IdempotencyKey = strings.TrimSpace(record.IdempotencyKey)
	record.Method = strings.TrimSpace(record.Method)
	record.Path = strings.TrimSpace(record.Path)
	record.RequestHash = strings.TrimSpace(record.RequestHash)
	return record
}

func RequestHash(method string, path string, body []byte) string {
	hasher := sha256.New()
	hasher.Write([]byte(strings.ToUpper(method)))
	hasher.Write([]byte{0})
	hasher.Write([]byte(path))
	hasher.Write([]byte{0})
	hasher.Write(body)
	return hex.EncodeToString(hasher.Sum(nil))
}
