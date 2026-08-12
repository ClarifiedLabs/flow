# Flow Author

## Workflow

1. Inspect the assignment before editing:
   - Use the task title and Markdown body from the initial prompt; the body contains the task's requirements and specification. Run `flow task show "$FLOW_TASK_ID"` only if that context is missing.
   - Check `FLOW_BRANCH`, `FLOW_BASE`, and `FLOW_CHANGE_ID`. If a prior session left a handoff, it is included in your prompt as "Prior Handoff" (there is no handoff file in the worktree to read).
   - If this is a fix round, inspect review threads with `flow thread list "$FLOW_CHANGE_ID"`.

2. Implement the requested change on the checked-out branch.
   - Keep edits scoped to the task and repository instructions.
   - Add regression tests when fixing bugs.
   - Do not certify or reopen review threads; authors may only claim them.

3. Verify locally with the narrowest useful tests first, then broader tests when risk justifies it.
   - Report meaningful progress with `flow status "<message>"` during longer work.
   - For addressed review threads, use `flow thread claim <thread-id> fixed|not_warranted|superseded`.
   - If a blocking concern cannot be fixed, report it with `flow status --kind question` and do not complete the node.

4. Finalize with three actions:
   - `git commit` your work with a conventional-commit message.
   - Create `.flow/session/` and write a concise Markdown summary of the change and verification results to `.flow/session/SUMMARY.md`.
   - Run `flow complete --summary-file .flow/session/SUMMARY.md [--evidence type:value]`. It pushes the run-specific branch, creates the typed change artifact, and advances the active workflow node.
   - Attach typed evidence for what proves the change works: `--evidence test:"go test ./... green"`, `--evidence pr:<url>`, `--evidence note:"<what you verified>"` (repeatable). The change's pinned HEAD is recorded automatically as commit evidence.
   - A useful summary looks like:

     ```
     # Change Summary

     ## Current Goal
     <goal>

     ## Completed Work
     <what you did>

     ## Remaining Work
     <what is left>

     ## Tests Run and Results
     <commands and outcomes>

     ## Failed Approaches
     <dead ends, or "None.">

     ## Important Files and Commands
     <files/commands the next session should inspect first>

     ## Next Recommended Action
     <the next concrete step>
     ```
