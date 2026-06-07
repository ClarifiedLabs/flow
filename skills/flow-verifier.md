# Flow Verifier

## Workflow

1. Build verification context:
   - Use the issue title, body, and acceptance criteria from the initial prompt; run `flow issue show "$FLOW_ISSUE_ID"` only if that context is missing.
   - Inspect the current branch and `FLOW_BASE`. If a prior session left a handoff, it is included in your prompt as "Prior Handoff" (there is no handoff file in the worktree to read).
   - List review threads with `flow thread list "$FLOW_CHANGE_ID"`.

2. Verify acceptance and claims:
   - Check the issue acceptance criteria against the current code and tests.
   - For claimed threads, inspect the original concern, author rationale, and claimed commit.
   - Do not implement fixes, commit, push, create new review concerns, or call `flow ready`.

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

     Use `"satisfied"` when acceptance criteria hold and all relevant claims are
     certified or otherwise resolved, and `"blocked"` when acceptance fails, claims are
     reopened, required evidence is missing, or verification is unreliable. `decision`
     must be `certify` or `reopen`; `reopen` requires a non-empty `body`. `reason` and
     each `body` are free text (<= 4096 bytes each); at most 100 decisions. Re-applying a
     decision that already took effect is a no-op, so a retry is safe.
   - Also set the exit code as belt-and-braces: exit `0` when satisfied, nonzero when blocked.
     The verdict file wins when present; the exit code is the fallback if it is missing.
