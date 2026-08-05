package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// runAgentDefs manages reusable harness+model+effort+prompt configurations.
// Project catalogs inherit coordinator-global definitions and may override them
// by name.
func runAgentDefs(args []string, stdout, stderr io.Writer) int {
	return runAgentDefsWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runAgentDefsWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: flow agent-defs {list|create -f FILE|edit -f FILE ID|rm ID} [--global]")
		return 2
	}
	switch args[0] {
	case "list":
		return runAgentDefsList(options.withConfig(args[1:]), stdout, stderr)
	case "create":
		return runAgentDefsCreate(options.withConfig(args[1:]), stdout, stderr)
	case "edit":
		return runAgentDefsEdit(options.withConfig(args[1:]), stdout, stderr)
	case "rm":
		return runAgentDefsRemove(options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent-defs subcommand: %s\n", args[0])
		return 2
	}
}

func rejectConflictingAgentDefScope(global bool, apiFlags *apiFlagValues, stderr io.Writer) bool {
	if !global || strings.TrimSpace(apiFlags.project) == "" {
		return false
	}
	fmt.Fprintln(stderr, "--global and --project cannot be used together")
	return true
}

func runAgentDefsList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-defs list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var global bool
	flags.BoolVar(&global, "global", false, "manage coordinator-global agent definitions")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if rejectConflictingAgentDefScope(global, apiFlags, stderr) {
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	var defs []coordinator.AgentDef
	if global {
		defs, err = client.ListGlobalAgentDefs()
	} else {
		defs, err = client.ListAgentDefs()
	}
	if err != nil {
		fmt.Fprintf(stderr, "list agent defs: %v\n", err)
		return 1
	}
	for _, def := range defs {
		selection := def.Model
		if def.ReasoningEffort != "" {
			if selection != "" {
				selection += " "
			}
			selection += "effort=" + def.ReasoningEffort
		}
		if selection == "" {
			selection = "-"
		}
		scope := ""
		if global {
			scope = "\tglobal"
		} else if def.Inherited {
			scope = "\tinherited"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s%s\n", def.ID, def.Name, def.Harness, selection, scope)
	}
	return 0
}

func runAgentDefsCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-defs create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var file string
	var global bool
	flags.StringVar(&file, "f", "", "agent definition YAML/JSON file (- for stdin)")
	flags.BoolVar(&global, "global", false, "manage coordinator-global agent definitions")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if rejectConflictingAgentDefScope(global, apiFlags, stderr) {
		return 2
	}
	var input coordinator.AgentDefInput
	if err := decodeConfigFile(file, &input); err != nil {
		fmt.Fprintf(stderr, "read agent definition: %v\n", err)
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	var def coordinator.AgentDef
	if global {
		def, err = client.CreateGlobalAgentDef(input)
	} else {
		def, err = client.CreateAgentDef(input)
	}
	if err != nil {
		fmt.Fprintf(stderr, "create agent def: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", def.ID, def.Name, def.Harness)
	return 0
}

func runAgentDefsEdit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-defs edit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var file string
	var global bool
	flags.StringVar(&file, "f", "", "agent definition YAML/JSON file (- for stdin)")
	flags.BoolVar(&global, "global", false, "manage coordinator-global agent definitions")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if rejectConflictingAgentDefScope(global, apiFlags, stderr) {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "agent definition id is required")
		return 2
	}
	var input coordinator.AgentDefInput
	if err := decodeConfigFile(file, &input); err != nil {
		fmt.Fprintf(stderr, "read agent definition: %v\n", err)
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	var def coordinator.AgentDef
	if global {
		def, err = client.UpdateGlobalAgentDef(flags.Arg(0), input)
	} else {
		def, err = client.UpdateAgentDef(flags.Arg(0), input)
	}
	if err != nil {
		fmt.Fprintf(stderr, "update agent def: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", def.ID, def.Name, def.Harness)
	return 0
}

func runAgentDefsRemove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-defs rm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var global bool
	flags.BoolVar(&global, "global", false, "manage coordinator-global agent definitions")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if rejectConflictingAgentDefScope(global, apiFlags, stderr) {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "agent definition id is required")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	if global {
		err = client.DeleteGlobalAgentDef(flags.Arg(0))
	} else {
		err = client.DeleteAgentDef(flags.Arg(0))
	}
	if err != nil {
		fmt.Fprintf(stderr, "delete agent def: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "deleted")
	return 0
}

// runFlows manages the project's trusted workflow graph catalog.
func runFlows(args []string, stdout, stderr io.Writer) int {
	return runFlowsWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runFlowsWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: flow flows {list|create -f FILE|edit ID -f FILE|rm ID|set-default ID_OR_NAME}")
		return 2
	}
	switch args[0] {
	case "list":
		return runFlowsList(options.withConfig(args[1:]), stdout, stderr)
	case "create":
		return runFlowsCreate(options.withConfig(args[1:]), stdout, stderr)
	case "edit":
		return runFlowsEdit(options.withConfig(args[1:]), stdout, stderr)
	case "rm":
		return runFlowsRemove(options.withConfig(args[1:]), stdout, stderr)
	case "set-default":
		return runFlowsSetDefault(options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown flows subcommand: %s\n", args[0])
		return 2
	}
}

func runFlowsList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("flows list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	list, err := client.ListFlows()
	if err != nil {
		fmt.Fprintf(stderr, "list flows: %v\n", err)
		return 1
	}
	for _, flow := range list.Flows {
		marker := ""
		if flow.Default {
			marker = "*"
		}
		reviewers, verifiers := summarizeFlowReviewAgents(flow)
		fmt.Fprintf(stdout, "%s\t%s%s\tstart=%s\tnodes=%d\treviewers=%d blocking/%d advisory\tverifiers=%d blocking/%d advisory\n",
			flow.ID, flow.Name, marker, flow.StartNode, len(flow.Nodes),
			reviewers.blocking, reviewers.advisory, verifiers.blocking, verifiers.advisory)
	}
	return 0
}

type flowReviewAgentSummary struct {
	blocking int
	advisory int
}

// summarizeFlowReviewAgents reads only the public workflow graph. An omitted
// blocking value has the graph contract's blocking default; false is advisory.
func summarizeFlowReviewAgents(flow coordinator.Flow) (reviewers, verifiers flowReviewAgentSummary) {
	add := func(summary *flowReviewAgentSummary, agents []coordinator.ReviewAgentConfig) {
		for _, agent := range agents {
			if agent.Blocking == nil || *agent.Blocking {
				summary.blocking++
			} else {
				summary.advisory++
			}
		}
	}
	for _, node := range flow.Nodes {
		switch node.Kind {
		case coordinator.NodeChangeReview:
			if node.Config.ChangeReview != nil {
				add(&reviewers, node.Config.ChangeReview.Agents)
			}
		case coordinator.NodeVerifyChange:
			if node.Config.VerifyChange != nil {
				add(&verifiers, node.Config.VerifyChange.Agents)
			}
		}
	}
	return reviewers, verifiers
}

func runFlowsCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("flows create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var file string
	flags.StringVar(&file, "f", "", "flow YAML/JSON file (- for stdin)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	var input coordinator.FlowInput
	if err := decodeConfigFile(file, &input); err != nil {
		fmt.Fprintf(stderr, "read flow: %v\n", err)
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	flow, err := client.CreateFlow(input)
	if err != nil {
		fmt.Fprintf(stderr, "create flow: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\n", flow.ID, flow.Name)
	return 0
}

func runFlowsEdit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("flows edit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var file string
	flags.StringVar(&file, "f", "", "flow YAML/JSON file (- for stdin)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "flow id is required")
		return 2
	}
	var input coordinator.FlowInput
	if err := decodeConfigFile(file, &input); err != nil {
		fmt.Fprintf(stderr, "read flow: %v\n", err)
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	flow, err := client.UpdateFlow(flags.Arg(0), input)
	if err != nil {
		fmt.Fprintf(stderr, "update flow: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\n", flow.ID, flow.Name)
	return 0
}

func runFlowsRemove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("flows rm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "flow id is required")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	if err := client.DeleteFlow(flags.Arg(0)); err != nil {
		fmt.Fprintf(stderr, "delete flow: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "deleted")
	return 0
}

func runFlowsSetDefault(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("flows set-default", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "flow id or name is required")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	flowID, err := resolveFlowRef(client, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "resolve flow: %v\n", err)
		return 1
	}
	flow, err := client.SetDefaultFlow(flowID)
	if err != nil {
		fmt.Fprintf(stderr, "set default flow: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\tdefault\n", flow.ID, flow.Name)
	return 0
}

// resolveFlowRef accepts a flow id ("fl-...") or a flow name and returns the id.
func resolveFlowRef(client *flowclient.Client, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "fl-") {
		return ref, nil
	}
	list, err := client.ListFlows()
	if err != nil {
		return "", err
	}
	for _, flow := range list.Flows {
		if flow.Name == ref {
			return flow.ID, nil
		}
	}
	return "", fmt.Errorf("no flow named %q", ref)
}

// resolveFeatureRef accepts a feature id ("f-...") or an open feature's title
// and returns the id.
func resolveFeatureRef(client *flowclient.Client, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "f-") {
		return ref, nil
	}
	features, err := client.ListFeatures("open")
	if err != nil {
		return "", err
	}
	for _, feature := range features {
		if feature.Feature.Title == ref {
			return feature.Feature.ID, nil
		}
	}
	return "", fmt.Errorf("no open feature titled %q", ref)
}

// decodeConfigFile reads a YAML or JSON config document (path "-" = stdin)
// into target. YAML is normalized through JSON so the struct JSON tags apply,
// then decoded strictly: removed workflow fields must not be silently dropped.
func decodeConfigFile(path string, target any) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("-f FILE is required")
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	normalized, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("config must contain exactly one JSON value")
		}
		return err
	}
	return nil
}
