package coordinator

import (
	"context"
	"sort"
	"strings"
)

// WorkItemDirectChildren summarizes only an item's immediate containment edges.
type WorkItemDirectChildren struct {
	Total    int `json:"total"`
	Terminal int `json:"terminal"`
}

// WorkItemDescendantTasks summarizes every task below an item in the
// containment hierarchy. A task is counted at most once for each ancestor.
type WorkItemDescendantTasks struct {
	Total                int `json:"total"`
	Unscheduled          int `json:"unscheduled"`
	Scheduled            int `json:"scheduled"`
	InProgress           int `json:"in_progress"`
	OtherOpen            int `json:"other_open"`
	SuccessfulTerminal   int `json:"successful_terminal"`
	UnsuccessfulTerminal int `json:"unsuccessful_terminal"`
	EffectivelyBlocked   int `json:"effectively_blocked"`
}

// WorkItemRollup is the durable portion of the portfolio read model. Runtime
// attention and Git state are deliberately composed by the API layer.
type WorkItemRollup struct {
	DirectChildren  WorkItemDirectChildren  `json:"direct_children"`
	DescendantTasks WorkItemDescendantTasks `json:"descendant_tasks"`
}

// WorkItemOverview is an internal coordinator aggregate. DescendantTaskIDs are
// retained for batched runtime attention composition and are not serialized.
type WorkItemOverview struct {
	Item              WorkItemSummary `json:"item"`
	Rollup            WorkItemRollup  `json:"rollup"`
	DescendantTaskIDs []string        `json:"-"`
	DescendantItemIDs []string        `json:"-"`
}

// WorkItemContext is a bounded hierarchy and relation view around one item.
type WorkItemContext struct {
	Overview  WorkItemOverview
	Ancestors []WorkItemSummary
	Children  []WorkItemSummary
	Blockers  []WorkItemBlocker
	Relations []WorkItemRelation
}

// Overview loads one project snapshot and derives every rollup with graph
// traversal in memory. It does not issue per-item detail reads.
func (s *WorkItemService) Overview(ctx context.Context, terminal *bool) ([]WorkItemOverview, error) {
	all, err := s.overviewSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]WorkItemOverview, 0, len(all))
	for _, aggregate := range all {
		if terminal == nil || aggregate.Item.State.Terminal == *terminal {
			result = append(result, aggregate)
		}
	}
	return result, nil
}

func (s *WorkItemService) overviewSnapshot(ctx context.Context) ([]WorkItemOverview, error) {
	items, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]WorkItemSummary, len(items))
	children := make(map[string][]string, len(items))
	for _, item := range items {
		byID[item.ID] = item
		if item.ParentItemID != "" {
			children[item.ParentItemID] = append(children[item.ParentItemID], item.ID)
		}
	}
	for parentID := range children {
		sort.Strings(children[parentID])
	}

	result := make([]WorkItemOverview, 0, len(items))
	for _, item := range items {
		aggregate := WorkItemOverview{Item: item}
		for _, childID := range children[item.ID] {
			aggregate.Rollup.DirectChildren.Total++
			if byID[childID].State.Terminal {
				aggregate.Rollup.DirectChildren.Terminal++
			}
		}

		seen := map[string]bool{item.ID: true}
		stack := append([]string(nil), children[item.ID]...)
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if seen[id] {
				continue
			}
			seen[id] = true
			descendant, ok := byID[id]
			if !ok {
				continue
			}
			aggregate.DescendantItemIDs = append(aggregate.DescendantItemIDs, id)
			if descendant.Kind == WorkItemTask {
				aggregate.DescendantTaskIDs = append(aggregate.DescendantTaskIDs, id)
				addDescendantTask(&aggregate.Rollup.DescendantTasks, descendant)
			}
			stack = append(stack, children[id]...)
		}
		sort.Strings(aggregate.DescendantTaskIDs)
		sort.Strings(aggregate.DescendantItemIDs)
		result = append(result, aggregate)
	}
	return result, nil
}

func addDescendantTask(rollup *WorkItemDescendantTasks, item WorkItemSummary) {
	rollup.Total++
	if item.UnresolvedBlockers > 0 {
		rollup.EffectivelyBlocked++
	}
	if item.State.Terminal {
		if item.State.Successful {
			rollup.SuccessfulTerminal++
		} else {
			rollup.UnsuccessfulTerminal++
		}
		return
	}
	switch strings.TrimSpace(item.State.Status) {
	case "":
		rollup.Unscheduled++
	case "unscheduled":
		rollup.Unscheduled++
	case "scheduled":
		rollup.Scheduled++
	case "in_progress":
		rollup.InProgress++
	default:
		rollup.OtherOpen++
	}
}

// Context returns only the selected item, its ancestor path, direct children,
// direct relations, and effective blockers. The rollup remains subtree-wide.
func (s *WorkItemService) Context(ctx context.Context, id string) (WorkItemContext, error) {
	id = strings.TrimSpace(id)
	all, err := s.overviewSnapshot(ctx)
	if err != nil {
		return WorkItemContext{}, err
	}
	byID := make(map[string]WorkItemOverview, len(all))
	for _, aggregate := range all {
		byID[aggregate.Item.ID] = aggregate
	}
	aggregate, ok := byID[id]
	if !ok {
		return WorkItemContext{}, ErrWorkItemNotFound
	}

	result := WorkItemContext{Overview: aggregate}
	for parentID := aggregate.Item.ParentItemID; parentID != ""; {
		parent, exists := byID[parentID]
		if !exists {
			break
		}
		result.Ancestors = append(result.Ancestors, parent.Item)
		parentID = parent.Item.ParentItemID
	}
	for left, right := 0, len(result.Ancestors)-1; left < right; left, right = left+1, right-1 {
		result.Ancestors[left], result.Ancestors[right] = result.Ancestors[right], result.Ancestors[left]
	}
	for _, candidate := range all {
		if candidate.Item.ParentItemID == id {
			result.Children = append(result.Children, candidate.Item)
		}
	}
	sort.Slice(result.Children, func(i, j int) bool { return result.Children[i].ID < result.Children[j].ID })

	result.Blockers, err = s.EffectiveBlockers(ctx, id, true)
	if err != nil {
		return WorkItemContext{}, err
	}
	result.Relations, err = s.Relations(ctx, id)
	if err != nil {
		return WorkItemContext{}, err
	}
	return result, nil
}
