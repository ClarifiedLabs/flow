package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	flowskills "github.com/ClarifiedLabs/flow/skills"
)

var (
	ErrAgentDefNotFound  = errors.New("agent definition not found")
	ErrAgentDefNameTaken = errors.New("an agent definition with this name already exists")
	ErrAgentDefInUse     = errors.New("agent definition is referenced by a flow")
)

// AgentDef is a reusable agent configuration: which harness to launch, the
// model/reasoning-effort selection, and the role-instruction prompt. Project
// catalogs include coordinator-global definitions unless a project definition
// with the same name overrides one. Flow nodes reference agent defs by id;
// tasks in flight carry frozen AgentDefSnapshot copies instead.
type AgentDef struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Harness         string    `json:"harness"`
	Model           string    `json:"model,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`
	Builtin         bool      `json:"builtin"`
	Inherited       bool      `json:"inherited,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ModelSelectionArgs renders the def's model/effort choice as harness argv
// tokens (empty when neither is set).
func (d AgentDef) ModelSelectionArgs() ([]string, error) {
	return flowharness.SerializeModelSelection(d.Harness, d.Model, d.ReasoningEffort)
}

type AgentDefInput struct {
	Name            string `json:"name"`
	Harness         string `json:"harness"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	Prompt          string `json:"prompt"`
}

// AgentDefService manages an agent definition catalog. A project service may
// have an inherited coordinator-global catalog; local definitions shadow
// inherited definitions by name. Global services omit project flow-reference
// checks because those references live in separate project databases and are
// checked by the registry before deletion.
type AgentDefService struct {
	db           *sql.DB
	inherited    *AgentDefService
	projectOwned bool
	now          func() time.Time
}

func NewAgentDefService(database *sql.DB) *AgentDefService {
	return &AgentDefService{db: database, projectOwned: true, now: sqlitex.UTCNow}
}

func NewInheritedAgentDefService(database *sql.DB, inherited *AgentDefService) *AgentDefService {
	return &AgentDefService{db: database, inherited: inherited, projectOwned: true, now: sqlitex.UTCNow}
}

func NewGlobalAgentDefService(database *sql.DB) *AgentDefService {
	return &AgentDefService{db: database, now: sqlitex.UTCNow}
}

type defaultAgentDefSeed struct {
	name  string
	skill string
	focus string
}

func defaultAgentDefSeeds() []defaultAgentDefSeed {
	return []defaultAgentDefSeed{
		{name: "task-planner", skill: flowskills.TaskPlannerSkill},
		{name: "author", skill: flowskills.AuthorSkill},
		{name: "code-reviewer", skill: flowskills.ReviewerSkill},
		{name: "security-reviewer", skill: flowskills.ReviewerSkill, focus: "Security focus: prioritize trust boundaries, authorization, input validation, secret handling, injection risks, and exploitable failure modes."},
		{name: "verifier", skill: flowskills.VerifierSkill},
	}
}

// SeedDefaults ensures the coordinator-global catalog contains the built-in
// definitions referenced by newly seeded project flows. Existing same-name
// global definitions are preserved so operators can customize a default role.
func (s *AgentDefService) SeedDefaults(ctx context.Context) error {
	if s.projectOwned {
		return errors.New("default agent definitions can only be seeded globally")
	}

	harness := flowharness.DefaultAgentName()
	for _, seed := range defaultAgentDefSeeds() {
		if _, err := s.GetByName(ctx, seed.name); err == nil {
			continue
		} else if !errors.Is(err, ErrAgentDefNotFound) {
			return fmt.Errorf("look up default agent def %s: %w", seed.name, err)
		}

		prompt, err := flowskills.Instructions(seed.skill)
		if err != nil {
			return fmt.Errorf("seed default agent def %s: %w", seed.name, err)
		}
		if seed.focus != "" {
			prompt += "\n\n" + seed.focus
		}
		if _, err := s.create(ctx, AgentDefInput{Name: seed.name, Harness: harness, Prompt: prompt}, true); err != nil {
			return fmt.Errorf("seed default agent def %s: %w", seed.name, err)
		}
	}
	return nil
}

func normalizeAgentDefInput(input AgentDefInput) (AgentDefInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return AgentDefInput{}, errors.New("agent definition name is required")
	}
	input.Harness = flowharness.NormalizeName(input.Harness)
	input.Model = strings.TrimSpace(input.Model)
	input.ReasoningEffort = strings.TrimSpace(input.ReasoningEffort)
	input.Prompt = strings.TrimSpace(input.Prompt)
	// SerializeModelSelection validates the harness kind and the model/effort
	// tokens; the same call renders the job argv later, so what saves here is
	// exactly what launches.
	if _, err := flowharness.SerializeModelSelection(input.Harness, input.Model, input.ReasoningEffort); err != nil {
		return AgentDefInput{}, err
	}
	return input, nil
}

func (s *AgentDefService) Create(ctx context.Context, input AgentDefInput) (AgentDef, error) {
	return s.create(ctx, input, false)
}

func (s *AgentDefService) create(ctx context.Context, input AgentDefInput, builtin bool) (AgentDef, error) {
	input, err := normalizeAgentDefInput(input)
	if err != nil {
		return AgentDef{}, err
	}
	id, err := randomPrefixedID("ad")
	if err != nil {
		return AgentDef{}, err
	}

	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO agent_defs (id, name, harness, model, reasoning_effort, prompt, builtin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, input.Name, input.Harness, input.Model, input.ReasoningEffort, input.Prompt,
		boolToInt(builtin), sqlitex.FormatTime(now), sqlitex.FormatTime(now))
	if err != nil {
		if isUniqueViolation(err, "agent_defs.name") {
			return AgentDef{}, ErrAgentDefNameTaken
		}
		return AgentDef{}, fmt.Errorf("insert agent def: %w", err)
	}

	return s.Get(ctx, id)
}

func (s *AgentDefService) Update(ctx context.Context, id string, input AgentDefInput) (AgentDef, error) {
	id = strings.TrimSpace(id)
	input, err := normalizeAgentDefInput(input)
	if err != nil {
		return AgentDef{}, err
	}

	result, err := s.db.ExecContext(ctx, `
UPDATE agent_defs
SET name = ?, harness = ?, model = ?, reasoning_effort = ?, prompt = ?, updated_at = ?
WHERE id = ?`,
		input.Name, input.Harness, input.Model, input.ReasoningEffort, input.Prompt,
		sqlitex.FormatTime(s.now().UTC()), id)
	if err != nil {
		if isUniqueViolation(err, "agent_defs.name") {
			return AgentDef{}, ErrAgentDefNameTaken
		}
		return AgentDef{}, fmt.Errorf("update agent def: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return AgentDef{}, err
	}
	if affected == 0 {
		// Editing an inherited row from a project's effective catalog creates a
		// local override. Keeping the name stable makes the override relationship
		// explicit and ensures flows that reference the global id resolve it.
		if s.inherited == nil {
			return AgentDef{}, ErrAgentDefNotFound
		}
		inherited, inheritedErr := s.inherited.Get(ctx, id)
		if inheritedErr != nil {
			return AgentDef{}, inheritedErr
		}
		if input.Name != inherited.Name {
			return AgentDef{}, errors.New("an inherited agent definition override must keep the inherited name")
		}
		return s.Create(ctx, input)
	}

	return s.getLocal(ctx, "id = ?", id)
}

func (s *AgentDefService) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if _, err := s.getLocal(ctx, "id = ?", id); err != nil {
		return err
	}
	if s.projectOwned {
		inUse, err := s.IsReferenced(ctx, id)
		if err != nil {
			return err
		}
		if inUse {
			return ErrAgentDefInUse
		}
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM agent_defs WHERE id = ?`, id)
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrAgentDefInUse
		}
		return fmt.Errorf("delete agent def: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrAgentDefNotFound
	}
	return nil
}

// IsReferenced reports whether this project's flows directly reference an
// agent definition id. The registry uses it across all projects before deleting
// a coordinator-global definition.
func (s *AgentDefService) IsReferenced(ctx context.Context, id string) (bool, error) {
	if !s.projectOwned {
		return false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT config_json FROM flow_nodes`)
	if err != nil {
		return false, fmt.Errorf("inspect workflow agent references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, fmt.Errorf("scan workflow agent reference: %w", err)
		}
		config, err := decodeNodeConfig(raw)
		if err != nil {
			return false, err
		}
		if flowNodeConfigUsesAgent(config, id) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate workflow agent references: %w", err)
	}
	return false, nil
}

func flowNodeConfigUsesAgent(config FlowNodeConfig, agentDefID string) bool {
	if config.Agent != nil && config.Agent.AgentDefID == agentDefID {
		return true
	}
	if config.ChangeReview != nil {
		for _, agent := range config.ChangeReview.Agents {
			if agent.AgentDefID == agentDefID {
				return true
			}
		}
	}
	if config.VerifyChange != nil {
		for _, agent := range config.VerifyChange.Agents {
			if agent.AgentDefID == agentDefID {
				return true
			}
		}
	}
	return false
}

func (s *AgentDefService) Get(ctx context.Context, id string) (AgentDef, error) {
	def, err := s.getLocal(ctx, "id = ?", strings.TrimSpace(id))
	if err == nil || !errors.Is(err, ErrAgentDefNotFound) || s.inherited == nil {
		return def, err
	}
	def, err = s.inherited.Get(ctx, id)
	if err == nil {
		def.Inherited = true
	}
	return def, err
}

func (s *AgentDefService) GetByName(ctx context.Context, name string) (AgentDef, error) {
	name = strings.TrimSpace(name)
	def, err := s.getLocal(ctx, "name = ?", name)
	if err == nil || !errors.Is(err, ErrAgentDefNotFound) || s.inherited == nil {
		return def, err
	}
	def, err = s.inherited.GetByName(ctx, name)
	if err == nil {
		def.Inherited = true
	}
	return def, err
}

func (s *AgentDefService) getInheritedByName(ctx context.Context, name string) (AgentDef, error) {
	if s.inherited == nil {
		return AgentDef{}, ErrAgentDefNotFound
	}
	def, err := s.inherited.GetByName(ctx, strings.TrimSpace(name))
	if err == nil {
		def.Inherited = true
	}
	return def, err
}

// Resolve returns the effective definition for a stored flow reference. Local
// ids resolve directly. A global id first resolves its global name, then applies
// a same-name local override when present.
func (s *AgentDefService) Resolve(ctx context.Context, id string) (AgentDef, error) {
	return s.resolveWith(ctx, s.db, strings.TrimSpace(id))
}

type agentDefQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *AgentDefService) resolveWith(ctx context.Context, local agentDefQueryer, id string) (AgentDef, error) {
	def, err := getAgentDefFrom(ctx, local, "id = ?", id)
	if err == nil || !errors.Is(err, ErrAgentDefNotFound) || s.inherited == nil {
		return def, err
	}
	inherited, err := s.inherited.Resolve(ctx, id)
	if err != nil {
		return AgentDef{}, err
	}
	override, overrideErr := getAgentDefFrom(ctx, local, "name = ?", inherited.Name)
	if overrideErr == nil {
		return override, nil
	}
	if !errors.Is(overrideErr, ErrAgentDefNotFound) {
		return AgentDef{}, overrideErr
	}
	inherited.Inherited = true
	return inherited, nil
}

func (s *AgentDefService) getLocal(ctx context.Context, where string, arg any) (AgentDef, error) {
	return getAgentDefFrom(ctx, s.db, where, arg)
}

func getAgentDefFrom(ctx context.Context, queryer agentDefQueryer, where string, arg any) (AgentDef, error) {
	row := queryer.QueryRowContext(ctx, `
SELECT id, name, harness, model, reasoning_effort, prompt, builtin, created_at, updated_at
FROM agent_defs WHERE `+where, arg)
	def, err := scanAgentDef(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentDef{}, ErrAgentDefNotFound
	}
	return def, err
}

func (s *AgentDefService) List(ctx context.Context) ([]AgentDef, error) {
	defs, err := s.listLocal(ctx)
	if err != nil || s.inherited == nil {
		return defs, err
	}
	inherited, err := s.inherited.List(ctx)
	if err != nil {
		return nil, err
	}
	localNames := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		localNames[def.Name] = struct{}{}
	}
	for _, def := range inherited {
		if _, overridden := localNames[def.Name]; overridden {
			continue
		}
		def.Inherited = true
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].Name != defs[j].Name {
			return defs[i].Name < defs[j].Name
		}
		return defs[i].ID < defs[j].ID
	})
	return defs, nil
}

func (s *AgentDefService) listLocal(ctx context.Context) ([]AgentDef, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, harness, model, reasoning_effort, prompt, builtin, created_at, updated_at
FROM agent_defs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list agent defs: %w", err)
	}
	defer rows.Close()

	defs := []AgentDef{}
	for rows.Next() {
		def, err := scanAgentDef(rows)
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	return defs, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgentDef(row rowScanner) (AgentDef, error) {
	var def AgentDef
	var builtin int
	var createdAt, updatedAt string
	if err := row.Scan(&def.ID, &def.Name, &def.Harness, &def.Model, &def.ReasoningEffort,
		&def.Prompt, &builtin, &createdAt, &updatedAt); err != nil {
		return AgentDef{}, err
	}
	def.Builtin = builtin != 0
	var err error
	if def.CreatedAt, err = sqlitex.ParseTime(createdAt); err != nil {
		return AgentDef{}, fmt.Errorf("parse agent def created_at: %w", err)
	}
	if def.UpdatedAt, err = sqlitex.ParseTime(updatedAt); err != nil {
		return AgentDef{}, fmt.Errorf("parse agent def updated_at: %w", err)
	}
	return def, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func isUniqueViolation(err error, column string) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: "+column)
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
