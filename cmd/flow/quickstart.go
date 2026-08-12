package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/cliout"
)

// quickstartContract is flow's agent operating contract, printed by
// `flow quickstart` (human long form) and embedded in a compact form by
// `flow init --with-agents`. Keep it aligned with docs/usage.md.
const quickstartContract = `# Flow agent operating contract

Flow coordinates your task: it owns the workflow, the git exchange, and the
review/verify gates. You do the work inside a session it starts.

## The loop

1. Fetch your prompt: it names the task, the acceptance criteria, and the
   files in scope. (Non-interactive agents: flow fetch-prompt.)
2. Do the work on the task branch Flow gives you. Commit as you go.
3. Finish with flow complete --summary-file PATH. Write a substantive summary:
   what changed, why, and what you verified. Flow seals the change, records
   the head SHA, and hands off to review/verify.
4. If a human or automated gate opens a question, answer it with
   flow task respond TASK_ID --node-run ... --review-wait ... --outcome ... .

## Working with the CLI

- Add --json or --agent to any command for stable machine output
  (flow --agent version prints agent_format and protocol).
- task create is idempotent by default: a retry reuses the task instead of
  duplicating it (agent output shows reused=true).
- flow events tails the project event log: one ordered stream of task,
  epic, feature, and session changes. Page with --since <seq> (resume from
  the last seq you saw, or next_since from the previous page); --follow
  streams live. Use it instead of polling board to react to changes.
- flow ready lists unblocked tasks you could start; flow next prints the best
  one. flow wait TASK_ID --until done blocks until a task reaches a state.

## Do not

- Do not run flow task reset, reopen, or retry, and do not merge or land
  features/epics, unless the user explicitly asks. Those rewrite shared
  state.
- Do not edit another task's branch or push to the exchange directly.
`

// quickstartCompact is the --agent form: the durable rules without prose.
const quickstartCompact = `flow agent contract:
- loop: fetch prompt -> work on the task branch -> flow complete --summary-file (substantive summary: what/why/verified) -> answer gates via flow task respond
- machine output: any command takes --json/--agent; flow --agent version -> agent_format, protocol
- task create is idempotent (reused=true on replay)
- flow events [--since SEQ] [--follow]: ordered project event log; resume from last seen seq / next_since
- flow ready / flow next: unblocked tasks; flow wait TASK_ID --until done
- never reset/reopen/retry/merge/land unless the user asks; never edit another task's branch
`

// runQuickstart implements `flow quickstart` (alias `agent-instructions`).
func runQuickstart(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("quickstart", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return machineUsage(stdout, stderr, apiFlags.outputMode(), "quickstart", "usage: flow quickstart")
	}
	if apiFlags.outputMode().Machine() {
		// Machine output is the compact contract as one fenced text payload so
		// agents can store it verbatim.
		return cliout.WriteData(stdout, apiFlags.outputMode(), "quickstart", map[string]any{"contract": quickstartCompact})
	}
	fmt.Fprint(stdout, quickstartContract)
	return 0
}

// Managed-block markers for `flow init --with-agents`. The block between them
// is owned by flow and refreshed on rerun; content outside is left untouched.
const (
	agentsBlockBegin = "<!-- flow:begin -->"
	agentsBlockEnd   = "<!-- flow:end -->"
)

// agentsBlock is the repo-local quickref written into AGENTS.md / CLAUDE.md.
func agentsBlock() string {
	return agentsBlockBegin + "\n" +
		"## Flow\n\n" +
		"This repo is a Flow project. Flow owns the task workflow, git exchange, and review gates.\n\n" +
		quickstartCompact +
		"\nRun `flow quickstart` for the full operating contract.\n" +
		agentsBlockEnd + "\n"
}

// writeAgentsBlock installs or refreshes the flow managed block in path.
// Returns the outcome: "created", "refreshed", "appended", or "skipped"
// (symlink refused).
func writeAgentsBlock(path string) (string, error) {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "skipped", fmt.Errorf("refusing to write through symlink %s", path)
	}

	block := agentsBlock()
	existing := ""
	if err == nil {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		existing = string(raw)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	switch {
	case existing == "":
		if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
			return "", err
		}
		return "created", nil
	case strings.Contains(existing, agentsBlockBegin):
		begin := strings.Index(existing, agentsBlockBegin)
		end := strings.Index(existing, agentsBlockEnd)
		if end == -1 || end < begin {
			// Corrupt/partial block: replace from the begin marker onward.
			updated := existing[:begin] + block
			if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
				return "", err
			}
			return "refreshed", nil
		}
		updated := existing[:begin] + block + existing[end+len(agentsBlockEnd)+1:]
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return "", err
		}
		return "refreshed", nil
	default:
		sep := "\n"
		if strings.HasSuffix(existing, "\n") {
			sep = ""
		}
		if err := os.WriteFile(path, []byte(existing+sep+block), 0o644); err != nil {
			return "", err
		}
		return "appended", nil
	}
}

// installAgentsBlocks writes the managed block into AGENTS.md (always) and
// CLAUDE.md (when it already exists as a real file) under repoRoot.
func installAgentsBlocks(repoRoot string, stdout io.Writer) {
	targets := []string{"AGENTS.md"}
	claude := filepath.Join(repoRoot, "CLAUDE.md")
	if info, err := os.Lstat(claude); err == nil && info.Mode().IsRegular() {
		targets = append(targets, "CLAUDE.md")
	}
	for _, name := range targets {
		outcome, err := writeAgentsBlock(filepath.Join(repoRoot, name))
		if err != nil {
			fmt.Fprintf(stdout, "agents_instructions: %s %s (%v)\n", name, "skipped", err)
			continue
		}
		fmt.Fprintf(stdout, "agents_instructions: %s %s\n", name, outcome)
	}
}
