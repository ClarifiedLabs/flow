package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrFlowNotFound  = errors.New("flow not found")
	ErrFlowNameTaken = errors.New("a flow with this name already exists")
	ErrFlowIsDefault = errors.New("flow is the project default; set another default before deleting it")
	ErrFlowInUse     = errors.New("flow is selected by an task or another workflow")
)

const defaultFlowMetadataKey = "default_flow_id"

// FlowGate is a work phase's exit policy: auto-advance to the next phase, or
// pause for human approval of the phase's handoff.
type FlowGate string

const (
	FlowGateAuto  FlowGate = "auto"
	FlowGateHuman FlowGate = "human"
)

// FlowReviewRole says which review round a flow review agent joins: reviewers
// run in critique, verifiers in acceptance.
type FlowReviewRole string

const (
	FlowReviewRoleReviewer FlowReviewRole = "reviewer"
	FlowReviewRoleVerifier FlowReviewRole = "verifier"
)

// Flow is a project-owned trusted workflow graph. Tasks freeze a resolved
// FlowSnapshot when they are scheduled.
type Flow struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	StartNode        string            `json:"start_node,omitempty"`
	TransitionBudget int               `json:"transition_budget,omitempty"`
	Nodes            []FlowNode        `json:"nodes,omitempty"`
	Edges            []FlowEdge        `json:"edges,omitempty"`
	FixAgentDefID    string            `json:"-"`
	Builtin          bool              `json:"builtin"`
	Default          bool              `json:"default"`
	Phases           []FlowPhase       `json:"-"`
	ReviewAgents     []FlowReviewAgent `json:"-"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type FlowPhase struct {
	ID         string   `json:"id"`
	Position   int      `json:"position"`
	Name       string   `json:"name"`
	AgentDefID string   `json:"agent_def_id"`
	Gate       FlowGate `json:"gate"`
}

type FlowReviewAgent struct {
	ID         string         `json:"id"`
	Role       FlowReviewRole `json:"role"`
	AgentDefID string         `json:"agent_def_id"`
	Position   int            `json:"position"`
	Required   bool           `json:"required"`
}

type FlowPhaseInput struct {
	Name       string   `json:"name"`
	AgentDefID string   `json:"agent_def_id"`
	Gate       FlowGate `json:"gate"`
}

type FlowReviewAgentInput struct {
	Role       FlowReviewRole `json:"role"`
	AgentDefID string         `json:"agent_def_id"`
	Required   *bool          `json:"required,omitempty"`
}

type FlowInput struct {
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	StartNode        string                 `json:"start_node,omitempty"`
	TransitionBudget int                    `json:"transition_budget,omitempty"`
	Nodes            []FlowNodeInput        `json:"nodes,omitempty"`
	Edges            []FlowEdgeInput        `json:"edges,omitempty"`
	FixAgentDefID    string                 `json:"-"`
	Phases           []FlowPhaseInput       `json:"-"`
	ReviewAgents     []FlowReviewAgentInput `json:"-"`
}

// AgentDefSnapshot is the frozen copy of an agent definition carried in a
// FlowSnapshot: everything a job needs to launch the agent, immune to later
// edits of the live agent_defs row.
type AgentDefSnapshot struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Harness         string `json:"harness"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	Prompt          string `json:"prompt,omitempty"`
}

// ModelSelectionArgs renders the snapshot's model/effort choice as harness
// argv tokens (empty when neither is set).
func (a AgentDefSnapshot) ModelSelectionArgs() ([]string, error) {
	return flowharness.SerializeModelSelection(a.Harness, a.Model, a.ReasoningEffort)
}

type FlowPhaseSnapshot struct {
	Name  string           `json:"name"`
	Gate  FlowGate         `json:"gate"`
	Agent AgentDefSnapshot `json:"agent"`
}

type FlowReviewAgentSnapshot struct {
	Role     FlowReviewRole   `json:"role"`
	Blocking bool             `json:"blocking"`
	Agent    AgentDefSnapshot `json:"agent"`
}

// FlowSnapshot is the fully resolved graph a workflow run executes. Agent
// definitions are frozen into node configs when the task is scheduled.
type FlowSnapshot struct {
	FlowID           string                    `json:"flow_id"`
	FlowName         string                    `json:"flow_name"`
	StartNode        string                    `json:"start_node,omitempty"`
	TransitionBudget int                       `json:"transition_budget,omitempty"`
	Nodes            []FlowNodeSnapshot        `json:"nodes,omitempty"`
	Edges            []FlowEdge                `json:"edges,omitempty"`
	Phases           []FlowPhaseSnapshot       `json:"-"`
	ReviewAgents     []FlowReviewAgentSnapshot `json:"-"`
	FixAgent         *AgentDefSnapshot         `json:"-"`
}

// FixAgentOrLastPhase returns the flow's designated fix-cycle agent, falling
// back to the final work phase's agent.
func (s FlowSnapshot) FixAgentOrLastPhase() (AgentDefSnapshot, error) {
	if s.FixAgent != nil {
		return *s.FixAgent, nil
	}
	if len(s.Phases) == 0 {
		return AgentDefSnapshot{}, errors.New("flow snapshot has no phases")
	}
	return s.Phases[len(s.Phases)-1].Agent, nil
}

// PhaseAt returns the work phase at index, or false when index is out of range.
func (s FlowSnapshot) PhaseAt(index int) (FlowPhaseSnapshot, bool) {
	if index < 0 || index >= len(s.Phases) {
		return FlowPhaseSnapshot{}, false
	}
	return s.Phases[index], true
}

// FlowService manages the per-project flow catalog and the project default.
type FlowService struct {
	db        *sql.DB
	agentDefs *AgentDefService
	now       func() time.Time
}

func NewFlowService(database *sql.DB) *FlowService {
	return NewFlowServiceWithAgentDefs(database, NewAgentDefService(database))
}

func NewFlowServiceWithAgentDefs(database *sql.DB, agentDefs *AgentDefService) *FlowService {
	return &FlowService{db: database, agentDefs: agentDefs, now: sqlitex.UTCNow}
}

func normalizeFlowInput(input FlowInput) (FlowInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return FlowInput{}, errors.New("flow name is required")
	}
	input.Description = strings.TrimSpace(input.Description)
	input.FixAgentDefID = strings.TrimSpace(input.FixAgentDefID)
	if len(input.Phases) > 0 || len(input.ReviewAgents) > 0 || input.FixAgentDefID != "" {
		return FlowInput{}, errors.New("ordered phases and legacy review configuration are not supported")
	}
	if len(input.Nodes) == 0 && strings.TrimSpace(input.StartNode) == "" {
		return FlowInput{}, errors.New("flow requires a graph definition")
	}
	return normalizeGraphInput(input)
}

func (s *FlowService) Create(ctx context.Context, input FlowInput) (Flow, error) {
	return s.create(ctx, input, false)
}

func (s *FlowService) create(ctx context.Context, input FlowInput, builtin bool) (Flow, error) {
	input, err := normalizeFlowInput(input)
	if err != nil {
		return Flow{}, err
	}
	id, err := randomPrefixedID("fl")
	if err != nil {
		return Flow{}, err
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Flow{}, err
	}
	defer tx.Rollback()

	now := sqlitex.FormatTime(s.now().UTC())
	budget := input.TransitionBudget
	if budget == 0 {
		budget = DefaultFlowTransitionBudget
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO flows (id, name, description, start_node_key, transition_budget, builtin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, input.Name, input.Description, input.StartNode, budget, boolToInt(builtin), now, now)
	if err != nil {
		if isUniqueViolation(err, "flows.name") {
			return Flow{}, ErrFlowNameTaken
		}
		return Flow{}, fmt.Errorf("insert flow: %w", err)
	}
	if err := s.insertFlowChildren(ctx, tx, id, input); err != nil {
		return Flow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Flow{}, err
	}

	return s.Get(ctx, id)
}

func (s *FlowService) Update(ctx context.Context, id string, input FlowInput) (Flow, error) {
	input, err := normalizeFlowInput(input)
	if err != nil {
		return Flow{}, err
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Flow{}, err
	}
	defer tx.Rollback()

	budget := input.TransitionBudget
	if budget == 0 {
		budget = DefaultFlowTransitionBudget
	}
	result, err := tx.ExecContext(ctx, `
UPDATE flows SET name = ?, description = ?, start_node_key = ?, transition_budget = ?, updated_at = ?
WHERE id = ?`, input.Name, input.Description, input.StartNode, budget,
		sqlitex.FormatTime(s.now().UTC()), id)
	if err != nil {
		if isUniqueViolation(err, "flows.name") {
			return Flow{}, ErrFlowNameTaken
		}
		return Flow{}, fmt.Errorf("update flow: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Flow{}, err
	}
	if affected == 0 {
		return Flow{}, ErrFlowNotFound
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM flow_edges WHERE flow_id = ?`, id); err != nil {
		return Flow{}, fmt.Errorf("clear flow edges: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM flow_nodes WHERE flow_id = ?`, id); err != nil {
		return Flow{}, fmt.Errorf("clear flow nodes: %w", err)
	}
	if err := s.insertFlowChildren(ctx, tx, id, input); err != nil {
		return Flow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Flow{}, err
	}

	return s.Get(ctx, id)
}

func (s *FlowService) insertFlowChildren(ctx context.Context, tx *sqlitex.Tx, flowID string, input FlowInput) error {
	if err := validateGraphReferencesInTx(ctx, tx, s.agentDefs, input); err != nil {
		return err
	}
	for position, node := range input.Nodes {
		nodeID, err := randomPrefixedID("fn")
		if err != nil {
			return err
		}
		configJSON, err := encodeNodeConfig(node.Config)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO flow_nodes (id, flow_id, node_key, name, kind, position, config_json)
VALUES (?, ?, ?, ?, ?, ?, ?)`, nodeID, flowID, node.Key, node.Name, string(node.Kind), position, configJSON); err != nil {
			return fmt.Errorf("insert flow node %q: %w", node.Key, err)
		}
	}
	for _, edge := range input.Edges {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO flow_edges (flow_id, from_node_key, outcome, to_node_key)
VALUES (?, ?, ?, ?)`, flowID, edge.From, edge.Outcome, edge.To); err != nil {
			return fmt.Errorf("insert flow edge %s.%s: %w", edge.From, edge.Outcome, err)
		}
	}
	return nil
}

func validateGraphReferencesInTx(ctx context.Context, tx *sqlitex.Tx, defs *AgentDefService, input FlowInput) error {
	checkAgent := func(nodeKey, id string) error {
		if defs == nil {
			return fmt.Errorf("node %q references unknown agent definition %q", nodeKey, id)
		}
		if _, err := defs.resolveWith(ctx, tx, id); err != nil {
			if errors.Is(err, ErrAgentDefNotFound) {
				return fmt.Errorf("node %q references unknown agent definition %q", nodeKey, id)
			}
			return err
		}
		return nil
	}
	for _, node := range input.Nodes {
		switch node.Kind {
		case NodeAgent:
			if err := checkAgent(node.Key, node.Config.Agent.AgentDefID); err != nil {
				return err
			}
		case NodeChangeReview:
			if err := checkAgent(node.Key, node.Config.ChangeReview.AggregatorAgentDefID); err != nil {
				return err
			}
			for _, agent := range node.Config.ChangeReview.Agents {
				if err := checkAgent(node.Key, agent.AgentDefID); err != nil {
					return err
				}
			}
		case NodeVerifyChange:
			for _, agent := range node.Config.VerifyChange.Agents {
				if err := checkAgent(node.Key, agent.AgentDefID); err != nil {
					return err
				}
			}
		case NodeMaterializeTaskSet:
			if err := requireFlowTx(ctx, tx, node.Config.MaterializeTaskSet.DefaultChildFlowID); err != nil {
				return fmt.Errorf("node %q default child flow: %w", node.Key, err)
			}
		}
	}
	return nil
}

func (s *FlowService) Delete(ctx context.Context, id string) error {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var defaultID string
	err = tx.QueryRowContext(ctx, `SELECT value FROM app_metadata WHERE key = ?`, defaultFlowMetadataKey).Scan(&defaultID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read default flow: %w", err)
	}
	if defaultID == id {
		return ErrFlowIsDefault
	}
	var references int
	if err := tx.QueryRowContext(ctx, `
SELECT
	(SELECT COUNT(*) FROM tasks WHERE flow_id = ?) +
	(SELECT COUNT(*) FROM flow_nodes
	 WHERE kind = 'materialize_task_set'
	   AND json_extract(config_json, '$.materialize_task_set.default_child_flow_id') = ?) +
	(SELECT COUNT(*)
	 FROM workflow_runs wr
	 WHERE wr.state IN ('scheduled', 'running', 'waiting')
	   AND (wr.flow_id = ? OR json_extract(wr.flow_snapshot_json, '$.flow_id') = ?)) +
	(SELECT COUNT(*)
	 FROM workflow_runs wr, json_each(wr.flow_snapshot_json, '$.nodes') node
	 WHERE wr.state IN ('scheduled', 'running', 'waiting')
	   AND json_extract(node.value, '$.config.materialize_task_set.default_child_flow_id') = ?) +
	(SELECT COUNT(*)
	 FROM workflow_artifacts artifact
	 JOIN workflow_runs wr ON wr.id = artifact.workflow_run_id,
	      json_each(artifact.payload_json, '$.items') task
	 WHERE artifact.kind = 'task_set'
	   AND wr.state IN ('scheduled', 'running', 'waiting')
	   AND trim(COALESCE(json_extract(task.value, '$.flow_id'), '')) = ?)`, id, id, id, id, id, id).Scan(&references); err != nil {
		return fmt.Errorf("inspect flow references: %w", err)
	}
	if references > 0 {
		return ErrFlowInUse
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM flows WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete flow: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrFlowNotFound
	}
	return tx.Commit(ctx)
}

func (s *FlowService) Get(ctx context.Context, id string) (Flow, error) {
	flows, err := s.list(ctx, "f.id = ?", id)
	if err != nil {
		return Flow{}, err
	}
	if len(flows) == 0 {
		return Flow{}, ErrFlowNotFound
	}
	return flows[0], nil
}

func (s *FlowService) GetByName(ctx context.Context, name string) (Flow, error) {
	flows, err := s.list(ctx, "f.name = ?", strings.TrimSpace(name))
	if err != nil {
		return Flow{}, err
	}
	if len(flows) == 0 {
		return Flow{}, ErrFlowNotFound
	}
	return flows[0], nil
}

func (s *FlowService) List(ctx context.Context) ([]Flow, error) {
	return s.list(ctx, "1 = 1")
}

func (s *FlowService) list(ctx context.Context, where string, args ...any) ([]Flow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT f.id, f.name, f.description, f.start_node_key, f.transition_budget, f.builtin, f.created_at, f.updated_at
FROM flows f WHERE `+where+` ORDER BY f.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list flows: %w", err)
	}
	defer rows.Close()

	flows := []Flow{}
	index := map[string]int{}
	for rows.Next() {
		var flow Flow
		var builtin int
		var createdAt, updatedAt string
		if err := rows.Scan(&flow.ID, &flow.Name, &flow.Description, &flow.StartNode, &flow.TransitionBudget, &builtin, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		flow.Builtin = builtin != 0
		if flow.CreatedAt, err = sqlitex.ParseTime(createdAt); err != nil {
			return nil, fmt.Errorf("parse flow created_at: %w", err)
		}
		if flow.UpdatedAt, err = sqlitex.ParseTime(updatedAt); err != nil {
			return nil, fmt.Errorf("parse flow updated_at: %w", err)
		}
		flow.Phases = []FlowPhase{}
		flow.ReviewAgents = []FlowReviewAgent{}
		flow.Nodes = []FlowNode{}
		flow.Edges = []FlowEdge{}
		index[flow.ID] = len(flows)
		flows = append(flows, flow)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(flows) == 0 {
		return flows, nil
	}

	nodeRows, err := s.db.QueryContext(ctx, `
SELECT n.id, n.flow_id, n.node_key, n.name, n.kind, n.position, n.config_json
FROM flow_nodes n JOIN flows f ON f.id = n.flow_id
WHERE `+where+` ORDER BY n.flow_id, n.position`, args...)
	if err != nil {
		return nil, fmt.Errorf("list flow nodes: %w", err)
	}
	defer nodeRows.Close()
	for nodeRows.Next() {
		var node FlowNode
		var flowID, kind, configJSON string
		if err := nodeRows.Scan(&node.ID, &flowID, &node.Key, &node.Name, &kind, &node.Position, &configJSON); err != nil {
			return nil, err
		}
		node.Kind = NodeKind(kind)
		node.Config, err = decodeNodeConfig(configJSON)
		if err != nil {
			return nil, fmt.Errorf("flow %s node %s: %w", flowID, node.Key, err)
		}
		if i, ok := index[flowID]; ok {
			flows[i].Nodes = append(flows[i].Nodes, node)
		}
	}
	if err := nodeRows.Err(); err != nil {
		return nil, err
	}

	edgeRows, err := s.db.QueryContext(ctx, `
SELECT e.flow_id, e.from_node_key, e.outcome, e.to_node_key
FROM flow_edges e JOIN flows f ON f.id = e.flow_id
WHERE `+where+` ORDER BY e.flow_id, e.from_node_key, e.outcome`, args...)
	if err != nil {
		return nil, fmt.Errorf("list flow edges: %w", err)
	}
	defer edgeRows.Close()
	for edgeRows.Next() {
		var flowID string
		var edge FlowEdge
		if err := edgeRows.Scan(&flowID, &edge.From, &edge.Outcome, &edge.To); err != nil {
			return nil, err
		}
		if i, ok := index[flowID]; ok {
			flows[i].Edges = append(flows[i].Edges, edge)
		}
	}
	if err := edgeRows.Err(); err != nil {
		return nil, err
	}

	defaultID, err := s.DefaultFlowID(ctx)
	if err != nil {
		return nil, err
	}
	for i := range flows {
		flows[i].Default = flows[i].ID == defaultID
	}

	return flows, nil
}

// DefaultFlowID returns the project's default flow id, or "" when unset.
func (s *FlowService) DefaultFlowID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM app_metadata WHERE key = ?`, defaultFlowMetadataKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read default flow: %w", err)
	}
	return id, nil
}

func (s *FlowService) SetDefaultFlow(ctx context.Context, id string) error {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM flows WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrFlowNotFound
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO app_metadata (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		defaultFlowMetadataKey, id, sqlitex.FormatTime(s.now().UTC())); err != nil {
		return fmt.Errorf("set default flow: %w", err)
	}
	return tx.Commit(ctx)
}

// ResolveSnapshot freezes the flow (default flow when flowID is empty) into a
// FlowSnapshot: phases and review agents joined with full agent-def copies.
func (s *FlowService) ResolveSnapshot(ctx context.Context, flowID string) (FlowSnapshot, error) {
	flowID = strings.TrimSpace(flowID)
	if flowID == "" {
		defaultID, err := s.DefaultFlowID(ctx)
		if err != nil {
			return FlowSnapshot{}, err
		}
		if defaultID == "" {
			return FlowSnapshot{}, errors.New("no flow selected and no project default flow configured")
		}
		flowID = defaultID
	}

	flow, err := s.Get(ctx, flowID)
	if err != nil {
		return FlowSnapshot{}, err
	}
	if len(flow.Nodes) == 0 {
		return FlowSnapshot{}, fmt.Errorf("flow %q has no executable definition", flow.Name)
	}

	snapshotAgent := func(agentDefID string) (AgentDefSnapshot, error) {
		def, err := s.agentDefs.Resolve(ctx, agentDefID)
		if err != nil {
			return AgentDefSnapshot{}, fmt.Errorf("flow %q: %w", flow.Name, err)
		}
		return AgentDefSnapshot{
			ID:              def.ID,
			Name:            def.Name,
			Harness:         def.Harness,
			Model:           def.Model,
			ReasoningEffort: def.ReasoningEffort,
			Prompt:          def.Prompt,
		}, nil
	}

	snapshot := FlowSnapshot{
		FlowID: flow.ID, FlowName: flow.Name, StartNode: flow.StartNode,
		TransitionBudget: flow.TransitionBudget, Edges: append([]FlowEdge(nil), flow.Edges...),
	}
	for _, node := range flow.Nodes {
		snapshotNode := FlowNodeSnapshot{Key: node.Key, Name: node.Name, Kind: node.Kind}
		switch node.Kind {
		case NodeAgent:
			agent, err := snapshotAgent(node.Config.Agent.AgentDefID)
			if err != nil {
				return FlowSnapshot{}, err
			}
			snapshotNode.Config.Agent = &AgentNodeSnapshotConfig{
				Agent: agent, Workspace: node.Config.Agent.Workspace, Artifact: node.Config.Agent.Artifact,
			}
		case NodeAutomatedChecks:
			snapshotNode.Config.AutomatedChecks = &AutomatedChecksNodeConfig{}
		case NodeChangeReview:
			aggregator, err := snapshotAgent(node.Config.ChangeReview.AggregatorAgentDefID)
			if err != nil {
				return FlowSnapshot{}, err
			}
			config := &ChangeReviewNodeSnapshotConfig{Aggregator: aggregator}
			for _, inputAgent := range node.Config.ChangeReview.Agents {
				agent, err := snapshotAgent(inputAgent.AgentDefID)
				if err != nil {
					return FlowSnapshot{}, err
				}
				config.Agents = append(config.Agents, SnapshotReviewAgent{Blocking: reviewAgentBlocking(inputAgent), Agent: agent})
			}
			snapshotNode.Config.ChangeReview = config
		case NodeHumanGate:
			copyConfig := *node.Config.HumanGate
			copyConfig.Outcomes = append([]string(nil), node.Config.HumanGate.Outcomes...)
			snapshotNode.Config.HumanGate = &copyConfig
		case NodeVerifyChange:
			config := &VerifyChangeNodeSnapshotConfig{}
			for _, inputAgent := range node.Config.VerifyChange.Agents {
				agent, err := snapshotAgent(inputAgent.AgentDefID)
				if err != nil {
					return FlowSnapshot{}, err
				}
				config.Agents = append(config.Agents, SnapshotReviewAgent{Blocking: reviewAgentBlocking(inputAgent), Agent: agent})
			}
			snapshotNode.Config.VerifyChange = config
		case NodeMaterializeTaskSet:
			copyConfig := *node.Config.MaterializeTaskSet
			snapshotNode.Config.MaterializeTaskSet = &copyConfig
		case NodeMergeChange:
			snapshotNode.Config.MergeChange = &MergeChangeNodeConfig{}
		case NodeFinalizeRebase:
			snapshotNode.Config.FinalizeRebase = &FinalizeRebaseNodeConfig{}
		case NodeTerminal:
			copyConfig := *node.Config.Terminal
			snapshotNode.Config.Terminal = &copyConfig
		}
		snapshot.Nodes = append(snapshot.Nodes, snapshotNode)
	}
	return snapshot, nil
}

// Names of the built-in flows every project is seeded with. Seeding matches
// on these names so a project that already has a flow with the same name —
// seeded earlier or user-created — is left untouched.
const (
	CodingFlowName        = "coding"
	PlanningFlowName      = "planning"
	FeatureRebaseFlowName = "feature-rebase"
)

// SeedDefaults populates a project with the built-in coding, planning, and
// feature-rebase graphs. Their agent definitions are coordinator-global rows
// inherited by the project, so seeding never creates project-local agent
// definitions. Seeding is per-name so new built-ins roll out to existing
// projects without disturbing customized or user-created flows; the default
// flow is only chosen on a completely fresh seed.
func (s *FlowService) SeedDefaults(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM flows`).Scan(&count); err != nil {
		return fmt.Errorf("check seed state: %w", err)
	}
	fresh := count == 0

	defIDs := map[string]string{}
	for _, seed := range defaultAgentDefSeeds() {
		inherited, err := s.agentDefs.getInheritedByName(ctx, seed.name)
		if err != nil {
			return fmt.Errorf("look up global default agent def %s: %w", seed.name, err)
		}
		defIDs[seed.name] = inherited.ID
	}

	codingID, err := s.flowIDByName(ctx, CodingFlowName)
	if err != nil {
		return err
	}
	if codingID == "" {
		coding, err := s.create(ctx, codingFlowInput(defIDs), true)
		if err != nil {
			return fmt.Errorf("seed coding flow: %w", err)
		}
		codingID = coding.ID
	}

	planningID, err := s.flowIDByName(ctx, PlanningFlowName)
	if err != nil {
		return err
	}
	if planningID == "" {
		if _, err := s.create(ctx, planningFlowInput(defIDs, codingID), true); err != nil {
			return fmt.Errorf("seed planning flow: %w", err)
		}
	}

	rebaseID, err := s.flowIDByName(ctx, FeatureRebaseFlowName)
	if err != nil {
		return err
	}
	if rebaseID == "" {
		if _, err := s.create(ctx, featureRebaseFlowInput(defIDs), true); err != nil {
			return fmt.Errorf("seed feature rebase flow: %w", err)
		}
	}

	if !fresh {
		return nil
	}
	return s.SetDefaultFlow(ctx, codingID)
}

// flowIDByName returns the id of the flow with the given name, or "" when the
// project has none.
func (s *FlowService) flowIDByName(ctx context.Context, name string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM flows WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up flow %s: %w", name, err)
	}
	return id, nil
}

func codingFlowInput(defIDs map[string]string) FlowInput {
	return FlowInput{
		Name:             CodingFlowName,
		Description:      "Implement, check, review, verify, and merge a change.",
		StartNode:        "implement",
		TransitionBudget: DefaultFlowTransitionBudget,
		Nodes: []FlowNodeInput{
			{Key: "implement", Name: "Implement", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: defIDs["author"], Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "checks", Name: "Automated checks", Kind: NodeAutomatedChecks, Config: FlowNodeConfig{AutomatedChecks: &AutomatedChecksNodeConfig{}}},
			{Key: "review", Name: "Code and security review", Kind: NodeChangeReview, Config: FlowNodeConfig{ChangeReview: &ChangeReviewNodeConfig{Agents: []ReviewAgentConfig{{AgentDefID: defIDs["code-reviewer"]}, {AgentDefID: defIDs["security-reviewer"]}}, AggregatorAgentDefID: defIDs["review-aggregator"]}}},
			{Key: "verify", Name: "Verify requirements", Kind: NodeVerifyChange, Config: FlowNodeConfig{VerifyChange: &VerifyChangeNodeConfig{Agents: []ReviewAgentConfig{{AgentDefID: defIDs["verifier"]}}}}},
			{Key: "human-review", Name: "Human change review", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Instructions: "Review the change and choose whether it can proceed.", Outcomes: []string{"approved", "changes_requested", "rejected"}}}},
			{Key: "merge", Name: "Merge change", Kind: NodeMergeChange, Config: FlowNodeConfig{MergeChange: &MergeChangeNodeConfig{}}},
			{Key: "done", Name: "Merged", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionMerged}}},
			{Key: "rejected", Name: "Rejected", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionRejected}}},
		},
		Edges: []FlowEdgeInput{
			{From: "implement", Outcome: "completed", To: "checks"},
			{From: "checks", Outcome: "passed", To: "review"},
			{From: "checks", Outcome: "failed", To: "implement"},
			{From: "review", Outcome: "approved", To: "verify"},
			{From: "review", Outcome: "changes_requested", To: "implement"},
			{From: "verify", Outcome: "passed", To: "human-review"},
			{From: "verify", Outcome: "changes_requested", To: "implement"},
			{From: "human-review", Outcome: "approved", To: "merge"},
			{From: "human-review", Outcome: "changes_requested", To: "implement"},
			{From: "human-review", Outcome: "rejected", To: "rejected"},
			{From: "merge", Outcome: "merged", To: "done"},
			{From: "merge", Outcome: "conflict", To: "implement"},
		},
	}
}

func planningFlowInput(defIDs map[string]string, codingID string) FlowInput {
	return FlowInput{
		Name:             PlanningFlowName,
		Description:      "Create a human-approved follow-on task graph, including narrower planning where needed.",
		StartNode:        "write-plan",
		TransitionBudget: DefaultFlowTransitionBudget,
		Nodes: []FlowNodeInput{
			{Key: "write-plan", Name: "Write task plan", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: defIDs["task-planner"], Workspace: WorkspaceBase, Artifact: ArtifactTaskSet}}},
			{Key: "review-plan", Name: "Review plan", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Instructions: "Review the proposed follow-on tasks and their selected workflows.", Outcomes: []string{"approved", "changes_requested", "rejected"}}}},
			{Key: "create-tasks", Name: "Create planned tasks", Kind: NodeMaterializeTaskSet, Config: FlowNodeConfig{MaterializeTaskSet: &MaterializeTaskSetNodeConfig{DefaultChildFlowID: codingID, AllowChildFlowOverride: true, MaxItems: 25}}},
			{Key: "done", Name: "Completed", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "rejected", Name: "Rejected", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionRejected}}},
		},
		Edges: []FlowEdgeInput{
			{From: "write-plan", Outcome: "completed", To: "review-plan"},
			{From: "review-plan", Outcome: "approved", To: "create-tasks"},
			{From: "review-plan", Outcome: "changes_requested", To: "write-plan"},
			{From: "review-plan", Outcome: "rejected", To: "rejected"},
			{From: "create-tasks", Outcome: "completed", To: "done"},
		},
	}
}

// featureRebaseFlowInput is the built-in flow a conflicted feature rebase runs
// as a system-created task: the agent rebases its ordinary task branch onto
// the project base, checks and the verifier prove the result, a human gates
// the publish, and the trusted finalize node rewrites the shared feature ref.
func featureRebaseFlowInput(defIDs map[string]string) FlowInput {
	return FlowInput{
		Name:             FeatureRebaseFlowName,
		Description:      "Rebase a feature branch onto the project base with agent conflict resolution.",
		StartNode:        "rebase",
		TransitionBudget: DefaultFlowTransitionBudget,
		Nodes: []FlowNodeInput{
			{Key: "rebase", Name: "Rebase feature branch", Kind: NodeAgent, Config: FlowNodeConfig{Agent: &AgentNodeConfig{AgentDefID: defIDs["rebase-author"], Workspace: WorkspaceChange, Artifact: ArtifactChange}}},
			{Key: "checks", Name: "Automated checks", Kind: NodeAutomatedChecks, Config: FlowNodeConfig{AutomatedChecks: &AutomatedChecksNodeConfig{}}},
			{Key: "verify", Name: "Verify rebase", Kind: NodeVerifyChange, Config: FlowNodeConfig{VerifyChange: &VerifyChangeNodeConfig{Agents: []ReviewAgentConfig{{AgentDefID: defIDs["rebase-verifier"]}}}}},
			{Key: "human-review", Name: "Human rebase review", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Instructions: "Review the rebased branch and choose whether to publish it to the feature branch.", Outcomes: []string{"approved", "changes_requested", "abandoned"}}}},
			{Key: "finalize", Name: "Finalize rebase", Kind: NodeFinalizeRebase, Config: FlowNodeConfig{FinalizeRebase: &FinalizeRebaseNodeConfig{}}},
			{Key: "done", Name: "Rebased", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "abandoned", Name: "Abandoned", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionAbandoned}}},
		},
		Edges: []FlowEdgeInput{
			{From: "rebase", Outcome: "completed", To: "checks"},
			{From: "checks", Outcome: "passed", To: "verify"},
			{From: "checks", Outcome: "failed", To: "rebase"},
			{From: "verify", Outcome: "passed", To: "human-review"},
			{From: "verify", Outcome: "changes_requested", To: "rebase"},
			{From: "human-review", Outcome: "approved", To: "finalize"},
			{From: "human-review", Outcome: "changes_requested", To: "rebase"},
			{From: "human-review", Outcome: "abandoned", To: "abandoned"},
			{From: "finalize", Outcome: "finalized", To: "done"},
			{From: "finalize", Outcome: "stale", To: "rebase"},
		},
	}
}
