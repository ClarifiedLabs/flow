// Package sqlitex holds the SQLite persistence primitives shared across Flow's
// stores: the single-writer BEGIN IMMEDIATE transaction wrapper, the
// lexicographically-sortable timestamp encoding, and small null-handling
// helpers. Centralizing them keeps the on-disk storage contract identical across
// the coordinator, worker, and lifecycle engines.
package sqlitex

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Tx is the single-writer BEGIN IMMEDIATE transaction used by Flow's stores. It
// holds a dedicated *sql.Conn so SQLite's write lock is taken up front rather
// than on first write, avoiding mid-transaction SQLITE_BUSY upgrades.
type Tx struct {
	conn *sql.Conn
	done bool
}

// BeginImmediate opens a connection and starts a BEGIN IMMEDIATE transaction.
func BeginImmediate(ctx context.Context, database *sql.DB) (*Tx, error) {
	conn, err := database.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &Tx{conn: conn}, nil
}

func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}

func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

func (tx *Tx) Commit(ctx context.Context) error {
	if tx.done {
		return nil
	}
	tx.done = true
	_, err := tx.conn.ExecContext(ctx, "COMMIT")
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}

	return closeErr
}

func (tx *Tx) Rollback() {
	if tx.done {
		return
	}
	tx.done = true
	_, _ = tx.conn.ExecContext(context.Background(), "ROLLBACK")
	_ = tx.conn.Close()
}

// timeStorageLayout is RFC3339 with fixed-width nanoseconds. RFC3339Nano trims
// trailing fractional zeros, which breaks lexicographic ordering of stored
// timestamp text within a second (".5Z" sorts after ".51Z"); a fixed width keeps
// string order identical to chronological order (timers, ORDER BY, and lease
// expiry sweeps all compare this timestamp text directly).
const timeStorageLayout = "2006-01-02T15:04:05.000000000Z07:00"

// UTCNow returns the current time in UTC. It is the default clock for stores
// that timestamp rows; tests inject their own clock instead.
func UTCNow() time.Time {
	return time.Now().UTC()
}

// FormatTime encodes t for storage using the sortable fixed-width layout.
func FormatTime(value time.Time) string {
	return value.UTC().Format(timeStorageLayout)
}

// ParseTime decodes a timestamp written by FormatTime back into UTC.
func ParseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", value, err)
	}

	return parsed.UTC(), nil
}

// NullableNonEmptyString returns the trimmed string for storage, or nil when it
// trims to empty so the column is written as SQL NULL.
func NullableNonEmptyString(value string) any {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}

	return trimmed
}

// NullableString is the *string form of NullableNonEmptyString: a nil pointer or
// blank value is written as SQL NULL, otherwise the trimmed string is stored.
func NullableString(value *string) any {
	if value == nil {
		return nil
	}

	return NullableNonEmptyString(*value)
}

// NullableStringPointer converts a scanned sql.NullString into a *string.
func NullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}

	return &value.String
}
