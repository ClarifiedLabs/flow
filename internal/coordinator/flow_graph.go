package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const (
	DefaultFlowTransitionBudget = 50
	MaxFlowTransitionBudget     = 500
	MaxFlowNodes                = 50
	MaxFlowEdges                = 200
)

var flowNodeKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

type WorkspaceMode string

const (
	WorkspaceBase   WorkspaceMode = "base"
	WorkspaceChange WorkspaceMode = "change"
)

type ArtifactKind string

const (
	ArtifactHandoff ArtifactKind = "handoff"
	ArtifactChange  ArtifactKind = "change"
	ArtifactTaskSet ArtifactKind = "task_set"
)

type NodeKind string

const (
	NodeAgent              NodeKind = "agent"
	NodeAutomatedChecks    NodeKind = "automated_checks"
	NodeChangeReview       NodeKind = "change_review"
	NodeHumanGate          NodeKind = "human_gate"
	NodeVerifyChange       NodeKind = "verify_change"
	NodeMaterializeTaskSet NodeKind = "materialize_task_set"
	NodeMergeChange        NodeKind = "merge_change"
	NodeFinalizeRebase     NodeKind = "finalize_rebase"
	NodeTerminal           NodeKind = "terminal"
)

type DoneResolution string

const (
	ResolutionCompleted DoneResolution = "completed"
	ResolutionMerged    DoneResolution = "merged"
	ResolutionRejected  DoneResolution = "rejected"
	ResolutionAbandoned DoneResolution = "abandoned"
	ResolutionCancelled DoneResolution = "cancelled"
	ResolutionFailed    DoneResolution = "failed"
)

// EvidenceType classifies one completion-evidence entry.
type EvidenceType string

const (
	EvidenceCommit  EvidenceType = "commit"
	EvidenceTest    EvidenceType = "test"
	EvidencePR      EvidenceType = "pr"
	EvidenceReview  EvidenceType = "review"
	EvidenceNote    EvidenceType = "note"
)

// Evidence is one typed artifact attesting a task's completion (a commit SHA,
// a test run, a PR URL, a review reference, or a free-text note).
type Evidence struct {
	Type  EvidenceType `json:"type"`
	Value string       `json:"value"`
}

// maxEvidenceEntries caps a task's evidence list.
const maxEvidenceEntries = 20

// validateEvidence enforces the type allowlist, non-empty values, and the
// list cap. It returns the normalized list (types lowercased, values trimmed).
func validateEvidence(evidence []Evidence) ([]Evidence, error) {
	if len(evidence) > maxEvidenceEntries {
		return nil, fmt.Errorf("evidence list is capped at %d entries", maxEvidenceEntries)
	}
	normalized := make([]Evidence, 0, len(evidence))
	for _, entry := range evidence {
		typeName := EvidenceType(strings.ToLower(strings.TrimSpace(string(entry.Type))))
		switch typeName {
		case EvidenceCommit, EvidenceTest, EvidencePR, EvidenceReview, EvidenceNote:
		default:
			return nil, fmt.Errorf("invalid evidence type %q: want commit|test|pr|review|note", entry.Type)
		}
		value := strings.TrimSpace(entry.Value)
		if value == "" {
			return nil, fmt.Errorf("evidence %q has an empty value", typeName)
		}
		normalized = append(normalized, Evidence{Type: typeName, Value: value})
	}
	return normalized, nil
}


type AgentNodeConfig struct {
	AgentDefID string        `json:"agent_def_id"`
	Workspace  WorkspaceMode `json:"workspace"`
	Artifact   ArtifactKind  `json:"artifact"`
}

type AutomatedChecksNodeConfig struct{}

type ReviewAgentConfig struct {
	AgentDefID string `json:"agent_def_id"`
	Blocking   *bool  `json:"blocking,omitempty"`
}

// UnmarshalJSON decodes a review agent from the current graph vocabulary only:
// {agent_def_id, blocking}. The removed `required` alias (and any other
// unknown field) is rejected rather than silently ignored. An omitted
// `blocking` defaults to blocking, the canonical live-config default.
func (c *ReviewAgentConfig) UnmarshalJSON(data []byte) error {
	var decoded struct {
		AgentDefID string          `json:"agent_def_id"`
		Blocking   json.RawMessage `json:"blocking"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("review agent must contain exactly one JSON value")
		}
		return err
	}

	c.AgentDefID = decoded.AgentDefID
	if len(decoded.Blocking) == 0 {
		blocking := true
		c.Blocking = &blocking
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(decoded.Blocking), []byte("null")) {
		return errors.New("review agent blocking must be a boolean, not null")
	}
	var blocking bool
	if err := json.Unmarshal(decoded.Blocking, &blocking); err != nil {
		return fmt.Errorf("review agent blocking must be a boolean: %w", err)
	}
	c.Blocking = &blocking
	return nil
}

type ChangeReviewNodeConfig struct {
	Agents               []ReviewAgentConfig `json:"agents"`
	AggregatorAgentDefID string              `json:"aggregator_agent_def_id"`
}

type HumanGateNodeConfig struct {
	Instructions string   `json:"instructions,omitempty"`
	Outcomes     []string `json:"outcomes"`
	// TaskOptIn makes this gate conditional on the task's requires_human_review
	// setting. SkipOutcome is the edge followed when the setting is disabled.
	TaskOptIn   bool   `json:"task_opt_in,omitempty"`
	SkipOutcome string `json:"skip_outcome,omitempty"`
}

type VerifyChangeNodeConfig struct {
	Agents []ReviewAgentConfig `json:"agents"`
}

type MaterializeTaskSetNodeConfig struct {
	DefaultChildFlowID     string `json:"default_child_flow_id"`
	AllowChildFlowOverride bool   `json:"allow_child_flow_override"`
	MaxItems               int    `json:"max_items"`
}

type MergeChangeNodeConfig struct{}

// FinalizeRebaseNodeConfig is empty: the trusted handler publishes the rebase
// task's change head to the task's feature ref, guarded by a compare-and-swap
// on the tip the running feature_rebases row recorded.
type FinalizeRebaseNodeConfig struct{}

type TerminalNodeConfig struct {
	Resolution DoneResolution `json:"resolution"`
}

// FlowNodeConfig is a strict discriminated union. Exactly one branch matching
// the containing node's kind must be present.
type FlowNodeConfig struct {
	Agent              *AgentNodeConfig              `json:"agent,omitempty"`
	AutomatedChecks    *AutomatedChecksNodeConfig    `json:"automated_checks,omitempty"`
	ChangeReview       *ChangeReviewNodeConfig       `json:"change_review,omitempty"`
	HumanGate          *HumanGateNodeConfig          `json:"human_gate,omitempty"`
	VerifyChange       *VerifyChangeNodeConfig       `json:"verify_change,omitempty"`
	MaterializeTaskSet *MaterializeTaskSetNodeConfig `json:"materialize_task_set,omitempty"`
	MergeChange        *MergeChangeNodeConfig        `json:"merge_change,omitempty"`
	FinalizeRebase     *FinalizeRebaseNodeConfig     `json:"finalize_rebase,omitempty"`
	Terminal           *TerminalNodeConfig           `json:"terminal,omitempty"`
}

type FlowNode struct {
	ID       string         `json:"id"`
	Key      string         `json:"key"`
	Name     string         `json:"name"`
	Kind     NodeKind       `json:"kind"`
	Position int            `json:"position"`
	Config   FlowNodeConfig `json:"config"`
}

type FlowNodeInput struct {
	Key    string         `json:"key"`
	Name   string         `json:"name"`
	Kind   NodeKind       `json:"kind"`
	Config FlowNodeConfig `json:"config"`
}

type FlowEdge struct {
	From    string `json:"from"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

type FlowEdgeInput = FlowEdge

type SnapshotReviewAgent struct {
	Blocking bool             `json:"blocking"`
	Agent    AgentDefSnapshot `json:"agent"`
}

// UnmarshalJSON accepts only the current frozen review-agent shape. Unlike a
// live graph configuration, a snapshot must carry an explicit blocking value:
// silently treating a persisted legacy `required`-only value as false would
// change a blocking reviewer into an advisory one when a run is resumed.
func (c *SnapshotReviewAgent) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Blocking *bool             `json:"blocking"`
		Agent    *AgentDefSnapshot `json:"agent"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("snapshot review agent must contain exactly one JSON value")
		}
		return err
	}
	if decoded.Blocking == nil {
		return errors.New("snapshot review agent blocking is required")
	}
	if decoded.Agent == nil {
		return errors.New("snapshot review agent is required")
	}
	c.Blocking = *decoded.Blocking
	c.Agent = *decoded.Agent
	return nil
}

// ReviewAggregationBlocksApproval reports whether any discovery reviewer may
// contribute a blocking finding to the final aggregate decision.
func ReviewAggregationBlocksApproval(agents []SnapshotReviewAgent) bool {
	for _, agent := range agents {
		if agent.Blocking {
			return true
		}
	}
	return false
}

// Snapshots always serialize an explicit Boolean blocking. A snapshot that
// predates the field (only the removed `required` alias) fails strict decode on
// load, so no alias decoding or silent advisory fallback remains.

type FlowNodeSnapshotConfig struct {
	Agent              *AgentNodeSnapshotConfig        `json:"agent,omitempty"`
	AutomatedChecks    *AutomatedChecksNodeConfig      `json:"automated_checks,omitempty"`
	ChangeReview       *ChangeReviewNodeSnapshotConfig `json:"change_review,omitempty"`
	HumanGate          *HumanGateNodeConfig            `json:"human_gate,omitempty"`
	VerifyChange       *VerifyChangeNodeSnapshotConfig `json:"verify_change,omitempty"`
	MaterializeTaskSet *MaterializeTaskSetNodeConfig   `json:"materialize_task_set,omitempty"`
	MergeChange        *MergeChangeNodeConfig          `json:"merge_change,omitempty"`
	FinalizeRebase     *FinalizeRebaseNodeConfig       `json:"finalize_rebase,omitempty"`
	Terminal           *TerminalNodeConfig             `json:"terminal,omitempty"`
}

type AgentNodeSnapshotConfig struct {
	Agent     AgentDefSnapshot `json:"agent"`
	Workspace WorkspaceMode    `json:"workspace"`
	Artifact  ArtifactKind     `json:"artifact"`
}

type ChangeReviewNodeSnapshotConfig struct {
	Agents     []SnapshotReviewAgent `json:"agents"`
	Aggregator AgentDefSnapshot      `json:"aggregator"`
}

type VerifyChangeNodeSnapshotConfig struct {
	Agents []SnapshotReviewAgent `json:"agents"`
}

type FlowNodeSnapshot struct {
	Key    string                 `json:"key"`
	Name   string                 `json:"name"`
	Kind   NodeKind               `json:"kind"`
	Config FlowNodeSnapshotConfig `json:"config"`
}

func (s FlowSnapshot) Node(key string) (FlowNodeSnapshot, bool) {
	for _, node := range s.Nodes {
		if node.Key == key {
			return node, true
		}
	}
	return FlowNodeSnapshot{}, false
}

// NodeIndex reports the node's position in the frozen node list. The snapshot
// preserves authoring order, so the index is a display ordinal ("step 3 of 6")
// rather than a claim about progress — branching and cyclic graphs visit nodes
// out of slice order.
func (s FlowSnapshot) NodeIndex(key string) (int, bool) {
	for index, node := range s.Nodes {
		if node.Key == key {
			return index, true
		}
	}
	return 0, false
}

// TerminalForResolution returns the frozen terminal with the requested
// resolution. Operator merge release must select an explicit merged terminal;
// declaration order must never turn that operation into another resolution.
func (s FlowSnapshot) TerminalForResolution(resolution DoneResolution) (FlowNodeSnapshot, bool) {
	for _, node := range s.Nodes {
		if node.Kind == NodeTerminal && node.Config.Terminal != nil && node.Config.Terminal.Resolution == resolution {
			return node, true
		}
	}
	return FlowNodeSnapshot{}, false
}

func (s FlowSnapshot) Target(from, outcome string) (string, bool) {
	for _, edge := range s.Edges {
		if edge.From == from && edge.Outcome == outcome {
			return edge.To, true
		}
	}
	return "", false
}

// snapshotForTaskHumanReview freezes the task-level human-review choice into a
// run. Ordinary human gates remain mandatory; only gates explicitly marked
// task_opt_in are bypassed when requires_human_review is false. Bypassing follows
// each gate's configured skip outcome and removes branches that become
// unreachable, leaving a complete graph that can still be validated normally.
func snapshotForTaskHumanReview(snapshot FlowSnapshot, required bool) (FlowSnapshot, error) {
	if required {
		return snapshot, nil
	}

	bypass := make(map[string]string)
	for _, node := range snapshot.Nodes {
		if node.Kind != NodeHumanGate || node.Config.HumanGate == nil || !node.Config.HumanGate.TaskOptIn {
			continue
		}
		target, ok := snapshot.Target(node.Key, node.Config.HumanGate.SkipOutcome)
		if !ok {
			return FlowSnapshot{}, fmt.Errorf("task-opt-in human gate %q has no edge for skip outcome %q", node.Key, node.Config.HumanGate.SkipOutcome)
		}
		bypass[node.Key] = target
	}
	if len(bypass) == 0 {
		return snapshot, nil
	}

	resolveTarget := func(key string) (string, error) {
		seen := map[string]bool{}
		for bypass[key] != "" {
			if seen[key] {
				return "", fmt.Errorf("task-opt-in human gate skip outcomes form a cycle at %q", key)
			}
			seen[key] = true
			key = bypass[key]
		}
		return key, nil
	}

	start, err := resolveTarget(snapshot.StartNode)
	if err != nil {
		return FlowSnapshot{}, err
	}
	projected := snapshot
	projected.StartNode = start
	projected.Edges = nil
	for _, edge := range snapshot.Edges {
		if bypass[edge.From] != "" {
			continue
		}
		target, err := resolveTarget(edge.To)
		if err != nil {
			return FlowSnapshot{}, err
		}
		edge.To = target
		projected.Edges = append(projected.Edges, edge)
	}

	reachable := map[string]bool{projected.StartNode: true}
	for changed := true; changed; {
		changed = false
		for _, edge := range projected.Edges {
			if reachable[edge.From] && !reachable[edge.To] {
				reachable[edge.To] = true
				changed = true
			}
		}
	}
	projected.Nodes = nil
	for _, node := range snapshot.Nodes {
		if bypass[node.Key] == "" && reachable[node.Key] {
			projected.Nodes = append(projected.Nodes, node)
		}
	}
	edges := projected.Edges[:0]
	for _, edge := range projected.Edges {
		if reachable[edge.From] && reachable[edge.To] {
			edges = append(edges, edge)
		}
	}
	projected.Edges = edges
	if err := validateFlowSnapshot(projected); err != nil {
		return FlowSnapshot{}, fmt.Errorf("project task human-review choice: %w", err)
	}
	return projected, nil
}

// decodeFlowSnapshot accepts one current graph snapshot only. Workflow runs are
// durable authority, so permissive JSON decoding here would allow a stale
// ordered-flow field or an unknown nested configuration to silently alter a
// resumed run. Decode every level strictly and then re-run the graph-shape
// validation against the frozen node configuration.
func decodeFlowSnapshot(data []byte) (FlowSnapshot, error) {
	var snapshot FlowSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return FlowSnapshot{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return FlowSnapshot{}, errors.New("workflow snapshot must contain exactly one JSON value")
		}
		return FlowSnapshot{}, err
	}
	if err := validateFlowSnapshot(snapshot); err != nil {
		return FlowSnapshot{}, err
	}
	return snapshot, nil
}

// validateFlowSnapshot translates frozen agent copies to their graph contracts
// and uses the same structural validation as a live flow. It intentionally does
// not resolve any live agent definitions: a snapshot must remain independent of
// later catalog edits.
func validateFlowSnapshot(snapshot FlowSnapshot) error {
	if err := requireCanonicalSnapshotValue("flow_id", snapshot.FlowID); err != nil {
		return err
	}
	if err := requireCanonicalSnapshotValue("flow_name", snapshot.FlowName); err != nil {
		return err
	}
	if snapshot.TransitionBudget < 1 || snapshot.TransitionBudget > MaxFlowTransitionBudget {
		return fmt.Errorf("snapshot transition budget must be between 1 and %d", MaxFlowTransitionBudget)
	}
	input := FlowInput{
		StartNode:        snapshot.StartNode,
		TransitionBudget: snapshot.TransitionBudget,
		Edges:            append([]FlowEdgeInput(nil), snapshot.Edges...),
	}
	for _, node := range snapshot.Nodes {
		config, err := snapshotNodeConfigToInput(node.Config)
		if err != nil {
			return fmt.Errorf("snapshot node %q: %w", node.Key, err)
		}
		input.Nodes = append(input.Nodes, FlowNodeInput{
			Key: node.Key, Name: node.Name, Kind: node.Kind, Config: config,
		})
	}
	normalized, err := normalizeGraphInput(input)
	if err != nil {
		return fmt.Errorf("invalid workflow snapshot graph: %w", err)
	}
	// A frozen snapshot is durable authority, not a permissive input format.
	// Accepting values that only become valid after trimming/defaulting would
	// leave the persisted snapshot different from the graph we validated (and
	// can make resumed routing depend on unvalidated raw keys). Snapshot writers
	// already persist canonical graph values, so reject any noncanonical shape.
	encoded, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode workflow snapshot graph for validation: %w", err)
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("encode normalized workflow snapshot graph: %w", err)
	}
	if !bytes.Equal(encoded, canonical) {
		return errors.New("workflow snapshot graph must use canonical values")
	}
	return nil
}

// requireCanonicalSnapshotValue rejects omitted and whitespace-normalizable
// persisted identifiers. Snapshot writers always store the canonical value, and
// accepting a different raw value would validate one graph while executing
// another.
func requireCanonicalSnapshotValue(field string, value string) error {
	canonical := strings.TrimSpace(value)
	if canonical == "" {
		return fmt.Errorf("workflow snapshot %s is required", field)
	}
	if value != canonical {
		return fmt.Errorf("workflow snapshot %s must use canonical values", field)
	}
	return nil
}

// frozenAgentID validates the complete self-contained agent contract carried by
// a snapshot. It intentionally does not resolve the ID through the current
// agent catalog: a persisted run remains executable after that catalog entry is
// changed or removed.
func frozenAgentID(agent AgentDefSnapshot) (string, error) {
	if err := requireCanonicalSnapshotValue("agent id", agent.ID); err != nil {
		return "", err
	}
	normalized, err := normalizeAgentDefInput(AgentDefInput{
		Name:            agent.Name,
		Harness:         agent.Harness,
		Model:           agent.Model,
		ReasoningEffort: agent.ReasoningEffort,
		Prompt:          agent.Prompt,
	})
	if err != nil {
		return "", fmt.Errorf("invalid frozen agent %q: %w", agent.ID, err)
	}
	canonical := AgentDefSnapshot{
		ID:              agent.ID,
		Name:            normalized.Name,
		Harness:         normalized.Harness,
		Model:           normalized.Model,
		ReasoningEffort: normalized.ReasoningEffort,
		Prompt:          normalized.Prompt,
	}
	if agent != canonical {
		return "", fmt.Errorf("frozen agent %q must use canonical values", agent.ID)
	}
	return agent.ID, nil
}

func snapshotNodeConfigToInput(config FlowNodeSnapshotConfig) (FlowNodeConfig, error) {
	out := FlowNodeConfig{}
	if config.Agent != nil {
		agentID, err := frozenAgentID(config.Agent.Agent)
		if err != nil {
			return FlowNodeConfig{}, err
		}
		out.Agent = &AgentNodeConfig{
			AgentDefID: agentID,
			Workspace:  config.Agent.Workspace,
			Artifact:   config.Agent.Artifact,
		}
	}
	if config.AutomatedChecks != nil {
		out.AutomatedChecks = &AutomatedChecksNodeConfig{}
	}
	if config.ChangeReview != nil {
		aggregatorID, err := frozenAgentID(config.ChangeReview.Aggregator)
		if err != nil {
			return FlowNodeConfig{}, fmt.Errorf("change-review aggregator: %w", err)
		}
		changeReview := &ChangeReviewNodeConfig{AggregatorAgentDefID: aggregatorID}
		for index, reviewAgent := range config.ChangeReview.Agents {
			agentID, err := frozenAgentID(reviewAgent.Agent)
			if err != nil {
				return FlowNodeConfig{}, fmt.Errorf("change-review agent %d: %w", index+1, err)
			}
			blocking := reviewAgent.Blocking
			changeReview.Agents = append(changeReview.Agents, ReviewAgentConfig{
				AgentDefID: agentID,
				Blocking:   &blocking,
			})
		}
		out.ChangeReview = changeReview
	}
	if config.HumanGate != nil {
		humanGate := *config.HumanGate
		humanGate.Outcomes = append([]string(nil), config.HumanGate.Outcomes...)
		out.HumanGate = &humanGate
	}
	if config.VerifyChange != nil {
		verifyChange := &VerifyChangeNodeConfig{}
		for index, reviewAgent := range config.VerifyChange.Agents {
			agentID, err := frozenAgentID(reviewAgent.Agent)
			if err != nil {
				return FlowNodeConfig{}, fmt.Errorf("verify-change agent %d: %w", index+1, err)
			}
			blocking := reviewAgent.Blocking
			verifyChange.Agents = append(verifyChange.Agents, ReviewAgentConfig{
				AgentDefID: agentID,
				Blocking:   &blocking,
			})
		}
		out.VerifyChange = verifyChange
	}
	if config.MaterializeTaskSet != nil {
		materialize := *config.MaterializeTaskSet
		out.MaterializeTaskSet = &materialize
	}
	if config.MergeChange != nil {
		out.MergeChange = &MergeChangeNodeConfig{}
	}
	if config.FinalizeRebase != nil {
		out.FinalizeRebase = &FinalizeRebaseNodeConfig{}
	}
	if config.Terminal != nil {
		terminal := *config.Terminal
		out.Terminal = &terminal
	}
	return out, nil
}

func normalizeGraphInput(input FlowInput) (FlowInput, error) {
	input.StartNode = strings.TrimSpace(input.StartNode)
	if input.TransitionBudget == 0 {
		input.TransitionBudget = DefaultFlowTransitionBudget
	}
	if input.TransitionBudget < 1 || input.TransitionBudget > MaxFlowTransitionBudget {
		return FlowInput{}, fmt.Errorf("transition budget must be between 1 and %d", MaxFlowTransitionBudget)
	}
	if len(input.Nodes) == 0 {
		return FlowInput{}, errors.New("flow requires at least one node")
	}
	if len(input.Nodes) > MaxFlowNodes {
		return FlowInput{}, fmt.Errorf("flow may contain at most %d nodes", MaxFlowNodes)
	}
	if len(input.Edges) > MaxFlowEdges {
		return FlowInput{}, fmt.Errorf("flow may contain at most %d edges", MaxFlowEdges)
	}

	nodes := make(map[string]FlowNodeInput, len(input.Nodes))
	declaredOutcomes := make(map[string]map[string]bool, len(input.Nodes))
	for i := range input.Nodes {
		node := input.Nodes[i]
		node.Key = strings.TrimSpace(node.Key)
		node.Name = strings.TrimSpace(node.Name)
		if !flowNodeKeyPattern.MatchString(node.Key) {
			return FlowInput{}, fmt.Errorf("node %d key %q must match %s", i+1, node.Key, flowNodeKeyPattern.String())
		}
		if node.Name == "" {
			return FlowInput{}, fmt.Errorf("node %q name is required", node.Key)
		}
		if _, exists := nodes[node.Key]; exists {
			return FlowInput{}, fmt.Errorf("duplicate node key %q", node.Key)
		}
		outcomes, normalizedConfig, err := normalizeNodeConfig(node.Key, node.Kind, node.Config)
		if err != nil {
			return FlowInput{}, err
		}
		node.Config = normalizedConfig
		input.Nodes[i] = node
		nodes[node.Key] = node
		declaredOutcomes[node.Key] = make(map[string]bool, len(outcomes))
		for _, outcome := range outcomes {
			declaredOutcomes[node.Key][outcome] = true
		}
	}
	if _, ok := nodes[input.StartNode]; !ok {
		return FlowInput{}, fmt.Errorf("start node %q does not exist", input.StartNode)
	}

	edgesByNode := make(map[string]map[string]string, len(nodes))
	for i := range input.Edges {
		edge := input.Edges[i]
		edge.From = strings.TrimSpace(edge.From)
		edge.Outcome = strings.TrimSpace(edge.Outcome)
		edge.To = strings.TrimSpace(edge.To)
		if _, ok := nodes[edge.From]; !ok {
			return FlowInput{}, fmt.Errorf("edge %d references unknown source node %q", i+1, edge.From)
		}
		if _, ok := nodes[edge.To]; !ok {
			return FlowInput{}, fmt.Errorf("edge %d references unknown target node %q", i+1, edge.To)
		}
		if !declaredOutcomes[edge.From][edge.Outcome] {
			return FlowInput{}, fmt.Errorf("node %q does not declare outcome %q", edge.From, edge.Outcome)
		}
		if edgesByNode[edge.From] == nil {
			edgesByNode[edge.From] = map[string]string{}
		}
		if _, duplicate := edgesByNode[edge.From][edge.Outcome]; duplicate {
			return FlowInput{}, fmt.Errorf("duplicate edge for node %q outcome %q", edge.From, edge.Outcome)
		}
		edgesByNode[edge.From][edge.Outcome] = edge.To
		input.Edges[i] = edge
	}
	for key, outcomes := range declaredOutcomes {
		for outcome := range outcomes {
			if edgesByNode[key][outcome] == "" {
				return FlowInput{}, fmt.Errorf("node %q outcome %q requires an edge", key, outcome)
			}
		}
	}
	if err := validateTaskOptInHumanGateSkipPaths(nodes, edgesByNode); err != nil {
		return FlowInput{}, err
	}
	if err := validateGraphReachability(input.StartNode, nodes, edgesByNode); err != nil {
		return FlowInput{}, err
	}
	if err := validateArtifactFlow(input.StartNode, nodes, edgesByNode); err != nil {
		return FlowInput{}, err
	}
	if err := validateTaskSetMaterializerPolicies(nodes, edgesByNode); err != nil {
		return FlowInput{}, err
	}
	if err := validateMergedTerminalPaths(input.StartNode, nodes, edgesByNode); err != nil {
		return FlowInput{}, err
	}
	return input, nil
}

func normalizeNodeConfig(key string, kind NodeKind, config FlowNodeConfig) ([]string, FlowNodeConfig, error) {
	branchCount := 0
	for _, present := range []bool{
		config.Agent != nil, config.AutomatedChecks != nil, config.ChangeReview != nil,
		config.HumanGate != nil, config.VerifyChange != nil, config.MaterializeTaskSet != nil,
		config.MergeChange != nil, config.FinalizeRebase != nil, config.Terminal != nil,
	} {
		if present {
			branchCount++
		}
	}
	if branchCount != 1 {
		return nil, FlowNodeConfig{}, fmt.Errorf("node %q config must contain exactly one branch", key)
	}
	switch kind {
	case NodeAgent:
		if config.Agent == nil {
			break
		}
		config.Agent.AgentDefID = strings.TrimSpace(config.Agent.AgentDefID)
		if config.Agent.AgentDefID == "" {
			return nil, FlowNodeConfig{}, fmt.Errorf("agent node %q requires an agent definition", key)
		}
		switch config.Agent.Workspace {
		case WorkspaceBase, WorkspaceChange:
		default:
			return nil, FlowNodeConfig{}, fmt.Errorf("agent node %q has invalid workspace %q", key, config.Agent.Workspace)
		}
		switch config.Agent.Artifact {
		case ArtifactHandoff, ArtifactChange, ArtifactTaskSet:
		default:
			return nil, FlowNodeConfig{}, fmt.Errorf("agent node %q has invalid artifact %q", key, config.Agent.Artifact)
		}
		if config.Agent.Workspace == WorkspaceBase && config.Agent.Artifact == ArtifactChange {
			return nil, FlowNodeConfig{}, fmt.Errorf("agent node %q cannot produce a change from a base workspace", key)
		}
		return []string{"completed"}, config, nil
	case NodeAutomatedChecks:
		if config.AutomatedChecks != nil {
			return []string{"passed", "failed"}, config, nil
		}
	case NodeChangeReview:
		if config.ChangeReview != nil {
			agents, err := normalizeReviewAgents(key, config.ChangeReview.Agents)
			if err != nil {
				return nil, FlowNodeConfig{}, err
			}
			config.ChangeReview.Agents = agents
			config.ChangeReview.AggregatorAgentDefID = strings.TrimSpace(config.ChangeReview.AggregatorAgentDefID)
			if config.ChangeReview.AggregatorAgentDefID == "" {
				return nil, FlowNodeConfig{}, fmt.Errorf("change review node %q requires an aggregator agent definition", key)
			}
			return []string{"approved", "changes_requested"}, config, nil
		}
	case NodeHumanGate:
		if config.HumanGate != nil {
			config.HumanGate.Instructions = strings.TrimSpace(config.HumanGate.Instructions)
			config.HumanGate.SkipOutcome = strings.TrimSpace(config.HumanGate.SkipOutcome)
			seen := map[string]bool{}
			var outcomes []string
			for _, raw := range config.HumanGate.Outcomes {
				outcome := strings.TrimSpace(raw)
				if !flowNodeKeyPattern.MatchString(outcome) {
					return nil, FlowNodeConfig{}, fmt.Errorf("human gate %q outcome %q must match %s", key, outcome, flowNodeKeyPattern.String())
				}
				if seen[outcome] {
					return nil, FlowNodeConfig{}, fmt.Errorf("human gate %q repeats outcome %q", key, outcome)
				}
				seen[outcome] = true
				outcomes = append(outcomes, outcome)
			}
			if len(outcomes) == 0 {
				return nil, FlowNodeConfig{}, fmt.Errorf("human gate %q requires at least one outcome", key)
			}
			if config.HumanGate.TaskOptIn {
				if !seen[config.HumanGate.SkipOutcome] {
					return nil, FlowNodeConfig{}, fmt.Errorf("task-opt-in human gate %q requires skip_outcome to name one of its outcomes", key)
				}
			} else if config.HumanGate.SkipOutcome != "" {
				return nil, FlowNodeConfig{}, fmt.Errorf("human gate %q cannot set skip_outcome unless task_opt_in is true", key)
			}
			config.HumanGate.Outcomes = outcomes
			return outcomes, config, nil
		}
	case NodeVerifyChange:
		if config.VerifyChange != nil {
			agents, err := normalizeReviewAgents(key, config.VerifyChange.Agents)
			if err != nil {
				return nil, FlowNodeConfig{}, err
			}
			config.VerifyChange.Agents = agents
			return []string{"passed", "changes_requested"}, config, nil
		}
	case NodeMaterializeTaskSet:
		if config.MaterializeTaskSet != nil {
			config.MaterializeTaskSet.DefaultChildFlowID = strings.TrimSpace(config.MaterializeTaskSet.DefaultChildFlowID)
			if config.MaterializeTaskSet.DefaultChildFlowID == "" {
				return nil, FlowNodeConfig{}, fmt.Errorf("task materialization node %q requires a default child flow", key)
			}
			if config.MaterializeTaskSet.MaxItems == 0 {
				config.MaterializeTaskSet.MaxItems = 25
			}
			if config.MaterializeTaskSet.MaxItems < 1 || config.MaterializeTaskSet.MaxItems > 50 {
				return nil, FlowNodeConfig{}, fmt.Errorf("task materialization node %q max_items must be between 1 and 50", key)
			}
			return []string{"completed"}, config, nil
		}
	case NodeMergeChange:
		if config.MergeChange != nil {
			return []string{"merged", "conflict"}, config, nil
		}
	case NodeFinalizeRebase:
		if config.FinalizeRebase != nil {
			return []string{"finalized", "stale"}, config, nil
		}
	case NodeTerminal:
		if config.Terminal != nil {
			if err := ValidateDoneResolution(config.Terminal.Resolution); err != nil {
				return nil, FlowNodeConfig{}, fmt.Errorf("terminal node %q: %w", key, err)
			}
			return nil, config, nil
		}
	default:
		return nil, FlowNodeConfig{}, fmt.Errorf("node %q has unknown kind %q", key, kind)
	}
	return nil, FlowNodeConfig{}, fmt.Errorf("node %q config does not match kind %q", key, kind)
}

func normalizeReviewAgents(key string, input []ReviewAgentConfig) ([]ReviewAgentConfig, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("node %q requires at least one agent", key)
	}
	seen := map[string]bool{}
	for i := range input {
		input[i].AgentDefID = strings.TrimSpace(input[i].AgentDefID)
		if input[i].AgentDefID == "" {
			return nil, fmt.Errorf("node %q agent %d requires an agent definition", key, i+1)
		}
		if seen[input[i].AgentDefID] {
			return nil, fmt.Errorf("node %q repeats agent definition %q", key, input[i].AgentDefID)
		}
		seen[input[i].AgentDefID] = true
		blocking := reviewAgentBlocking(input[i])
		input[i].Blocking = &blocking
	}
	return input, nil
}

func reviewAgentBlocking(agent ReviewAgentConfig) bool {
	if agent.Blocking != nil {
		return *agent.Blocking
	}
	return true
}

func validateTaskOptInHumanGateSkipPaths(nodes map[string]FlowNodeInput, edges map[string]map[string]string) error {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int, len(nodes))
	var visit func(string) error
	visit = func(key string) error {
		node, ok := nodes[key]
		if !ok || node.Kind != NodeHumanGate || node.Config.HumanGate == nil || !node.Config.HumanGate.TaskOptIn {
			return nil
		}
		switch state[key] {
		case visiting:
			return fmt.Errorf("task-opt-in human gate skip outcomes form a cycle at %q", key)
		case visited:
			return nil
		}
		state[key] = visiting
		target := edges[key][node.Config.HumanGate.SkipOutcome]
		if err := visit(target); err != nil {
			return err
		}
		state[key] = visited
		return nil
	}

	keys := make([]string, 0, len(nodes))
	for key, node := range nodes {
		if node.Kind == NodeHumanGate && node.Config.HumanGate != nil && node.Config.HumanGate.TaskOptIn {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if state[key] == unvisited {
			if err := visit(key); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateGraphReachability(start string, nodes map[string]FlowNodeInput, edges map[string]map[string]string) error {
	reachable := map[string]bool{}
	var visit func(string)
	visit = func(key string) {
		if reachable[key] {
			return
		}
		reachable[key] = true
		for _, target := range edges[key] {
			visit(target)
		}
	}
	visit(start)
	for key := range nodes {
		if !reachable[key] {
			return fmt.Errorf("node %q is unreachable from start node %q", key, start)
		}
	}

	reverse := map[string][]string{}
	var terminals []string
	for key, node := range nodes {
		if node.Kind == NodeTerminal {
			terminals = append(terminals, key)
		}
		for _, target := range edges[key] {
			reverse[target] = append(reverse[target], key)
		}
	}
	if len(terminals) == 0 {
		return errors.New("flow requires at least one terminal node")
	}
	canFinish := map[string]bool{}
	queue := append([]string(nil), terminals...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if canFinish[key] {
			continue
		}
		canFinish[key] = true
		queue = append(queue, reverse[key]...)
	}
	for key := range nodes {
		if !canFinish[key] {
			return fmt.Errorf("node %q cannot reach a terminal node", key)
		}
	}
	return nil
}

func validateArtifactFlow(start string, nodes map[string]FlowNodeInput, edges map[string]map[string]string) error {
	// A small fixed-point analysis tracks the artifact that can arrive at each
	// node. Agent nodes replace it; all other non-terminal nodes pass it through.
	type artifactSet map[ArtifactKind]bool
	incoming := map[string]artifactSet{start: {}}
	changed := true
	for changed {
		changed = false
		keys := make([]string, 0, len(nodes))
		for key := range nodes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			node := nodes[key]
			in, known := incoming[key]
			if !known {
				continue
			}
			if err := validateNodeInputArtifacts(node, in); err != nil {
				return err
			}
			out := in
			if node.Kind == NodeAgent {
				out = artifactSet{node.Config.Agent.Artifact: true}
			}
			for _, target := range edges[key] {
				if incoming[target] == nil {
					incoming[target] = artifactSet{}
				}
				for artifact := range out {
					if !incoming[target][artifact] {
						incoming[target][artifact] = true
						changed = true
					}
				}
			}
		}
	}
	return nil
}

func validateNodeInputArtifacts(node FlowNodeInput, incoming map[ArtifactKind]bool) error {
	require := func(kind ArtifactKind) error {
		if len(incoming) == 1 && incoming[kind] {
			return nil
		}
		return fmt.Errorf("node %q requires exactly one incoming %s artifact", node.Key, kind)
	}
	switch node.Kind {
	case NodeAutomatedChecks, NodeChangeReview, NodeVerifyChange, NodeMergeChange, NodeFinalizeRebase:
		return require(ArtifactChange)
	case NodeMaterializeTaskSet:
		return require(ArtifactTaskSet)
	}
	return nil
}

func validateMergedTerminalPaths(start string, nodes map[string]FlowNodeInput, edges map[string]map[string]string) error {
	const (
		pathUnmerged = 1 << iota
		pathMerged
	)
	incoming := map[string]int{start: pathUnmerged}
	changed := true
	for changed {
		changed = false
		for key, node := range nodes {
			state := incoming[key]
			if state == 0 {
				continue
			}
			for outcome, target := range edges[key] {
				out := state
				if node.Kind == NodeMergeChange && outcome == "merged" {
					out = pathMerged
				}
				next := incoming[target] | out
				if next != incoming[target] {
					incoming[target] = next
					changed = true
				}
			}
		}
	}
	for key, node := range nodes {
		if node.Kind != NodeTerminal || node.Config.Terminal == nil || node.Config.Terminal.Resolution != ResolutionMerged {
			continue
		}
		if incoming[key] != pathMerged {
			return fmt.Errorf("merged terminal node %q must only be reachable after a successful merge_change outcome", key)
		}
	}
	return nil
}

func ValidateDoneResolution(resolution DoneResolution) error {
	switch resolution {
	case ResolutionCompleted, ResolutionMerged, ResolutionRejected, ResolutionAbandoned, ResolutionCancelled, ResolutionFailed:
		return nil
	default:
		return fmt.Errorf("invalid done resolution %q", resolution)
	}
}

func encodeNodeConfig(config FlowNodeConfig) (string, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encode node config: %w", err)
	}
	return string(data), nil
}

func decodeNodeConfig(raw string) (FlowNodeConfig, error) {
	var config FlowNodeConfig
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return FlowNodeConfig{}, fmt.Errorf("decode node config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return FlowNodeConfig{}, errors.New("decode node config: expected exactly one JSON value")
		}
		return FlowNodeConfig{}, fmt.Errorf("decode node config: %w", err)
	}
	return config, nil
}
