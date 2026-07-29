package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TaskSetFlowOption is a project workflow the planner may assign to a generated
// task. Descriptions let the planner choose based on the task's deliverable
// instead of inferring from the source task's workflow.
type TaskSetFlowOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// TaskSetWorkflowContract is the materializer policy and workflow catalog
// advertised to an agent that produces a task-set artifact.
type TaskSetWorkflowContract struct {
	DefaultChildFlowID     string              `json:"default_child_flow_id"`
	AllowChildFlowOverride bool                `json:"allow_child_flow_override"`
	MaxItems               int                 `json:"max_items"`
	AvailableFlows         []TaskSetFlowOption `json:"available_flows"`
}

// TaskSetWorkflowContractForNode resolves the frozen materializer policy for a
// task-set-producing agent and combines it with current project flow metadata.
func (s *FlowService) TaskSetWorkflowContractForNode(ctx context.Context, snapshot FlowSnapshot, producerNodeKey string) (TaskSetWorkflowContract, bool, error) {
	config, found, err := taskSetMaterializerConfig(snapshot, producerNodeKey)
	if err != nil || !found {
		return TaskSetWorkflowContract{}, found, err
	}

	flows, err := s.List(ctx)
	if err != nil {
		return TaskSetWorkflowContract{}, false, err
	}
	contract := TaskSetWorkflowContract{
		DefaultChildFlowID:     config.DefaultChildFlowID,
		AllowChildFlowOverride: config.AllowChildFlowOverride,
		MaxItems:               config.MaxItems,
		AvailableFlows:         []TaskSetFlowOption{},
	}
	defaultFound := false
	for _, flow := range flows {
		if flow.ID == config.DefaultChildFlowID {
			defaultFound = true
		}
		if config.AllowChildFlowOverride || flow.ID == config.DefaultChildFlowID {
			contract.AvailableFlows = append(contract.AvailableFlows, TaskSetFlowOption{
				ID: flow.ID, Name: flow.Name, Description: flow.Description,
			})
		}
	}
	if !defaultFound {
		return TaskSetWorkflowContract{}, false, fmt.Errorf("default child flow %q does not exist", config.DefaultChildFlowID)
	}
	return contract, true, nil
}

// taskSetMaterializerConfig follows the artifact's paths until another agent
// replaces it or a materializer consumes it. Cycles through a human review gate
// are safe. Multiple consumers are permitted only when their policies agree.
func taskSetMaterializerConfig(snapshot FlowSnapshot, producerNodeKey string) (MaterializeTaskSetNodeConfig, bool, error) {
	producerNodeKey = strings.TrimSpace(producerNodeKey)
	producer, ok := snapshot.Node(producerNodeKey)
	if !ok || producer.Kind != NodeAgent || producer.Config.Agent == nil || producer.Config.Agent.Artifact != ArtifactTaskSet {
		return MaterializeTaskSetNodeConfig{}, false, nil
	}

	outgoing := make(map[string][]string, len(snapshot.Nodes))
	for _, edge := range snapshot.Edges {
		outgoing[edge.From] = append(outgoing[edge.From], edge.To)
	}
	visited := map[string]bool{producerNodeKey: true}
	queue := append([]string(nil), outgoing[producerNodeKey]...)
	var resolved *MaterializeTaskSetNodeConfig
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if visited[key] {
			continue
		}
		visited[key] = true
		node, ok := snapshot.Node(key)
		if !ok {
			continue
		}
		if node.Kind == NodeAgent {
			continue
		}
		if node.Kind == NodeMaterializeTaskSet && node.Config.MaterializeTaskSet != nil {
			config := normalizedTaskSetMaterializerConfig(*node.Config.MaterializeTaskSet)
			if resolved == nil {
				resolved = &config
			} else if *resolved != config {
				return MaterializeTaskSetNodeConfig{}, false, fmt.Errorf("task-set producer %q reaches materializers with conflicting policies", producerNodeKey)
			}
		}
		queue = append(queue, outgoing[key]...)
	}
	if resolved == nil {
		return MaterializeTaskSetNodeConfig{}, false, nil
	}
	return *resolved, true, nil
}

func validateTaskSetMaterializerPolicies(nodes map[string]FlowNodeInput, edges map[string]map[string]string) error {
	for producerKey, producer := range nodes {
		if producer.Kind != NodeAgent || producer.Config.Agent == nil || producer.Config.Agent.Artifact != ArtifactTaskSet {
			continue
		}
		visited := map[string]bool{producerKey: true}
		queue := make([]string, 0, len(edges[producerKey]))
		for _, target := range edges[producerKey] {
			queue = append(queue, target)
		}
		var resolved *MaterializeTaskSetNodeConfig
		for len(queue) > 0 {
			key := queue[0]
			queue = queue[1:]
			if visited[key] {
				continue
			}
			visited[key] = true
			node := nodes[key]
			if node.Kind == NodeAgent {
				continue
			}
			if node.Kind == NodeMaterializeTaskSet && node.Config.MaterializeTaskSet != nil {
				config := normalizedTaskSetMaterializerConfig(*node.Config.MaterializeTaskSet)
				if resolved == nil {
					resolved = &config
				} else if *resolved != config {
					return fmt.Errorf("task-set agent %q reaches materializers with conflicting policies", producerKey)
				}
			}
			for _, target := range edges[key] {
				queue = append(queue, target)
			}
		}
	}
	return nil
}

func normalizedTaskSetMaterializerConfig(config MaterializeTaskSetNodeConfig) MaterializeTaskSetNodeConfig {
	config.DefaultChildFlowID = strings.TrimSpace(config.DefaultChildFlowID)
	if config.MaxItems == 0 {
		config.MaxItems = 25
	}
	return config
}

func validateTaskSetWorkflowSelectionTx(ctx context.Context, tx queryer, manifest TaskSetManifest, config MaterializeTaskSetNodeConfig) error {
	config = normalizedTaskSetMaterializerConfig(config)
	if config.DefaultChildFlowID == "" {
		return errors.New("materialization requires a default child flow")
	}
	if len(manifest.Tasks) > config.MaxItems {
		return fmt.Errorf("task-set contains %d tasks; maximum is %d", len(manifest.Tasks), config.MaxItems)
	}
	if err := requireFlowTx(ctx, tx, config.DefaultChildFlowID); err != nil {
		return fmt.Errorf("default child flow: %w", err)
	}

	validated := map[string]bool{config.DefaultChildFlowID: true}
	for _, item := range manifest.Tasks {
		flowID := strings.TrimSpace(item.FlowID)
		if flowID == "" {
			flowID = config.DefaultChildFlowID
		} else if flowID != config.DefaultChildFlowID && !config.AllowChildFlowOverride {
			return fmt.Errorf("task %q may not override default child flow %q", item.Key, config.DefaultChildFlowID)
		}
		if validated[flowID] {
			continue
		}
		if err := requireFlowTx(ctx, tx, flowID); err != nil {
			return fmt.Errorf("task %q flow: %w", item.Key, err)
		}
		validated[flowID] = true
	}
	return nil
}

func requireFlowTx(ctx context.Context, tx queryer, flowID string) error {
	flowID = strings.TrimSpace(flowID)
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM flows WHERE id = ?`, flowID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("flow %q does not exist", flowID)
	}
	return nil
}
