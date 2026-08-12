//go:build !sqlite_fts5

package coordinator

// fts5Available is false without the sqlite_fts5 build tag; SearchTasks falls
// back to case-insensitive substring matching.
const fts5Available = false
