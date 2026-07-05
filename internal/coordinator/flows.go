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
	flowskills "github.com/ClarifiedLabs/flow/skills"
)

var (
	ErrFlowNotFound  = errors.New("flow not found")
	ErrFlowNameTaken = errors.New("a flow with this name already exists")
	ErrFlowIsDefault = errors.New("flow is the project default; set another default before deleting it")
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

// Flow is an ordered work pipeline (phases) plus the agent review set that
// runs once the final phase readies a change. Flows are project-owned rows
// edited live; issues freeze a FlowSnapshot at schedule time.
type Flow struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	FixAgentDefID string            `json:"fix_agent_def_id,omitempty"`
	Builtin       bool              `json:"builtin"`
	Default       bool              `json:"default"`
	Phases        []FlowPhase       `json:"phases"`
	ReviewAgents  []FlowReviewAgent `json:"review_agents"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
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
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	FixAgentDefID string                 `json:"fix_agent_def_id"`
	Phases        []FlowPhaseInput       `json:"phases"`
	ReviewAgents  []FlowReviewAgentInput `json:"review_agents"`
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
	Required bool             `json:"required"`
	Agent    AgentDefSnapshot `json:"agent"`
}

// FlowSnapshot is the fully resolved flow an issue runs: ordered work phases
// with frozen agent defs, the review set, and the fix-cycle agent. It is
// stored as JSON on issue_flow_cursor at schedule time.
type FlowSnapshot struct {
	FlowID       string                    `json:"flow_id"`
	FlowName     string                    `json:"flow_name"`
	Phases       []FlowPhaseSnapshot       `json:"phases"`
	ReviewAgents []FlowReviewAgentSnapshot `json:"review_agents,omitempty"`
	FixAgent     *AgentDefSnapshot         `json:"fix_agent,omitempty"`
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
	db  *sql.DB
	now func() time.Time
}

func NewFlowService(database *sql.DB) *FlowService {
	return &FlowService{db: database, now: sqlitex.UTCNow}
}

func normalizeFlowInput(input FlowInput) (FlowInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return FlowInput{}, errors.New("flow name is required")
	}
	input.Description = strings.TrimSpace(input.Description)
	input.FixAgentDefID = strings.TrimSpace(input.FixAgentDefID)
	if len(input.Phases) == 0 {
		return FlowInput{}, errors.New("flow requires at least one phase")
	}
	seenPhaseNames := map[string]struct{}{}
	for i, phase := range input.Phases {
		phase.Name = strings.TrimSpace(phase.Name)
		phase.AgentDefID = strings.TrimSpace(phase.AgentDefID)
		if phase.Name == "" {
			return FlowInput{}, fmt.Errorf("phase %d name is required", i+1)
		}
		if _, dup := seenPhaseNames[phase.Name]; dup {
			return FlowInput{}, fmt.Errorf("duplicate phase name %q", phase.Name)
		}
		seenPhaseNames[phase.Name] = struct{}{}
		if phase.AgentDefID == "" {
			return FlowInput{}, fmt.Errorf("phase %q requires an agent definition", phase.Name)
		}
		switch phase.Gate {
		case FlowGateAuto, FlowGateHuman:
		case "":
			phase.Gate = FlowGateAuto
		default:
			return FlowInput{}, fmt.Errorf("phase %q has invalid gate %q", phase.Name, phase.Gate)
		}
		input.Phases[i] = phase
	}
	for i, agent := range input.ReviewAgents {
		agent.AgentDefID = strings.TrimSpace(agent.AgentDefID)
		if agent.AgentDefID == "" {
			return FlowInput{}, fmt.Errorf("review agent %d requires an agent definition", i+1)
		}
		switch agent.Role {
		case FlowReviewRoleReviewer, FlowReviewRoleVerifier:
		default:
			return FlowInput{}, fmt.Errorf("review agent %d has invalid role %q", i+1, agent.Role)
		}
		input.ReviewAgents[i] = agent
	}
	return input, nil
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
	_, err = tx.ExecContext(ctx, `
INSERT INTO flows (id, name, description, fix_agent_def_id, builtin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, input.Name, input.Description, sqlitex.NullableNonEmptyString(input.FixAgentDefID),
		boolToInt(builtin), now, now)
	if err != nil {
		if isUniqueViolation(err, "flows.name") {
			return Flow{}, ErrFlowNameTaken
		}
		return Flow{}, fmt.Errorf("insert flow: %w", err)
	}
	if err := insertFlowChildren(ctx, tx, id, input); err != nil {
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

	result, err := tx.ExecContext(ctx, `
UPDATE flows SET name = ?, description = ?, fix_agent_def_id = ?, updated_at = ?
WHERE id = ?`,
		input.Name, input.Description, sqlitex.NullableNonEmptyString(input.FixAgentDefID),
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

	// Phases and review agents are replace-all: the input is the complete
	// desired flow, not a patch.
	if _, err := tx.ExecContext(ctx, `DELETE FROM flow_phases WHERE flow_id = ?`, id); err != nil {
		return Flow{}, fmt.Errorf("clear flow phases: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM flow_review_agents WHERE flow_id = ?`, id); err != nil {
		return Flow{}, fmt.Errorf("clear flow review agents: %w", err)
	}
	if err := insertFlowChildren(ctx, tx, id, input); err != nil {
		return Flow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Flow{}, err
	}

	return s.Get(ctx, id)
}

func insertFlowChildren(ctx context.Context, tx *sqlitex.Tx, flowID string, input FlowInput) error {
	for position, phase := range input.Phases {
		phaseID, err := randomPrefixedID("fp")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO flow_phases (id, flow_id, position, name, agent_def_id, gate)
VALUES (?, ?, ?, ?, ?, ?)`,
			phaseID, flowID, position, phase.Name, phase.AgentDefID, string(phase.Gate)); err != nil {
			if isForeignKeyViolation(err) {
				return fmt.Errorf("phase %q references an unknown agent definition", phase.Name)
			}
			return fmt.Errorf("insert flow phase %q: %w", phase.Name, err)
		}
	}
	positions := map[FlowReviewRole]int{}
	for _, agent := range input.ReviewAgents {
		agentID, err := randomPrefixedID("fra")
		if err != nil {
			return err
		}
		required := true
		if agent.Required != nil {
			required = *agent.Required
		}
		position := positions[agent.Role]
		positions[agent.Role] = position + 1
		if _, err := tx.ExecContext(ctx, `
INSERT INTO flow_review_agents (id, flow_id, role, agent_def_id, position, required)
VALUES (?, ?, ?, ?, ?, ?)`,
			agentID, flowID, string(agent.Role), agent.AgentDefID, position, boolToInt(required)); err != nil {
			if isForeignKeyViolation(err) {
				return fmt.Errorf("review agent %d references an unknown agent definition", position+1)
			}
			return fmt.Errorf("insert flow review agent: %w", err)
		}
	}
	return nil
}

func (s *FlowService) Delete(ctx context.Context, id string) error {
	defaultID, err := s.DefaultFlowID(ctx)
	if err != nil {
		return err
	}
	if defaultID == id {
		return ErrFlowIsDefault
	}

	result, err := s.db.ExecContext(ctx, `DELETE FROM flows WHERE id = ?`, id)
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
	return nil
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
SELECT f.id, f.name, f.description, f.fix_agent_def_id, f.builtin, f.created_at, f.updated_at
FROM flows f WHERE `+where+` ORDER BY f.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list flows: %w", err)
	}
	defer rows.Close()

	flows := []Flow{}
	index := map[string]int{}
	for rows.Next() {
		var flow Flow
		var fixAgent sql.NullString
		var builtin int
		var createdAt, updatedAt string
		if err := rows.Scan(&flow.ID, &flow.Name, &flow.Description, &fixAgent, &builtin, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if fixAgent.Valid {
			flow.FixAgentDefID = fixAgent.String
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
		index[flow.ID] = len(flows)
		flows = append(flows, flow)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(flows) == 0 {
		return flows, nil
	}

	phaseRows, err := s.db.QueryContext(ctx, `
SELECT p.id, p.flow_id, p.position, p.name, p.agent_def_id, p.gate
FROM flow_phases p JOIN flows f ON f.id = p.flow_id
WHERE `+where+` ORDER BY p.flow_id, p.position`, args...)
	if err != nil {
		return nil, fmt.Errorf("list flow phases: %w", err)
	}
	defer phaseRows.Close()
	for phaseRows.Next() {
		var phase FlowPhase
		var flowID, gate string
		if err := phaseRows.Scan(&phase.ID, &flowID, &phase.Position, &phase.Name, &phase.AgentDefID, &gate); err != nil {
			return nil, err
		}
		phase.Gate = FlowGate(gate)
		if i, ok := index[flowID]; ok {
			flows[i].Phases = append(flows[i].Phases, phase)
		}
	}
	if err := phaseRows.Err(); err != nil {
		return nil, err
	}

	reviewRows, err := s.db.QueryContext(ctx, `
SELECT r.id, r.flow_id, r.role, r.agent_def_id, r.position, r.required
FROM flow_review_agents r JOIN flows f ON f.id = r.flow_id
WHERE `+where+` ORDER BY r.flow_id, r.role, r.position`, args...)
	if err != nil {
		return nil, fmt.Errorf("list flow review agents: %w", err)
	}
	defer reviewRows.Close()
	for reviewRows.Next() {
		var agent FlowReviewAgent
		var flowID, role string
		var required int
		if err := reviewRows.Scan(&agent.ID, &flowID, &role, &agent.AgentDefID, &agent.Position, &required); err != nil {
			return nil, err
		}
		agent.Role = FlowReviewRole(role)
		agent.Required = required != 0
		if i, ok := index[flowID]; ok {
			flows[i].ReviewAgents = append(flows[i].ReviewAgents, agent)
		}
	}
	if err := reviewRows.Err(); err != nil {
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
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO app_metadata (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		defaultFlowMetadataKey, id, sqlitex.FormatTime(s.now().UTC()))
	if err != nil {
		return fmt.Errorf("set default flow: %w", err)
	}
	return nil
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
	if len(flow.Phases) == 0 {
		return FlowSnapshot{}, fmt.Errorf("flow %q has no phases", flow.Name)
	}

	defs := NewAgentDefService(s.db)
	snapshotAgent := func(agentDefID string) (AgentDefSnapshot, error) {
		def, err := defs.Get(ctx, agentDefID)
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

	snapshot := FlowSnapshot{FlowID: flow.ID, FlowName: flow.Name}
	for _, phase := range flow.Phases {
		agent, err := snapshotAgent(phase.AgentDefID)
		if err != nil {
			return FlowSnapshot{}, err
		}
		snapshot.Phases = append(snapshot.Phases, FlowPhaseSnapshot{
			Name:  phase.Name,
			Gate:  phase.Gate,
			Agent: agent,
		})
	}
	for _, reviewAgent := range flow.ReviewAgents {
		agent, err := snapshotAgent(reviewAgent.AgentDefID)
		if err != nil {
			return FlowSnapshot{}, err
		}
		snapshot.ReviewAgents = append(snapshot.ReviewAgents, FlowReviewAgentSnapshot{
			Role:     reviewAgent.Role,
			Required: reviewAgent.Required,
			Agent:    agent,
		})
	}
	if flow.FixAgentDefID != "" {
		agent, err := snapshotAgent(flow.FixAgentDefID)
		if err != nil {
			return FlowSnapshot{}, err
		}
		snapshot.FixAgent = &agent
	}

	return snapshot, nil
}

// SeedDefaults populates a fresh project with the built-in agent defs and
// flows so it works with zero configuration: planner/author/reviewer/verifier
// defs (prompts from the embedded skills), a "direct" flow (implement only)
// and a "planned" flow (human-gated plan, then implement). Idempotent: any
// existing agent def or flow row disables seeding entirely.
func (s *FlowService) SeedDefaults(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM agent_defs) + (SELECT COUNT(*) FROM flows)`).Scan(&count); err != nil {
		return fmt.Errorf("check seed state: %w", err)
	}
	if count > 0 {
		return nil
	}

	defs := NewAgentDefService(s.db)
	harness := flowharness.DefaultAgentName()
	defIDs := map[string]string{}
	for _, seed := range []struct {
		name  string
		skill string
	}{
		{"planner", flowskills.PlannerSkill},
		{"author", flowskills.AuthorSkill},
		{"reviewer", flowskills.ReviewerSkill},
		{"verifier", flowskills.VerifierSkill},
	} {
		prompt, err := flowskills.Instructions(seed.skill)
		if err != nil {
			return fmt.Errorf("seed agent def %s: %w", seed.name, err)
		}
		def, err := defs.create(ctx, AgentDefInput{Name: seed.name, Harness: harness, Prompt: prompt}, true)
		if err != nil {
			return fmt.Errorf("seed agent def %s: %w", seed.name, err)
		}
		defIDs[seed.name] = def.ID
	}

	reviewSet := []FlowReviewAgentInput{
		{Role: FlowReviewRoleReviewer, AgentDefID: defIDs["reviewer"]},
		{Role: FlowReviewRoleVerifier, AgentDefID: defIDs["verifier"]},
	}
	direct, err := s.create(ctx, FlowInput{
		Name:          "direct",
		Description:   "Implement immediately; agent review before merge.",
		FixAgentDefID: defIDs["author"],
		Phases: []FlowPhaseInput{
			{Name: "implement", AgentDefID: defIDs["author"], Gate: FlowGateAuto},
		},
		ReviewAgents: reviewSet,
	}, true)
	if err != nil {
		return fmt.Errorf("seed direct flow: %w", err)
	}
	if _, err := s.create(ctx, FlowInput{
		Name:          "planned",
		Description:   "Plan first with human approval, then implement.",
		FixAgentDefID: defIDs["author"],
		Phases: []FlowPhaseInput{
			{Name: "plan", AgentDefID: defIDs["planner"], Gate: FlowGateHuman},
			{Name: "implement", AgentDefID: defIDs["author"], Gate: FlowGateAuto},
		},
		ReviewAgents: reviewSet,
	}, true); err != nil {
		return fmt.Errorf("seed planned flow: %w", err)
	}

	return s.SetDefaultFlow(ctx, direct.ID)
}
