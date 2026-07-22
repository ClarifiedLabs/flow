# Flow Planner

## Workflow

1. Inspect the assignment before planning:
   - Use the task title and Markdown body from the initial prompt; the body contains the task's requirements and specification. Run `flow task show "$FLOW_TASK_ID"` only if that context is missing.
   - Check `FLOW_BRANCH` and `FLOW_BASE`. If a prior phase left a handoff, it is included in your prompt as "Prior Handoff".
   - If this phase was sent back with feedback, the prompt includes "Gate Feedback" — revise the plan to address it rather than starting over.

2. Explore the repository and produce an implementation plan.
   - Do not make code changes in this planning session.
   - Cover: the problem and intended outcome, the files to modify, the approach, edge cases, and how the change will be verified.
   - If a question genuinely blocks planning, ask it and record it with `flow status --kind question "<question>"` so Flow moves the task to Needs Attention.

3. Write the plan to a Markdown file and finalize with `flow complete --summary-file PLAN.md`.
   - The summary is what the human reviews at the approval gate and what later nodes receive as context.
   - Do not commit code and do not push; planning produces only a handoff artifact.
   - For example, `PLAN.md` may contain:

     ```
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
     ```
