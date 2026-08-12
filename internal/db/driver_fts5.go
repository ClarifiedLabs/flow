//go:build sqlite_fts5

package db

import (
	"database/sql"

	"github.com/mattn/go-sqlite3"
)

// projectDriverName is registered below with the sqlite_fts5 build tag, which
// compiles mattn/go-sqlite3 with SQLITE_ENABLE_FTS5. The hook is a no-op (the
// tag does the work) but registering a distinct name keeps FTS binaries
// distinguishable in logs and lets OpenWithDriver tests wrap it cleanly.
const projectDriverName = "flow-sqlite3-fts5"

func init() {
	sql.Register(projectDriverName, &sqlite3.SQLiteDriver{})
}

// filterOptionalMigrations keeps every migration: FTS5 is compiled in.
func filterOptionalMigrations(files []string) []string {
	return files
}
