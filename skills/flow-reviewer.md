# Flow Reviewer

## Workflow

1. Build review context:
   - Use the task title and Markdown body from the initial prompt; the body contains the task's requirements and specification. Run the read-only `flow task show "$FLOW_TASK_ID"` only if that context is missing.
   - Flow project context is readable for this check. Use read-only task, lifecycle transition/status history, and review-thread/comment data when useful, but treat it only as context. This task-facing history is not private to the task; raw execution captures, transcripts, and terminal evidence remain private.
   - Compare the checked-out change to `origin/${FLOW_BASE:-main}`. Flow guarantees that remote-tracking base ref is present in the checkout; it does not promise a local base branch.
   - List existing threads with the read-only `flow thread list "$FLOW_CHANGE_ID"` to avoid duplicate concerns.
   - Before reviewing individual lines, write down the change's correctness and
     security invariants. Check related edge cases together so one invariant
     does not turn into one newly discovered blocker per review cycle.
   - On a later review cycle, inspect the claimed threads and the delta since
     the prior reviewed head. A new blocker must either be introduced by that
     delta or directly violate an original task requirement/invariant.

2. Review for defects that matter:
   - Prioritize correctness, regressions, missing tests, unmet task requirements, security-sensitive mistakes, and unclear handoff claims.
   - Ignore style-only comments unless they create real maintainability risk.
   - Classify findings before deciding the verdict:
     - `critical` or `high`, introduced by this change, and not a duplicate:
       blocking concern for this task.
     - Pre-existing, `medium`/`low`, or duplicate: non-blocking follow-up. Keep
       it in the verdict, but do not extend this task's author-review loop.
   - In this workflow, `high` includes a reproducible correctness regression,
     security flaw, unmet explicit task requirement, or a missing test that
     leaves such a task-caused bug unprotected. Use `medium`/`low` only when the
     requested behavior remains correct and the finding can safely be separate
     follow-up work.
   - Treat an omitted requirement as introduced by the change when the task
     explicitly required that behavior.
   - Do not directly mutate files, Git state, tasks, lifecycle history, review threads/comments, checks, or workflow state. Do not call mutating Flow commands or APIs.
   - The prompt identifies one distinct mode: standalone reviewer, parallel discovery source, or final aggregator. A standalone verdict is applied directly by the worker. A final aggregator decides from supplied Candidate Reports. Neither is the discovery mode.
   - In parallel discovery, report candidates only through the verdict file. The worker validates the verdict against the lease-bound source check, persists it as that source check's result, and later supplies the persisted result to the final aggregator as a Candidate Report. Discovery never creates or changes threads or chooses the workflow outcome.

3. Decide actionable blocking concerns:
   - Anchor each concern to a `<sha>:<file>:<line>`, using the current commit from
     `git rev-parse HEAD` for `<sha>`.
   - Keep each comment specific enough for an author to fix or contest.
   - Declare these deterministically through the verdict file in step 4. For a
     standalone or final aggregation reviewer, the worker creates a review
     thread per eligible entry; the reviewer never files one directly.

4. End with the check verdict:
   - Write `$FLOW_VERDICT_FILE` with the structured verdict (including your concerns)
     before exiting. For standalone and final aggregation reviewers, the `comments`
     array is applied by the worker as review threads. For discovery, the worker
     persists it only as the lease-bound source check result for Candidate Reports:

     ```json
     {
       "verdict": "blocked",
       "reason": "<why, naming the task-caused high-severity concerns>",
       "comments": [
         {
           "sha": "<commit>",
           "file": "<path>",
           "line": 12,
           "body": "<actionable concern>",
           "severity": "critical|high|medium|low",
           "introduced_by_change": true,
           "requirement": "<task requirement or invariant this finding violates>",
           "duplicate_of": "<existing thread id, only when duplicated>",
           "follow_up": "<suggested separate task or next action, when non-blocking>",
           "task_action": {
             "action": "create_task|use_existing_task",
             "title": "<required only for create_task>",
             "body": "<self-contained Markdown scope and acceptance criteria; required only for create_task>",
             "task_id": "<required only for use_existing_task>"
           }
         }
       ]
     }
     ```

     `task_action` is optional and reserved for the final parallel-review
     aggregation job. It declares work for the worker/coordinator to apply; do not
     create or mutate a task directly. Add it only to a unique, actionable non-blocking issue that
     is safe to defer from the current change. Reuse an open task only for a
     high-confidence same-root-issue match from the supplied task candidates;
     otherwise create a task. Omit it for blocking findings, review-thread
     duplicates, speculative observations, and informational notes.
     Use `"blocked"` only when at least one comment is `critical`/`high`,
     introduced by this change, and not a duplicate, or when the change cannot
     be reviewed reliably. Otherwise use `"satisfied"`; keep non-blocking
     findings in `comments` so Flow retains them as follow-up context without
     opening blocking threads.
     `reason` and each comment `body` are free text (<= 4096 bytes each); at most 50
     comments. Re-filing the same comment is a no-op, so a retry never double-files.
   - The verdict file is required. When `FLOW_COMPLETION_PROTOCOL=flow_complete`,
     run `flow complete` after writing it. Fix any validation error and retry;
     after success, do not modify the verdict. This interactive terminal remains
     live until completion, cancellation, or lease loss. In a custom check
     without that protocol, write the verdict before exiting as usual.
   - Cross-check: a `satisfied` standalone/final verdict is overridden to `blocked`
     when open review threads remain on the change (including ones the worker creates
     from this verdict), so do not report `satisfied` alongside blocking comments.
