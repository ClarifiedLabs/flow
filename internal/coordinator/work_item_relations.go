package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrWorkItemRelationExists = errors.New("work item relation already exists")
	ErrWorkItemHasParent      = errors.New("work item already has a parent")
	ErrWorkItemCycle          = errors.New("work item relation would create a dependency cycle")
	ErrWorkItemParentClosed   = errors.New("work item parent is closed")
	ErrActiveRebaseBlocker    = errors.New("active feature rebase blocker cannot be removed")
)

// WorkItemRelation is the typed, cross-kind relation returned by the canonical
// work-item API. RelatedTo is symmetric and is stored in canonical ID order.
type WorkItemRelation struct {
	Source    WorkItemSummary `json:"source"`
	Target    WorkItemSummary `json:"target"`
	Kind      RelationKind    `json:"kind"`
	Resolved  bool            `json:"resolved"`
	CreatedBy Actor           `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
}

type WorkItemBlocker struct {
	Item     WorkItemSummary `json:"item"`
	Direct   bool            `json:"direct"`
	ViaItem  *WorkItemRef    `json:"via_item,omitempty"`
	Resolved bool            `json:"resolved"`
}

// CreateWorkItemRelationInput describes a relation involving an item whose ID
// is allocated by the surrounding create transaction. Exactly one endpoint is
// marked as new; that endpoint's ID must be blank and the other must name an
// existing work item in this project.
type CreateWorkItemRelationInput struct {
	SourceItemID    string
	TargetItemID    string
	SourceIsNewItem bool
	TargetIsNewItem bool
	Kind            RelationKind
	CreatedBy       Actor
}

type preparedCreateWorkItemRelations struct {
	Relations          []CreateWorkItemRelationInput
	ParentItemID       string
	parentCreatedBy    Actor
	parentFromRelation bool
}

type workItemRelationQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Link creates a canonical cross-kind relation. Containment and blocker edges
// share one dependency graph: blocks A->B contributes A->B, while parent P->C
// contributes C->P because a container depends on its child completing.
func (s *WorkItemService) Link(ctx context.Context, sourceID, targetID string, kind RelationKind, actor Actor) error {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return fmt.Errorf("begin link work item transaction: %w", err)
	}
	defer tx.Rollback()

	if err := s.linkTx(ctx, tx, sourceID, targetID, kind, actor); err != nil {
		return err
	}
	if kind == RelationParentOf {
		if err := s.syncSubtreeFeatureCachesTx(ctx, tx, targetID, s.now().UTC()); err != nil {
			return err
		}
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{sourceID, targetID}, s.now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit link work items: %w", err)
	}
	return nil
}

// normalizeCreateRelations validates and canonicalizes the request-only part of
// a create relation set without consulting mutable work-item rows. The JSON is
// stable across relation ordering and harmless whitespace differences, making
// it suitable as durable feature-creation intent identity.
func normalizeCreateRelations(declaredParentID string, inputs []CreateWorkItemRelationInput, fallbackActor Actor) (preparedCreateWorkItemRelations, string, error) {
	plan := preparedCreateWorkItemRelations{
		ParentItemID: strings.TrimSpace(declaredParentID),
		Relations:    make([]CreateWorkItemRelationInput, 0, len(inputs)),
	}
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		input.SourceItemID = strings.TrimSpace(input.SourceItemID)
		input.TargetItemID = strings.TrimSpace(input.TargetItemID)
		if input.SourceIsNewItem == input.TargetIsNewItem {
			return preparedCreateWorkItemRelations{}, "", errors.New("work item create relation must mark exactly one endpoint as the new item")
		}
		if input.SourceIsNewItem {
			if input.SourceItemID != "" {
				return preparedCreateWorkItemRelations{}, "", errors.New("work item create relation source_item_id must be blank when source_is_new_item is true")
			}
			if input.TargetItemID == "" {
				return preparedCreateWorkItemRelations{}, "", errors.New("work item create relation target_item_id is required")
			}
		} else {
			if input.TargetItemID != "" {
				return preparedCreateWorkItemRelations{}, "", errors.New("work item create relation target_item_id must be blank when target_is_new_item is true")
			}
			if input.SourceItemID == "" {
				return preparedCreateWorkItemRelations{}, "", errors.New("work item create relation source_item_id is required")
			}
		}
		if err := validateRelationKind(input.Kind); err != nil {
			return preparedCreateWorkItemRelations{}, "", err
		}
		input.CreatedBy = defaultActor(input.CreatedBy, fallbackActor)
		if err := validateActor(input.CreatedBy); err != nil {
			return preparedCreateWorkItemRelations{}, "", err
		}
		if input.Kind == RelationParentOf && input.TargetIsNewItem {
			if plan.ParentItemID != "" {
				if plan.parentFromRelation {
					return preparedCreateWorkItemRelations{}, "", errors.New("work item create request declares more than one parent")
				}
				return preparedCreateWorkItemRelations{}, "", errors.New("work item create request declares a parent in both parent_item_id and work_item_relations")
			}
			plan.ParentItemID = input.SourceItemID
			plan.parentCreatedBy = input.CreatedBy
			plan.parentFromRelation = true
		}

		keySource, keyTarget := input.SourceItemID, input.TargetItemID
		if input.SourceIsNewItem {
			keySource = "<new>"
		} else {
			keyTarget = "<new>"
		}
		if input.Kind == RelationRelatedTo && keyTarget < keySource {
			keySource, keyTarget = keyTarget, keySource
		}
		key := string(input.Kind) + "\x00" + keySource + "\x00" + keyTarget
		if _, ok := seen[key]; ok {
			return preparedCreateWorkItemRelations{}, "", ErrWorkItemRelationExists
		}
		seen[key] = struct{}{}
		plan.Relations = append(plan.Relations, input)
	}

	payloadRelations := append([]CreateWorkItemRelationInput(nil), plan.Relations...)
	for i := range payloadRelations {
		if payloadRelations[i].Kind == RelationRelatedTo && payloadRelations[i].TargetIsNewItem {
			payloadRelations[i].SourceItemID, payloadRelations[i].TargetItemID = "", payloadRelations[i].SourceItemID
			payloadRelations[i].SourceIsNewItem, payloadRelations[i].TargetIsNewItem = true, false
		}
	}
	sort.Slice(payloadRelations, func(i, j int) bool {
		left, _ := json.Marshal(payloadRelations[i])
		right, _ := json.Marshal(payloadRelations[j])
		return string(left) < string(right)
	})
	payload, err := json.Marshal(payloadRelations)
	if err != nil {
		return preparedCreateWorkItemRelations{}, "", fmt.Errorf("encode create relation payload: %w", err)
	}
	return plan, string(payload), nil
}

func decodeCreateRelationPayload(payload string) ([]CreateWorkItemRelationInput, error) {
	var relations []CreateWorkItemRelationInput
	if err := json.Unmarshal([]byte(payload), &relations); err != nil {
		return nil, fmt.Errorf("decode create relation payload: %w", err)
	}
	if relations == nil {
		relations = []CreateWorkItemRelationInput{}
	}
	return relations, nil
}

// prepareCreateRelations validates the complete generic relation set before a
// subtype starts inserting rows. linkTx remains authoritative inside the
// eventual write transaction; this preflight additionally prevents feature Git
// side effects for requests that are already invalid.
func (s *WorkItemService) prepareCreateRelations(ctx context.Context, q workItemRelationQuerier, newKind WorkItemKind, declaredParentID string, inputs []CreateWorkItemRelationInput, fallbackActor Actor) (preparedCreateWorkItemRelations, error) {
	plan, _, err := normalizeCreateRelations(declaredParentID, inputs, fallbackActor)
	if err != nil {
		return preparedCreateWorkItemRelations{}, err
	}
	var incoming, outgoing []string

	for _, input := range plan.Relations {
		existingID := input.SourceItemID
		sourceKind := newKind
		if input.SourceIsNewItem {
			existingID = input.TargetItemID
		} else {
			var err error
			sourceKind, err = workItemKindTx(ctx, q, input.SourceItemID)
			if err != nil {
				return preparedCreateWorkItemRelations{}, fmt.Errorf("source: %w", err)
			}
		}
		targetKind := newKind
		if input.TargetIsNewItem {
			// sourceKind was loaded above.
		} else {
			var err error
			targetKind, err = workItemKindTx(ctx, q, input.TargetItemID)
			if err != nil {
				return preparedCreateWorkItemRelations{}, fmt.Errorf("target: %w", err)
			}
		}
		existingKind := sourceKind
		if input.SourceIsNewItem {
			existingKind = targetKind
		}
		if err := requireWorkItemRelationMutableTx(ctx, q, existingID, existingKind); err != nil {
			return preparedCreateWorkItemRelations{}, err
		}

		if input.Kind == RelationParentOf {
			if sourceKind != WorkItemEpic && sourceKind != WorkItemFeature {
				return preparedCreateWorkItemRelations{}, fmt.Errorf("%w: %s items cannot contain children", ErrWorkItemMoveConflict, sourceKind)
			}
			if !input.SourceIsNewItem {
				open, err := createRelationParentOpenTx(ctx, q, input.SourceItemID, sourceKind)
				if err != nil {
					return preparedCreateWorkItemRelations{}, err
				}
				if !open {
					return preparedCreateWorkItemRelations{}, ErrWorkItemParentClosed
				}
			}
			if !input.TargetIsNewItem {
				var count int
				if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_item_relations WHERE target_item_id = ? AND kind = 'parent_of'`, input.TargetItemID).Scan(&count); err != nil {
					return preparedCreateWorkItemRelations{}, err
				}
				if count != 0 {
					return preparedCreateWorkItemRelations{}, ErrWorkItemHasParent
				}
			}
		}

		if input.Kind == RelationBlocks || input.Kind == RelationParentOf {
			newToExisting := input.SourceIsNewItem
			if input.Kind == RelationParentOf {
				newToExisting = input.TargetIsNewItem
			}
			if newToExisting {
				outgoing = append(outgoing, existingID)
			} else {
				incoming = append(incoming, existingID)
			}
		}
	}

	if plan.ParentItemID != "" && !plan.parentFromRelation {
		outgoing = append(outgoing, plan.ParentItemID)
	}
	for _, fromNew := range outgoing {
		for _, toNew := range incoming {
			cycle := fromNew == toNew
			if !cycle {
				var err error
				cycle, err = workItemDependencyPathExists(ctx, q, fromNew, toNew)
				if err != nil {
					return preparedCreateWorkItemRelations{}, err
				}
			}
			if cycle {
				return preparedCreateWorkItemRelations{}, ErrWorkItemCycle
			}
		}
	}
	return plan, nil
}

func createRelationParentOpenTx(ctx context.Context, q workItemRelationQuerier, id string, kind WorkItemKind) (bool, error) {
	if kind != WorkItemEpic {
		return workItemContainerOpenTx(ctx, q, id, kind)
	}
	var status string
	var automatic int
	if err := q.QueryRowContext(ctx, `SELECT status, completed_automatically FROM epics WHERE id = ?`, id).Scan(&status, &automatic); err != nil {
		return false, err
	}
	return status == string(EpicOpen) || (status == string(EpicCompleted) && automatic != 0), nil
}

func resolveCreateRelation(newID string, input CreateWorkItemRelationInput) (string, string) {
	if input.SourceIsNewItem {
		return newID, input.TargetItemID
	}
	return input.SourceItemID, newID
}

// linkCreateRelationsTx inserts the already-prepared generic relations. A
// parent_of relation targeting the new item is skipped because each create path
// inserts the effective ParentItemID first, preserving legacy parent/cache
// behavior while storing that generic declaration exactly once.
func (s *WorkItemService) linkCreateRelationsTx(ctx context.Context, tx workItemRelationQuerier, newID string, plan preparedCreateWorkItemRelations, actor Actor) ([]string, error) {
	touched := []string{newID}
	parentTargets := make(map[string]struct{})
	if plan.ParentItemID != "" {
		parentActor := actor
		if plan.parentFromRelation {
			parentActor = plan.parentCreatedBy
		}
		if err := s.linkTx(ctx, tx, plan.ParentItemID, newID, RelationParentOf, parentActor); err != nil {
			return nil, err
		}
		parentTargets[newID] = struct{}{}
		touched = append(touched, plan.ParentItemID)
	}
	for _, input := range plan.Relations {
		if input.Kind == RelationParentOf && input.TargetIsNewItem {
			continue
		}
		sourceID, targetID := resolveCreateRelation(newID, input)
		if err := s.linkTx(ctx, tx, sourceID, targetID, input.Kind, input.CreatedBy); err != nil {
			return nil, err
		}
		if input.Kind == RelationParentOf {
			parentTargets[targetID] = struct{}{}
		}
		touched = append(touched, sourceID, targetID)
	}
	for targetID := range parentTargets {
		if err := s.syncSubtreeFeatureCachesTx(ctx, tx, targetID, s.now().UTC()); err != nil {
			return nil, err
		}
	}
	return touched, nil
}

func (s *WorkItemService) linkTx(ctx context.Context, tx workItemRelationQuerier, sourceID, targetID string, kind RelationKind, actor Actor) error {
	sourceID = strings.TrimSpace(sourceID)
	targetID = strings.TrimSpace(targetID)
	if sourceID == "" || targetID == "" {
		return errors.New("work item relation source and target are required")
	}
	if sourceID == targetID {
		return errors.New("work item relation cannot target itself")
	}
	if err := validateRelationKind(kind); err != nil {
		return err
	}
	actor = defaultActor(actor, ActorHuman)
	if err := validateActor(actor); err != nil {
		return err
	}

	sourceKind, err := workItemKindTx(ctx, tx, sourceID)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	targetKind, err := workItemKindTx(ctx, tx, targetID)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if err := requireWorkItemRelationMutableTx(ctx, tx, sourceID, sourceKind); err != nil {
		return err
	}
	if err := requireWorkItemRelationMutableTx(ctx, tx, targetID, targetKind); err != nil {
		return err
	}

	if kind == RelationParentOf {
		if sourceKind != WorkItemEpic && sourceKind != WorkItemFeature {
			return fmt.Errorf("%w: %s items cannot contain children", ErrWorkItemMoveConflict, sourceKind)
		}
		open, err := prepareWorkItemParentTx(ctx, tx, sourceID, sourceKind, s.now().UTC())
		if err != nil {
			return err
		}
		if !open {
			return ErrWorkItemParentClosed
		}
		var count int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM work_item_relations WHERE target_item_id = ? AND kind = 'parent_of'`, targetID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrWorkItemHasParent
		}
	}

	if kind == RelationBlocks || kind == RelationParentOf {
		fromID, toID := sourceID, targetID
		if kind == RelationParentOf {
			fromID, toID = targetID, sourceID
		}
		cycle, err := workItemDependencyPathExists(ctx, tx, toID, fromID)
		if err != nil {
			return err
		}
		if cycle {
			return ErrWorkItemCycle
		}
	}

	if kind == RelationRelatedTo && targetID < sourceID {
		sourceID, targetID = targetID, sourceID
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO work_item_relations (source_item_id, target_item_id, kind, created_by, created_at)
VALUES (?, ?, ?, ?, ?)`, sourceID, targetID, string(kind), string(actor), formatTime(s.now().UTC())); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrWorkItemRelationExists
		}
		return fmt.Errorf("link work items: %w", err)
	}
	return nil
}

func requireWorkItemRelationMutableTx(ctx context.Context, q workItemRelationQuerier, id string, kind WorkItemKind) error {
	var archived int
	switch kind {
	case WorkItemEpic:
		if err := q.QueryRowContext(ctx, `SELECT status = 'archived' FROM epics WHERE id = ?`, id).Scan(&archived); err != nil {
			return err
		}
	case WorkItemFeature:
		if err := q.QueryRowContext(ctx, `SELECT status = 'archived' FROM features WHERE id = ?`, id).Scan(&archived); err != nil {
			return err
		}
	}
	if archived != 0 {
		return ErrWorkItemParentClosed
	}
	return nil
}

func prepareWorkItemParentTx(ctx context.Context, q workItemRelationQuerier, id string, kind WorkItemKind, now time.Time) (bool, error) {
	if kind != WorkItemEpic {
		return workItemContainerOpenTx(ctx, q, id, kind)
	}
	var status string
	var automatic int
	if err := q.QueryRowContext(ctx, `SELECT status, completed_automatically FROM epics WHERE id = ?`, id).Scan(&status, &automatic); err != nil {
		return false, err
	}
	if status == string(EpicCompleted) && automatic != 0 {
		if _, err := q.ExecContext(ctx, `
UPDATE epics SET status = 'open', completed_automatically = 0,
	completed_at = NULL, updated_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
			return false, err
		}
		return true, nil
	}
	return status == string(EpicOpen), nil
}

func workItemContainerOpenTx(ctx context.Context, q workItemRelationQuerier, id string, kind WorkItemKind) (bool, error) {
	var status string
	var err error
	switch kind {
	case WorkItemEpic:
		err = q.QueryRowContext(ctx, `SELECT status FROM epics WHERE id = ?`, id).Scan(&status)
	case WorkItemFeature:
		err = q.QueryRowContext(ctx, `SELECT status FROM features WHERE id = ?`, id).Scan(&status)
	default:
		return false, nil
	}
	return status == "open", err
}

func workItemDependencyPathExists(ctx context.Context, q workItemRelationQuerier, startID, goalID string) (bool, error) {
	var found int
	err := q.QueryRowContext(ctx, `
WITH RECURSIVE reachable(id) AS (
	VALUES (?)
	UNION
	SELECT CASE r.kind
		WHEN 'blocks' THEN r.target_item_id
		WHEN 'parent_of' THEN r.source_item_id
	END
	FROM reachable x
	JOIN work_item_relations r
		ON (r.kind = 'blocks' AND r.source_item_id = x.id)
		OR (r.kind = 'parent_of' AND r.target_item_id = x.id)
)
SELECT EXISTS(SELECT 1 FROM reachable WHERE id = ?)`, startID, goalID).Scan(&found)
	return found != 0, err
}

// preserveActiveRebaseBlockerTx prevents user-facing relation APIs from
// removing the system-owned dependency gate for a rebase that is still
// running. Callers hold the project writer lock across this check and the
// delete, so rebase finalization and relation removal have one serialization
// order: cleanup is allowed only after the running row has closed.
func preserveActiveRebaseBlockerTx(ctx context.Context, q workItemRelationQuerier, sourceID, targetID string, kind RelationKind) error {
	if kind != RelationBlocks {
		return nil
	}
	var protected int
	if err := q.QueryRowContext(ctx, `
SELECT EXISTS (
	SELECT 1
	FROM work_item_relations r
	JOIN feature_rebases fr ON fr.task_id = r.source_item_id
	WHERE r.source_item_id = ?
		AND r.target_item_id = ?
		AND r.kind = 'blocks'
		AND r.created_by = ?
		AND fr.state = 'running'
)`, sourceID, targetID, string(ActorSystem)).Scan(&protected); err != nil {
		return fmt.Errorf("check active feature rebase blocker: %w", err)
	}
	if protected != 0 {
		return ErrActiveRebaseBlocker
	}
	return nil
}

func (s *WorkItemService) Unlink(ctx context.Context, sourceID, targetID string, kind RelationKind) error {
	if err := validateRelationKind(kind); err != nil {
		return err
	}
	sourceID = strings.TrimSpace(sourceID)
	targetID = strings.TrimSpace(targetID)
	if kind == RelationRelatedTo && targetID < sourceID {
		sourceID, targetID = targetID, sourceID
	}
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sourceKind, err := workItemKindTx(ctx, tx, sourceID)
	if err != nil {
		return err
	}
	targetKind, err := workItemKindTx(ctx, tx, targetID)
	if err != nil {
		return err
	}
	if err := requireWorkItemRelationMutableTx(ctx, tx, sourceID, sourceKind); err != nil {
		return err
	}
	if err := requireWorkItemRelationMutableTx(ctx, tx, targetID, targetKind); err != nil {
		return err
	}
	if err := preserveActiveRebaseBlockerTx(ctx, tx, sourceID, targetID, kind); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
DELETE FROM work_item_relations
WHERE source_item_id = ? AND target_item_id = ? AND kind = ?`, sourceID, targetID, string(kind))
	if err != nil {
		return fmt.Errorf("unlink work items: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 0 && kind == RelationParentOf {
		if err := s.syncSubtreeFeatureCachesTx(ctx, tx, targetID, s.now().UTC()); err != nil {
			return err
		}
	}
	if err := reconcileEpicAncestorsTx(ctx, tx, []string{sourceID, targetID}, s.now().UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *WorkItemService) Parent(ctx context.Context, itemID string) (*WorkItemSummary, error) {
	parentID, err := s.ParentID(ctx, itemID)
	if err != nil || parentID == "" {
		return nil, err
	}
	item, err := s.Get(ctx, parentID)
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *WorkItemService) Children(ctx context.Context, itemID string) ([]WorkItemSummary, error) {
	return s.relatedItems(ctx, itemID, false)
}

func (s *WorkItemService) Descendants(ctx context.Context, itemID string) ([]WorkItemSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE descendants(id, depth) AS (
	SELECT target_item_id, 1 FROM work_item_relations
	WHERE source_item_id = ? AND kind = 'parent_of'
	UNION ALL
	SELECT r.target_item_id, descendants.depth + 1
	FROM descendants
	JOIN work_item_relations r ON r.source_item_id = descendants.id AND r.kind = 'parent_of'
)
SELECT id FROM descendants ORDER BY depth, id`, strings.TrimSpace(itemID))
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

func (s *WorkItemService) Ancestors(ctx context.Context, itemID string) ([]WorkItemSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE ancestors(id, depth) AS (
	SELECT source_item_id, 1 FROM work_item_relations
	WHERE target_item_id = ? AND kind = 'parent_of'
	UNION ALL
	SELECT r.source_item_id, ancestors.depth + 1
	FROM ancestors
	JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
)
SELECT id FROM ancestors ORDER BY depth, id`, strings.TrimSpace(itemID))
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

func (s *WorkItemService) relatedItems(ctx context.Context, itemID string, parents bool) ([]WorkItemSummary, error) {
	column, match := "target_item_id", "source_item_id"
	if parents {
		column, match = "source_item_id", "target_item_id"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+column+` FROM work_item_relations WHERE `+match+` = ? AND kind = 'parent_of' ORDER BY `+column, strings.TrimSpace(itemID))
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

func (s *WorkItemService) Relations(ctx context.Context, itemID string) ([]WorkItemRelation, error) {
	result, err := s.RelationsForItems(ctx, []string{itemID})
	return result[strings.TrimSpace(itemID)], err
}

func (s *WorkItemService) RelationsForItems(ctx context.Context, itemIDs []string) (map[string][]WorkItemRelation, error) {
	result := make(map[string][]WorkItemRelation)
	ids := uniqueTrimmed(itemIDs)
	if len(ids) == 0 {
		return result, nil
	}
	args := make([]any, len(ids))
	for i := range ids {
		args[i] = ids[i]
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT source_item_id, target_item_id, kind, created_by, created_at
FROM work_item_relations
WHERE `+inPredicate("source_item_id", len(ids))+` OR `+inPredicate("target_item_id", len(ids))+`
ORDER BY created_at, source_item_id, target_item_id, kind`, append(args, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rawRelation struct {
		source, target, kind, actor, created string
	}
	var raw []rawRelation
	var endpointIDs []string
	for rows.Next() {
		var relation rawRelation
		if err := rows.Scan(&relation.source, &relation.target, &relation.kind, &relation.actor, &relation.created); err != nil {
			return nil, err
		}
		raw = append(raw, relation)
		endpointIDs = append(endpointIDs, relation.source, relation.target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items, err := s.GetMany(ctx, endpointIDs)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]WorkItemSummary, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	for _, value := range raw {
		createdAt, err := parseTime(value.created)
		if err != nil {
			return nil, err
		}
		relation := WorkItemRelation{
			Source: byID[value.source], Target: byID[value.target], Kind: RelationKind(value.kind),
			CreatedBy: Actor(value.actor), CreatedAt: createdAt,
		}
		relation.Resolved = relation.Kind == RelationBlocks && relation.Source.State.Terminal
		if requested[value.source] {
			result[value.source] = append(result[value.source], relation)
		}
		if value.target != value.source && requested[value.target] {
			result[value.target] = append(result[value.target], relation)
		}
	}
	return result, nil
}

// EffectiveBlockers includes blockers attached to itemID and to each of its
// organizational ancestors. Resolved blockers are returned with an explicit
// verdict so callers never have to interpret kind-specific states.
func (s *WorkItemService) EffectiveBlockers(ctx context.Context, itemID string, includeResolved bool) ([]WorkItemBlocker, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE ancestors(id, depth) AS (
	VALUES (?, 0)
	UNION ALL
	SELECT r.source_item_id, ancestors.depth + 1
	FROM ancestors
	JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'parent_of'
)
SELECT r.source_item_id, ancestors.id, ancestors.depth
FROM ancestors
JOIN work_item_relations r ON r.target_item_id = ancestors.id AND r.kind = 'blocks'
ORDER BY ancestors.depth, r.source_item_id`, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rawBlocker struct {
		blocker, via string
		depth        int
	}
	var raw []rawBlocker
	var ids []string
	for rows.Next() {
		var blocker rawBlocker
		if err := rows.Scan(&blocker.blocker, &blocker.via, &blocker.depth); err != nil {
			return nil, err
		}
		raw = append(raw, blocker)
		ids = append(ids, blocker.blocker, blocker.via)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items, err := s.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]WorkItemSummary, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	var blockers []WorkItemBlocker
	seen := map[string]bool{}
	for _, rawBlocker := range raw {
		item := byID[rawBlocker.blocker]
		resolved := item.State.Terminal
		if resolved && !includeResolved || seen[rawBlocker.blocker] {
			continue
		}
		seen[rawBlocker.blocker] = true
		blocker := WorkItemBlocker{Item: item, Direct: rawBlocker.depth == 0, Resolved: resolved}
		if !blocker.Direct {
			via := byID[rawBlocker.via].WorkItemRef
			blocker.ViaItem = &via
		}
		blockers = append(blockers, blocker)
	}
	sort.SliceStable(blockers, func(i, j int) bool {
		if blockers[i].Resolved != blockers[j].Resolved {
			return !blockers[i].Resolved
		}
		return blockers[i].Item.ID < blockers[j].Item.ID
	})
	return blockers, nil
}

func uniqueTrimmed(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
