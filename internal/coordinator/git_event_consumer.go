package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GitEventConsumer keeps change projections in sync with out-of-band pushes.
// Recorded git_events were previously write-only: a push that bypassed the
// session-ready flow left change.head_sha stale until a manual reconcile, and
// the merge head guard then rejected the change. The consumer runs from the
// background ticker: when new events appear it reconciles the exchange (which
// syncs every change head to the real branch tip rather than trusting the
// event payload) and resets automated checks for issues whose head moved. It
// deliberately does NOT schedule a review round — that still requires an
// explicit session-ready signal.
// gitEventWatermarkName is the consumer_watermarks key under which the
// git-event consumer's high-water mark is persisted.
const gitEventWatermarkName = "git_events"

type GitEventConsumer struct {
	db         *sql.DB
	project    Project
	reconciler *ReconcileService
	checks     *CheckService

	// lastSeenID caches the high-water mark of consumed git_events. It is
	// persisted to consumer_watermarks (name = git_events) on every clean pass
	// and loaded lazily on the first ConsumeNew, so a restart does not re-run an
	// already-consumed reconcile pass. loaded guards the one-time load.
	lastSeenID int64
	loaded     bool
}

func NewGitEventConsumer(database *sql.DB, project Project) *GitEventConsumer {
	return &GitEventConsumer{
		db:         database,
		project:    project,
		reconciler: NewReconcileService(database),
		checks:     NewCheckService(database),
	}
}

// ConsumeNew runs one reconcile pass when git events arrived since the last
// pass, and reports whether it ran. The watermark only advances on a clean
// pass so partial failures are retried on the next tick.
func (c *GitEventConsumer) ConsumeNew(ctx context.Context) (bool, error) {
	if !c.loaded {
		if err := c.loadWatermark(ctx); err != nil {
			return false, err
		}
		c.loaded = true
	}

	var maxID sql.NullInt64
	if err := c.db.QueryRowContext(ctx, `SELECT MAX(id) FROM git_events`).Scan(&maxID); err != nil {
		return false, fmt.Errorf("read git event high-water mark: %w", err)
	}
	if !maxID.Valid || maxID.Int64 <= c.lastSeenID {
		return false, nil
	}

	result, err := c.reconciler.Reconcile(ctx, c.project)
	var errs error
	if err != nil {
		errs = errors.Join(errs, err)
	}
	resetIssues := map[string]bool{}
	for _, updated := range result.UpdatedChanges {
		if resetIssues[updated.IssueID] {
			continue
		}
		resetIssues[updated.IssueID] = true
		if _, err := c.checks.ResetAutomatedChecksForNewRevision(ctx, updated.IssueID); err != nil {
			errs = errors.Join(errs, fmt.Errorf("reset automated checks for %s: %w", updated.IssueID, err))
		}
	}
	if errs == nil {
		if err := c.saveWatermark(ctx, maxID.Int64); err != nil {
			return true, err
		}
		c.lastSeenID = maxID.Int64
	}
	return true, errs
}

// loadWatermark reads the persisted high-water mark into the in-memory cache.
// A missing row (first run on a fresh db) leaves lastSeenID at zero.
func (c *GitEventConsumer) loadWatermark(ctx context.Context) error {
	var lastSeen int64
	err := c.db.QueryRowContext(ctx,
		`SELECT last_seen_id FROM consumer_watermarks WHERE name = ?`,
		gitEventWatermarkName).Scan(&lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load git event watermark: %w", err)
	}
	c.lastSeenID = lastSeen
	return nil
}

// saveWatermark persists the high-water mark after a clean pass so a restart
// does not re-run an already-consumed reconcile.
func (c *GitEventConsumer) saveWatermark(ctx context.Context, lastSeen int64) error {
	if _, err := c.db.ExecContext(ctx, `
INSERT INTO consumer_watermarks (name, last_seen_id, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
	last_seen_id = excluded.last_seen_id,
	updated_at = excluded.updated_at`,
		gitEventWatermarkName, lastSeen, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("persist git event watermark: %w", err)
	}
	return nil
}
