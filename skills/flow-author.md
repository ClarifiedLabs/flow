# Flow Author

## Workflow

1. Inspect the assignment before editing:
   - Use the issue title, body, and acceptance criteria from the initial prompt; run `flow issue show "$FLOW_ISSUE_ID"` only if that context is missing.
   - Check `FLOW_BRANCH`, `FLOW_BASE`, and `FLOW_CHANGE_ID`. If a prior session left a handoff, it is included in your prompt as "Prior Handoff" (there is no handoff file in the worktree to read).
   - If this is a fix round, inspect review threads with `flow thread list "$FLOW_CHANGE_ID"`.

2. Implement the requested change on the checked-out branch.
   - Keep edits scoped to the issue and repository instructions.
   - Add regression tests when fixing bugs.
   - Do not certify or reopen review threads; authors may only claim them.

3. Verify locally with the narrowest useful tests first, then broader tests when risk justifies it.
   - Report meaningful progress with `flow status "<message>"` during longer work.
   - For addressed review threads, use `flow thread claim <thread-id> fixed|not_warranted|superseded`.
   - If a blocking concern cannot be fixed, explain it in the handoff and do not call `flow ready`.

4. Finalize with two actions:
   - `git commit` your work with a conventional-commit message.
   - `flow ready`, piping the handoff on stdin. `flow ready` pushes the branch to the Flow exchange remote, submits the handoff to the coordinator, and marks the change ready for review. Do not push or write a handoff file separately.
   - Provide the handoff via a heredoc, for example:

     ```
     flow ready <<'HANDOFF'
     # Flow Handoff

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
     HANDOFF
     ```
