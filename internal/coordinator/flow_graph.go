package coordinator

import (
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

// UnmarshalJSON accepts the deprecated required field while keeping blocking as
// the only marshaled representation. Specifying both is rejected so legacy and
// canonical inputs can never have ambiguous precedence.
func (c *ReviewAgentConfig) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, hasBlocking := fields["blocking"]
	_, hasRequired := fields["required"]
	if hasBlocking && hasRequired {
		return errors.New("review agent cannot specify both blocking and deprecated required")
	}

	var decoded struct {
		AgentDefID string `json:"agent_def_id"`
		Blocking   *bool  `json:"blocking"`
		Required   *bool  `json:"required"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	c.AgentDefID = decoded.AgentDefID
	c.Blocking = decoded.Blocking
	if hasRequired {
		c.Blocking = decoded.Required
	}
	if c.Blocking == nil {
		blocking := true
		c.Blocking = &blocking
	}
	return nil
}

type ChangeReviewNodeConfig struct {
	Agents []ReviewAgentConfig `json:"agents"`
}

type HumanGateNodeConfig struct {
	Instructions string   `json:"instructions,omitempty"`
	Outcomes     []string `json:"outcomes"`
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

// ReviewAggregationAgent selects the runtime for the final aggregation pass:
// prefer the first reviewer allowed to block, otherwise use the first advisory
// reviewer and keep the aggregate advisory.
func ReviewAggregationAgent(agents []SnapshotReviewAgent) (SnapshotReviewAgent, bool) {
	for _, agent := range agents {
		if agent.Blocking {
			return agent, true
		}
	}
	if len(agents) == 0 {
		return SnapshotReviewAgent{}, false
	}
	return agents[0], true
}

// UnmarshalJSON keeps already-scheduled workflow snapshots readable after the
// workflow-facing field was renamed from required to blocking.
func (a *SnapshotReviewAgent) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, hasBlocking := fields["blocking"]
	_, hasRequired := fields["required"]
	if hasBlocking && hasRequired {
		return errors.New("snapshot review agent cannot specify both blocking and deprecated required")
	}

	var decoded struct {
		Blocking *bool            `json:"blocking"`
		Required *bool            `json:"required"`
		Agent    AgentDefSnapshot `json:"agent"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	a.Blocking = true
	if hasBlocking && decoded.Blocking != nil {
		a.Blocking = *decoded.Blocking
	}
	if hasRequired && decoded.Required != nil {
		a.Blocking = *decoded.Required
	}
	a.Agent = decoded.Agent
	return nil
}

type FlowNodeSnapshotConfig struct {
	Agent              *AgentNodeSnapshotConfig        `json:"agent,omitempty"`
	AutomatedChecks    *AutomatedChecksNodeConfig      `json:"automated_checks,omitempty"`
	ChangeReview       *ChangeReviewNodeSnapshotConfig `json:"change_review,omitempty"`
	HumanGate          *HumanGateNodeConfig            `json:"human_gate,omitempty"`
	VerifyChange       *VerifyChangeNodeSnapshotConfig `json:"verify_change,omitempty"`
	MaterializeTaskSet *MaterializeTaskSetNodeConfig   `json:"materialize_task_set,omitempty"`
	MergeChange        *MergeChangeNodeConfig          `json:"merge_change,omitempty"`
	Terminal           *TerminalNodeConfig             `json:"terminal,omitempty"`
}

type AgentNodeSnapshotConfig struct {
	Agent     AgentDefSnapshot `json:"agent"`
	Workspace WorkspaceMode    `json:"workspace"`
	Artifact  ArtifactKind     `json:"artifact"`
}

type ChangeReviewNodeSnapshotConfig struct {
	Agents []SnapshotReviewAgent `json:"agents"`
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

// TerminalNodeKey returns the key of the first terminal node, which is where
// "skip to merge" hands a held run back to the executor.
func (s FlowSnapshot) TerminalNodeKey() (string, bool) {
	for _, node := range s.Nodes {
		if node.Kind == NodeTerminal {
			return node.Key, true
		}
	}
	return "", false
}

func (s FlowSnapshot) Target(from, outcome string) (string, bool) {
	for _, edge := range s.Edges {
		if edge.From == from && edge.Outcome == outcome {
			return edge.To, true
		}
	}
	return "", false
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
	if err := validateGraphReachability(input.StartNode, nodes, edgesByNode); err != nil {
		return FlowInput{}, err
	}
	if err := validateArtifactFlow(input.StartNode, nodes, edgesByNode); err != nil {
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
		config.MergeChange != nil, config.Terminal != nil,
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
			return []string{"approved", "changes_requested"}, config, nil
		}
	case NodeHumanGate:
		if config.HumanGate != nil {
			config.HumanGate.Instructions = strings.TrimSpace(config.HumanGate.Instructions)
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
	case NodeTerminal:
		if config.Terminal != nil {
			if err := validateDoneResolution(config.Terminal.Resolution); err != nil {
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
	case NodeAutomatedChecks, NodeChangeReview, NodeVerifyChange, NodeMergeChange:
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

func validateDoneResolution(resolution DoneResolution) error {
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
