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
	ErrAgentDefNotFound  = errors.New("agent definition not found")
	ErrAgentDefNameTaken = errors.New("an agent definition with this name already exists")
	ErrAgentDefInUse     = errors.New("agent definition is referenced by a flow")
)

// AgentDef is a reusable, project-owned agent configuration: which harness to
// launch, the model/reasoning-effort selection, and the role-instruction
// prompt. Flow phases and review sets reference agent defs by id; issues in
// flight carry frozen AgentDefSnapshot copies instead.
type AgentDef struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Harness         string    `json:"harness"`
	Model           string    `json:"model,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`
	Builtin         bool      `json:"builtin"`
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

// AgentDefService manages the per-project agent definition catalog.
type AgentDefService struct {
	db  *sql.DB
	now func() time.Time
}

func NewAgentDefService(database *sql.DB) *AgentDefService {
	return &AgentDefService{db: database, now: sqlitex.UTCNow}
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
		return AgentDef{}, ErrAgentDefNotFound
	}

	return s.Get(ctx, id)
}

func (s *AgentDefService) Delete(ctx context.Context, id string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT config_json FROM flow_nodes`)
	if err != nil {
		return fmt.Errorf("inspect workflow agent references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan workflow agent reference: %w", err)
		}
		config, err := decodeNodeConfig(raw)
		if err != nil {
			return err
		}
		if flowNodeConfigUsesAgent(config, id) {
			return ErrAgentDefInUse
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate workflow agent references: %w", err)
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
	return s.getWhere(ctx, "id = ?", id)
}

func (s *AgentDefService) GetByName(ctx context.Context, name string) (AgentDef, error) {
	return s.getWhere(ctx, "name = ?", strings.TrimSpace(name))
}

func (s *AgentDefService) getWhere(ctx context.Context, where string, arg any) (AgentDef, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, harness, model, reasoning_effort, prompt, builtin, created_at, updated_at
FROM agent_defs WHERE `+where, arg)
	def, err := scanAgentDef(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentDef{}, ErrAgentDefNotFound
	}
	return def, err
}

func (s *AgentDefService) List(ctx context.Context) ([]AgentDef, error) {
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
