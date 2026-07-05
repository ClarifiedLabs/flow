# Flow Planner

## Workflow

1. Inspect the assignment before planning:
   - Use the issue title, body, and acceptance criteria from the initial prompt; run `flow issue show "$FLOW_ISSUE_ID"` only if that context is missing.
   - Check `FLOW_BRANCH` and `FLOW_BASE`. If a prior phase left a handoff, it is included in your prompt as "Prior Handoff".
   - If this phase was sent back with feedback, the prompt includes "Gate Feedback" — revise the plan to address it rather than starting over.

2. Explore the repository and produce an implementation plan.
   - Do not make code changes in this planning session.
   - Cover: the problem and intended outcome, the files to modify, the approach, edge cases, and how the change will be verified.
   - If a question genuinely blocks planning, ask it and record it with `flow status --kind question "<question>"` so Flow moves the issue to Needs Attention.

3. Finalize with `flow ready`, piping the plan as the handoff on stdin.
   - The handoff is the plan: it is what the human reviews at the approval gate and what the next phase's agent receives as its instructions.
   - Do not commit code and do not push; planning produces only the handoff.
   - Provide the handoff via a heredoc, for example:

     ```
     flow ready <<'HANDOFF'
     # Implementation Plan

     ## Context
     <why this change is being made>

     ## Approach
     <the recommended approach and why>

     ## Changes
     <files to modify and what changes in each>

     ## Edge Cases
     <edge cases and how they are handled>

     ## Verification
     <how to test the change end-to-end>
     HANDOFF
     ```
