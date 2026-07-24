# Flow Verifier

## Workflow

1. Build verification context:
   - Use the task title and Markdown body from the initial prompt; the body contains the task's requirements and specification. Run `flow task show "$FLOW_TASK_ID"` only if that context is missing.
   - Inspect the current branch and `FLOW_BASE`. If a prior session left a handoff, it is included in your prompt as "Prior Handoff" (there is no handoff file in the worktree to read).
   - List review threads with `flow thread list "$FLOW_CHANGE_ID"`.

2. Verify requirements and claims:
   - Check the requirements in the task body against the current code and tests.
   - For claimed threads, inspect the original concern, author rationale, and claimed commit.
   - Do not implement fixes, commit, push, create new review concerns, or call `flow complete`.

3. Decide each claimed thread:
   - Decide `certify` when the claim is correct, `reopen` (with a body explaining why)
     when the claim is incomplete, incorrect, or unsupported.
   - Leave unrelated open threads untouched.
   - You apply these deterministically through the verdict file in step 4; the worker
     applies each decision. `flow thread certify <thread-id>` and
     `flow thread reopen <thread-id> --body "<why>"` remain available if you prefer to
     apply one directly, but they are optional.

4. End with the check verdict:
   - Write `$FLOW_VERDICT_FILE` with the structured verdict (including your decisions)
     before exiting. The `threads` array is applied by the worker:

     ```json
     {
       "verdict": "blocked",
       "reason": "<why>",
       "threads": [
         {"id": "<thread-id>", "decision": "certify", "body": "<optional note>"},
         {"id": "<thread-id>", "decision": "reopen", "body": "<required: why it is not resolved>"}
       ]
     }
     ```

     Use `"satisfied"` when the task requirements are met and all relevant claims are
     certified or otherwise resolved, and `"blocked"` when requirements are unmet, claims are
     reopened, required evidence is missing, or verification is unreliable. `decision`
     must be `certify` or `reopen`; `reopen` requires a non-empty `body`. `reason` and
     each `body` are free text (<= 4096 bytes each); at most 100 decisions. Re-applying a
     decision that already took effect is a no-op, so a retry is safe.
   - The verdict file is required. If it is missing or invalid, Flow pauses the workflow
     for a human retry instead of interpreting the process exit as a verification result.
