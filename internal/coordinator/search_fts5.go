//go:build sqlite_fts5

package coordinator

// fts5Available reports whether the binary was built with the sqlite_fts5 tag
// (FTS5 compiled into mattn/go-sqlite3 and the task_fts migration applied).
// SearchTasks uses it to choose ranked FTS matching over the LIKE fallback.
const fts5Available = true
