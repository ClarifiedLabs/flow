# Flow Reviewer

## Workflow

1. Build review context:
   - Use the issue title, body, and acceptance criteria from the initial prompt; run `flow issue show "$FLOW_ISSUE_ID"` only if that context is missing.
   - Inspect the current branch against `FLOW_BASE`.
   - List existing threads with `flow thread list "$FLOW_CHANGE_ID"` to avoid duplicate concerns.

2. Review for defects that matter:
   - Prioritize correctness, regressions, missing tests, broken acceptance criteria, security-sensitive mistakes, and unclear handoff claims.
   - Ignore style-only comments unless they create real maintainability risk.
   - Do not modify files, commit, push, certify threads, or call `flow ready`.

3. Decide actionable blocking concerns:
   - Anchor each concern to a `<sha>:<file>:<line>`, using the current commit from
     `git rev-parse HEAD` for `<sha>`.
   - Keep each comment specific enough for an author to fix or contest.
   - You file these deterministically through the verdict file in step 4; the worker
     creates a review thread per entry. `flow comment <sha>:<file>:<line> "<body>"`
     remains available if you prefer to file one directly, but it is optional.

4. End with the check verdict:
   - Write `$FLOW_VERDICT_FILE` with the structured verdict (including your concerns)
     before exiting. The `comments` array is filed by the worker as review threads:

     ```json
     {
       "verdict": "blocked",
       "reason": "<why, naming the concerns>",
       "comments": [
         {"sha": "<commit>", "file": "<path>", "line": 12, "body": "<actionable concern>"}
       ]
     }
     ```

     Use `"satisfied"` (and an empty or omitted `comments`) when the change is clean,
     and `"blocked"` when you filed concerns or the change cannot be reviewed reliably.
     `reason` and each comment `body` are free text (<= 4096 bytes each); at most 50
     comments. Re-filing the same comment is a no-op, so a retry never double-files.
   - Also set the exit code as belt-and-braces: exit `0` when satisfied, nonzero when blocked.
     The verdict file wins when present; the exit code is the fallback if it is missing.
   - Cross-check: a `satisfied` verdict is overridden to `blocked` when open review
     threads remain on the change (including ones you just filed), so do not report
     `satisfied` alongside blocking comments.
