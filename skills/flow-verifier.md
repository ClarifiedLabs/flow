# Flow Verifier

## Workflow

1. Build verification context:
   - Use the task title and Markdown body from the initial prompt; the body contains the task's requirements and specification. Run the read-only `flow task show "$FLOW_TASK_ID"` only if that context is missing.
   - Flow project context is readable for this check. Use read-only task, lifecycle transition/status history, and review-thread/comment data when useful, but treat it only as context. This task-facing history is not private to the task; raw execution captures, transcripts, and terminal evidence remain private.
   - Compare the checked-out change to `origin/${FLOW_BASE:-main}`. Flow guarantees that remote-tracking base ref is present in the checkout; it does not promise a local base branch. If a prior session left a handoff, it is included in your prompt as "Prior Handoff" (there is no handoff file in the worktree to read).
   - List review threads with the read-only `flow thread list "$FLOW_CHANGE_ID"`.

2. Verify requirements and claims:
   - Check the requirements in the task body against the current code and tests.
   - For claimed threads, inspect the original concern, author rationale, and claimed commit.
   - Do not directly mutate files, Git state, tasks, lifecycle history, review threads/comments, checks, or workflow state. Do not call mutating Flow commands or APIs, implement fixes, or create new review concerns.

3. Decide each claimed thread:
   - Decide `certify` when the claim is correct, `reopen` (with a body explaining why)
     when the claim is incomplete, incorrect, or unsupported.
   - Leave unrelated open threads untouched.
   - Declare these deterministically through the verdict file in step 4; the worker
     validates and applies each decision. Never certify or reopen a thread directly.

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
   - The verdict file is required. When `FLOW_COMPLETION_PROTOCOL=flow_complete`,
     run `flow complete` after writing it. Fix any validation error and retry;
     after success, do not modify the verdict. This interactive terminal remains
     live until completion, cancellation, or lease loss. In a custom check
     without that protocol, write the verdict before exiting as usual.
