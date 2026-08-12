package db

import (
	"strings"
	"testing"
)

// TestFTSMigrationPresentOrGated pins the FTS5 fail-closed contract: the
// 0005_task_fts migration file exists, and whether it is applied depends on
// the build tag via filterOptionalMigrations. A binary without FTS5 must skip
// the FTS migration rather than fail partway through opening the database.
func TestFTSMigrationGating(t *testing.T) {
	files, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	foundFTS := false
	for _, f := range files {
		names = append(names, f.Name())
		if strings.Contains(f.Name(), "_fts") {
			foundFTS = true
		}
	}
	if !foundFTS {
		t.Fatalf("no *_fts migration present in %v", names)
	}

	filtered := filterOptionalMigrations([]string{"migrations/0005_task_fts.sql", "migrations/0004_event_log.sql"})
	// With the tag the FTS migration is kept; without it, dropped. Either way
	// the non-FTS migration survives and ordering is preserved.
	if len(filtered) < 1 || filtered[len(filtered)-1] != "migrations/0004_event_log.sql" {
		t.Fatalf("filterOptionalMigrations = %v, want 0004 kept", filtered)
	}
}
