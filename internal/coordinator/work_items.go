package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type WorkItemKind string

const (
	WorkItemTask    WorkItemKind = "task"
	WorkItemEpic    WorkItemKind = "epic"
	WorkItemFeature WorkItemKind = "feature"
)

var (
	ErrWorkItemNotFound       = errors.New("work item not found")
	ErrWorkItemNotSchedulable = errors.New("work item is not schedulable")
	ErrWorkItemMoveConflict   = errors.New("work item cannot be moved")
	ErrWorkItemBlocked        = errors.New("work item has unresolved blockers")
)

type WorkItemRef struct {
	ID    string       `json:"id"`
	Kind  WorkItemKind `json:"kind"`
	Title string       `json:"title"`
}

type WorkItemState struct {
	Status     string `json:"status"`
	Terminal   bool   `json:"terminal"`
	Successful bool   `json:"successful"`
}

type WorkItemCapabilities struct {
	Schedulable bool `json:"schedulable"`
	CanContain  bool `json:"can_contain"`
	CanStart    bool `json:"can_start"`
	CanComplete bool `json:"can_complete"`
	CanReopen   bool `json:"can_reopen"`
	CanArchive  bool `json:"can_archive"`
	OwnsBranch  bool `json:"owns_branch"`
	CanRebase   bool `json:"can_rebase"`
	CanLand     bool `json:"can_land"`
}

type WorkItemSummary struct {
	WorkItemRef
	Body               string               `json:"body"`
	Priority           int                  `json:"priority"`
	ParentItemID       string               `json:"parent_item_id,omitempty"`
	EffectiveFeatureID string               `json:"effective_feature_id,omitempty"`
	UnresolvedBlockers int                  `json:"unresolved_blockers"`
	State              WorkItemState        `json:"state"`
	Capabilities       WorkItemCapabilities `json:"capabilities"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

type WorkItemConsistencyIssue struct {
	Code    string `json:"code"`
	ItemID  string `json:"item_id,omitempty"`
	Message string `json:"message"`
}

type WorkItemConsistencyReport struct {
	Healthy bool                       `json:"healthy"`
	Issues  []WorkItemConsistencyIssue `json:"issues"`
}

type WorkItemService struct {
	db        *sql.DB
	projectID string
	now       func() time.Time
}

func NewWorkItemService(database *sql.DB, projectID string) *WorkItemService {
	return &WorkItemService{db: database, projectID: strings.TrimSpace(projectID), now: sqlitex.UTCNow}
}

func capabilitiesForKind(kind WorkItemKind) WorkItemCapabilities {
	switch kind {
	case WorkItemTask:
		return WorkItemCapabilities{Schedulable: true}
	case WorkItemEpic:
		return WorkItemCapabilities{CanContain: true, CanStart: true, CanComplete: true, CanReopen: true, CanArchive: true}
	case WorkItemFeature:
		return WorkItemCapabilities{CanContain: true, CanStart: true, CanArchive: true, OwnsBranch: true, CanRebase: true, CanLand: true}
	default:
		return WorkItemCapabilities{}
	}
}

type workItemExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertWorkItem(ctx context.Context, tx workItemExecer, id string, kind WorkItemKind, createdAt string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_items (id, kind, created_at) VALUES (?, ?, ?)`, id, string(kind), createdAt); err != nil {
		return fmt.Errorf("insert %s work item: %w", kind, err)
	}
	return nil
}

func workItemKindTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (WorkItemKind, error) {
	var kind string
	if err := q.QueryRowContext(ctx, `SELECT kind FROM work_items WHERE id = ?`, strings.TrimSpace(id)).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrWorkItemNotFound
		}
		return "", err
	}
	return WorkItemKind(kind), nil
}

func (s *WorkItemService) Kind(ctx context.Context, id string) (WorkItemKind, error) {
	return workItemKindTx(ctx, s.db, id)
}

func (s *WorkItemService) Get(ctx context.Context, id string) (WorkItemSummary, error) {
	items, err := s.GetMany(ctx, []string{id})
	if err != nil {
		return WorkItemSummary{}, err
	}
	if len(items) == 0 {
		return WorkItemSummary{}, ErrWorkItemNotFound
	}
	return items[0], nil
}

func (s *WorkItemService) List(ctx context.Context) ([]WorkItemSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM work_items ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.GetMany(ctx, ids)
}

// GetMany resolves subtype-owned metadata and normalized state in one query.
// Results follow the caller's first-seen ID order and omit unknown IDs.
func (s *WorkItemService) GetMany(ctx context.Context, itemIDs []string) ([]WorkItemSummary, error) {
	ids := uniqueTrimmed(itemIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i := range ids {
		args[i] = ids[i]
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	wi.id,
	wi.kind,
	COALESCE(t.title, e.title, f.title),
	COALESCE(t.body, e.body, f.body, ''),
	COALESCE(t.priority, e.priority, 0),
	COALESCE((
		SELECT r.source_item_id FROM work_item_relations r
		WHERE r.target_item_id = wi.id AND r.kind = 'parent_of'
	), ''),
	CASE wi.kind
		WHEN 'task' THEN COALESCE(t.lifecycle_state, 'unscheduled')
		WHEN 'epic' THEN e.status
		WHEN 'feature' THEN f.status
	END,
	CASE
		WHEN wi.kind = 'task' AND t.lifecycle_state = 'done' THEN 1
		WHEN wi.kind = 'epic' AND e.status IN ('completed', 'archived') THEN 1
		WHEN wi.kind = 'feature' AND f.status IN ('landed', 'archived') THEN 1
		ELSE 0
	END,
	CASE
		WHEN wi.kind = 'task' AND t.lifecycle_state = 'done' AND t.done_resolution IN ('completed', 'merged') THEN 1
		WHEN wi.kind = 'epic' AND e.status = 'completed' THEN 1
		WHEN wi.kind = 'feature' AND f.status = 'landed' THEN 1
		ELSE 0
	END,
	COALESCE(t.created_at, e.created_at, f.created_at),
	COALESCE(t.updated_at, e.updated_at, f.updated_at),
	COALESCE((
		WITH RECURSIVE feature_ancestors(id, depth) AS (
			SELECT wi.id, 0
			UNION ALL
			SELECT r.source_item_id, feature_ancestors.depth + 1
			FROM feature_ancestors
			JOIN work_item_relations r ON r.target_item_id = feature_ancestors.id AND r.kind = 'parent_of'
		)
		SELECT feature_ancestors.id
		FROM feature_ancestors
		JOIN work_items fwi ON fwi.id = feature_ancestors.id AND fwi.kind = 'feature'
		ORDER BY feature_ancestors.depth
		LIMIT 1
	), ''),
	COALESCE((
		WITH RECURSIVE ancestors(id) AS (
			VALUES (wi.id)
			UNION ALL
			SELECT r.source_item_id FROM ancestors
			JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
		), blockers(id) AS (
			SELECT DISTINCT r.source_item_id FROM ancestors
			JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'blocks'
		)
		SELECT COUNT(*) FROM blockers b
		JOIN work_items bwi ON bwi.id = b.id
		LEFT JOIN tasks bt ON bt.id = b.id AND bwi.kind = 'task'
		LEFT JOIN epics be ON be.id = b.id AND bwi.kind = 'epic'
		LEFT JOIN features bf ON bf.id = b.id AND bwi.kind = 'feature'
		WHERE CASE bwi.kind
			WHEN 'task' THEN COALESCE(bt.lifecycle_state = 'done', 0)
			WHEN 'epic' THEN COALESCE(be.status IN ('completed', 'archived'), 0)
			WHEN 'feature' THEN COALESCE(bf.status IN ('landed', 'archived'), 0)
		END = 0
	), 0)
FROM work_items wi
LEFT JOIN tasks t ON t.id = wi.id AND wi.kind = 'task'
LEFT JOIN epics e ON e.id = wi.id AND wi.kind = 'epic'
LEFT JOIN features f ON f.id = wi.id AND wi.kind = 'feature'
WHERE `+inPredicate("wi.id", len(ids)), args...)
	if err != nil {
		return nil, fmt.Errorf("list work items: %w", err)
	}
	defer rows.Close()
	byID := make(map[string]WorkItemSummary, len(ids))
	for rows.Next() {
		var item WorkItemSummary
		var kind, status, createdAt, updatedAt, explicitFeatureID string
		var terminal, successful int
		if err := rows.Scan(
			&item.ID, &kind, &item.Title, &item.Body, &item.Priority,
			&item.ParentItemID, &status, &terminal, &successful,
			&createdAt, &updatedAt, &explicitFeatureID, &item.UnresolvedBlockers,
		); err != nil {
			return nil, fmt.Errorf("scan work item: %w", err)
		}
		item.Kind = WorkItemKind(kind)
		item.State = WorkItemState{Status: status, Terminal: terminal != 0, Successful: successful != 0}
		item.Capabilities = capabilitiesForKind(item.Kind)
		item.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		item.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return nil, err
		}
		item.EffectiveFeatureID = explicitFeatureID
		byID[item.ID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]WorkItemSummary, 0, len(byID))
	for _, id := range ids {
		item, ok := byID[id]
		if !ok {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *WorkItemService) ParentID(ctx context.Context, id string) (string, error) {
	var parent string
	err := s.db.QueryRowContext(ctx, `
SELECT source_item_id FROM work_item_relations
WHERE target_item_id = ? AND kind = 'parent_of'`, strings.TrimSpace(id)).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return parent, err
}

func (s *WorkItemService) EffectiveFeatureID(ctx context.Context, id string) (string, error) {
	var featureID sql.NullString
	err := s.db.QueryRowContext(ctx, `
WITH RECURSIVE ancestors(id, depth) AS (
	SELECT ?, 0
	UNION ALL
	SELECT r.source_item_id, ancestors.depth + 1
	FROM ancestors
	JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
)
SELECT a.id
FROM ancestors a
JOIN work_items wi ON wi.id = a.id AND wi.kind = 'feature'
ORDER BY a.depth
LIMIT 1`, strings.TrimSpace(id)).Scan(&featureID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return featureID.String, nil
}

// CheckConsistency validates invariants that span subtype tables, the
// hierarchy, dependency edges, and derived feature caches.
func (s *WorkItemService) CheckConsistency(ctx context.Context) (WorkItemConsistencyReport, error) {
	report := WorkItemConsistencyReport{Healthy: true, Issues: []WorkItemConsistencyIssue{}}
	checks := []struct {
		code  string
		query string
		args  []any
		msg   string
	}{
		{
			code: "subtype_mismatch",
			query: `
SELECT id FROM (
	SELECT wi.id FROM work_items wi
	LEFT JOIN tasks t ON t.id = wi.id
	LEFT JOIN epics e ON e.id = wi.id
	LEFT JOIN features f ON f.id = wi.id
	WHERE (wi.kind = 'task' AND (t.id IS NULL OR e.id IS NOT NULL OR f.id IS NOT NULL))
		OR (wi.kind = 'epic' AND (e.id IS NULL OR t.id IS NOT NULL OR f.id IS NOT NULL))
		OR (wi.kind = 'feature' AND (f.id IS NULL OR t.id IS NOT NULL OR e.id IS NOT NULL))
	UNION SELECT t.id FROM tasks t LEFT JOIN work_items wi ON wi.id = t.id WHERE COALESCE(wi.kind, '') != 'task'
	UNION SELECT e.id FROM epics e LEFT JOIN work_items wi ON wi.id = e.id WHERE COALESCE(wi.kind, '') != 'epic'
	UNION SELECT f.id FROM features f LEFT JOIN work_items wi ON wi.id = f.id WHERE COALESCE(wi.kind, '') != 'feature'
) ORDER BY id`,
			msg: "registry kind and subtype row do not match",
		},
		{
			code: "invalid_parent",
			query: `
SELECT r.source_item_id || ' -> ' || r.target_item_id
FROM work_item_relations r JOIN work_items wi ON wi.id = r.source_item_id
WHERE r.kind = 'parent_of' AND wi.kind NOT IN ('epic', 'feature')
ORDER BY r.source_item_id, r.target_item_id`,
			msg: "parent_of source is not a container",
		},
		{
			code: "task_feature_cache_mismatch",
			query: `
SELECT t.id FROM tasks t
WHERE COALESCE(t.feature_id, '') != COALESCE((
	WITH RECURSIVE ancestors(id, depth) AS (
		SELECT r.source_item_id, 1 FROM work_item_relations r
		WHERE r.target_item_id = t.id AND r.kind = 'parent_of'
		UNION ALL
		SELECT r.source_item_id, ancestors.depth + 1 FROM ancestors
		JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
	)
	SELECT ancestors.id FROM ancestors JOIN work_items wi ON wi.id = ancestors.id AND wi.kind = 'feature'
	ORDER BY ancestors.depth LIMIT 1
), '') ORDER BY t.id`,
			msg: "task feature cache does not match its nearest feature ancestor",
		},
		{
			code: "feature_target_mismatch",
			query: `
SELECT f.id FROM features f
WHERE COALESCE(f.integration_feature_id, '') != COALESCE((
	WITH RECURSIVE ancestors(id, depth) AS (
		SELECT r.source_item_id, 1 FROM work_item_relations r
		WHERE r.target_item_id = f.id AND r.kind = 'parent_of'
		UNION ALL
		SELECT r.source_item_id, ancestors.depth + 1 FROM ancestors
		JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
	)
	SELECT ancestors.id FROM ancestors JOIN work_items wi ON wi.id = ancestors.id AND wi.kind = 'feature'
	ORDER BY ancestors.depth LIMIT 1
), '') ORDER BY f.id`,
			msg: "feature integration target does not match its immutable hierarchy scope",
		},
		{
			code: "dependency_cycle",
			query: `
WITH RECURSIVE edges(source, target) AS (
	SELECT source_item_id, target_item_id FROM work_item_relations WHERE kind = 'blocks'
	UNION
	SELECT target_item_id, source_item_id FROM work_item_relations WHERE kind = 'parent_of'
), paths(start, id) AS (
	SELECT source, target FROM edges
	UNION
	SELECT paths.start, edges.target FROM paths JOIN edges ON edges.source = paths.id
)
SELECT DISTINCT start FROM paths WHERE start = id ORDER BY start`,
			msg: "combined containment and blocker graph contains a cycle",
		},
	}
	for _, check := range checks {
		rows, err := s.db.QueryContext(ctx, check.query, check.args...)
		if err != nil {
			return WorkItemConsistencyReport{}, fmt.Errorf("check %s: %w", check.code, err)
		}
		for rows.Next() {
			var itemID string
			if err := rows.Scan(&itemID); err != nil {
				rows.Close()
				return WorkItemConsistencyReport{}, err
			}
			report.Issues = append(report.Issues, WorkItemConsistencyIssue{Code: check.code, ItemID: itemID, Message: check.msg})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return WorkItemConsistencyReport{}, err
		}
		if err := rows.Close(); err != nil {
			return WorkItemConsistencyReport{}, err
		}
	}
	report.Healthy = len(report.Issues) == 0
	return report, nil
}
