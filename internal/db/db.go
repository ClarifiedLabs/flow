package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed migrations_global/*.sql
var globalMigrationFS embed.FS

type Store struct {
	db             *sql.DB
	path           string
	migrations     embed.FS
	migrationsGlob string
}

// Open opens a per-project database and applies the per-project migration set.
func Open(ctx context.Context, path string) (*Store, error) {
	return openWith(ctx, "sqlite3", path, migrationFS, "migrations/*.sql", "7")
}

// OpenGlobal opens the coordinator-wide database (projects registry, workers,
// tokens, web sessions) and applies the global migration set.
func OpenGlobal(ctx context.Context, path string) (*Store, error) {
	return openWith(ctx, "sqlite3", path, globalMigrationFS, "migrations_global/*.sql", "6")
}

// OpenWithDriver opens a per-project database through a named driver and applies
// the per-project migration set. Tests use it to wrap the real driver, for
// example to count the queries a read model issues.
func OpenWithDriver(ctx context.Context, driverName, path string) (*Store, error) {
	return openWith(ctx, driverName, path, migrationFS, "migrations/*.sql", "7")
}

func openWith(ctx context.Context, driverName, path string, migrations embed.FS, glob string, expectedStorageFormat string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}

	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	conn, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	conn.SetMaxOpenConns(1)

	store := &Store{db: conn, path: path, migrations: migrations, migrationsGlob: glob}
	if err := store.configure(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if expectedStorageFormat != "" {
		if err := store.validateExistingStorageFormat(ctx, expectedStorageFormat); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if err := store.Migrate(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if expectedStorageFormat != "" {
		if err := store.requireStorageFormat(ctx, expectedStorageFormat); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}

	return store, nil
}

// validateExistingStorageFormat rejects an incompatible database before
// migrations can reinterpret its schema. A completely empty database is valid
// and receives the current marker from the initial migration.
func (s *Store) validateExistingStorageFormat(ctx context.Context, expected string) error {
	var appMetadataTables int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'app_metadata'`).Scan(&appMetadataTables); err != nil {
		return fmt.Errorf("inspect database format: %w", err)
	}
	if appMetadataTables == 0 {
		var userTables int
		if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&userTables); err != nil {
			return fmt.Errorf("inspect database tables: %w", err)
		}
		if userTables == 0 {
			return nil
		}
		return incompatibleStorageFormatError(expected, "missing")
	}
	return s.requireStorageFormat(ctx, expected)
}

func (s *Store) requireStorageFormat(ctx context.Context, expected string) error {
	var actual string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM app_metadata WHERE key = 'storage_format'`).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return incompatibleStorageFormatError(expected, "missing")
	}
	if err != nil {
		return fmt.Errorf("read project database format: %w", err)
	}
	if actual != expected {
		return incompatibleStorageFormatError(expected, actual)
	}
	return nil
}

func incompatibleStorageFormatError(expected, actual string) error {
	return fmt.Errorf("incompatible database storage format %q (need %q); back up and recreate the Flow data directory", actual, expected)
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) configure(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return fmt.Errorf("configure sqlite busy timeout: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable sqlite foreign keys: %w", err)
	}

	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("enable sqlite WAL mode: %w", err)
	}

	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
)`); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	files, err := fs.Glob(s.migrations, s.migrationsGlob)
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(files)

	// SQLite's documented table-rebuild procedure requires foreign-key
	// enforcement to be disabled before the migration transaction begins. The
	// controlled migrations may then replace CHECK-constrained parent tables.
	// A full foreign_key_check below is the gate before any migration commits.
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("disable foreign keys for migrations: %w", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_, _ = s.db.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
		}
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer tx.Rollback()

	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".sql")
		applied, err := migrationApplied(ctx, tx, version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		contents, err := s.migrations.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
			return fmt.Errorf("record migration %s: %w", version, err)
		}
	}

	violations, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("check migrated foreign keys: %w", err)
	}
	if violations.Next() {
		var table, parent string
		var rowID any
		var foreignKeyID int64
		if err := violations.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			_ = violations.Close()
			return fmt.Errorf("scan migrated foreign key violation: %w", err)
		}
		_ = violations.Close()
		return fmt.Errorf("migration left foreign key violation: table %s row %v parent %s constraint %d", table, rowID, parent, foreignKeyID)
	}
	if err := violations.Err(); err != nil {
		_ = violations.Close()
		return fmt.Errorf("iterate migrated foreign key violations: %w", err)
	}
	if err := violations.Close(); err != nil {
		return fmt.Errorf("close migrated foreign key check: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("restore foreign keys after migrations: %w", err)
	}
	foreignKeysDisabled = false

	return nil
}

func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("query applied migrations: %w", err)
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}

	return versions, nil
}

func migrationApplied(ctx context.Context, tx *sql.Tx, version string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&count); err != nil {
		return false, fmt.Errorf("check migration %s: %w", version, err)
	}

	return count > 0, nil
}
