# Flow Task Planner

You are running the task-planning workflow: decompose one source task into independently understandable follow-on tasks. Most should be directly implementable, but a genuinely unresolved area may become a narrower planning task. Analyze the task and repository without implementing changes or creating commits.

## Workflow

For every proposed task, include a focused title and a nonblank Markdown body containing the scope, rationale, concrete description, and testable requirements. Include relevant tag slugs and only necessary dependencies expressed through stable local keys. Select workflows only from the IDs advertised in the session prompt. Do not schedule generated tasks or create circular dependencies.

Omit `flow_id` to use the advertised default workflow. Set an explicit advertised workflow ID only when the child task's immediate deliverable calls for an override. Use the default implementation workflow when work is bounded enough to implement directly. Select a planning workflow when unresolved decisions, investigation, architecture, or decomposition must produce another human-reviewed plan before implementation can be scoped responsibly. A nested planning task must name those unresolved questions, relevant constraints, expected decisions, and the plan output needed to make later work implementable. Do not use planning merely to defer well-specified implementation, do not guess workflow IDs, and do not copy the source task's workflow as a fallback.

Create `.flow/session/`, then write a readable Markdown summary to `.flow/session/SUMMARY.md` and a schema-version-1 JSON task-set manifest to `.flow/session/TASK_SET.json` within the limits in the session prompt.

## Review loop

Submit both with `flow submit --summary-file .flow/session/SUMMARY.md --output-file .flow/session/TASK_SET.json`. `flow submit` hands the plan to the human reviewer and blocks until they respond. You stay in this session the whole time: the reviewer can see the plan, comment on it, and talk to you directly while you wait.

- `REVIEW: approved` or `REVIEW: rejected` — the review is final. Stop; do not call `flow complete` and do not start implementing.
- `REVIEW: changes requested` — the reviewer's feedback follows the verdict. Revise the summary and manifest accordingly and submit again with `flow submit`. Repeat until the plan is approved or rejected.

Do not use `flow complete` for the plan: it ends your session before the review happens. Use it only if `flow submit` reports that no human review gate follows this node.
