//go:build !sqlite_fts5

package db

import "strings"

// projectDriverName is the stock mattn/go-sqlite3 driver. Without the
// sqlite_fts5 build tag there is no FTS5 support; the optional FTS migration
// is skipped (filterOptionalMigrations) so these databases keep working and
// TaskService.SearchTasks falls back to substring matching.
const projectDriverName = "sqlite3"

// filterOptionalMigrations drops *_fts migrations: FTS5 is not compiled in.
func filterOptionalMigrations(files []string) []string {
	kept := files[:0]
	for _, f := range files {
		if strings.Contains(f, "_fts") {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}
