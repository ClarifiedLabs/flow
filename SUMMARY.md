# Change Summary

## Current Goal

Restore rendering of headless changes (no `head_sha` recorded yet) on the standalone `/ui/changes/:id` route in `renderChangeRoute`, while keeping the retryable coherent-pair error when a named head disagrees with the diff head. Reconcile with the superseded task t-flow-0093 by folding its requested headless-mount coverage into this work.

## Completed Work

This fix round integrated `origin/main` into `task/t-flow-0107/run-1` per the coordinator's auto-merge remediation, resolved the empty-change situation, and re-submitted.

- **Production behavior is already correct on `main`.** `renderChangeRoute` (`internal/web/assets/change-route.js`) distinguishes 'no head recorded yet' from 'head moved between the two GETs': metadata naming no `head_sha` mounts as-is with an explicit empty diff (`{ ...data, diff: {} }`, lines 30-37), skips the `/diff` fetch entirely, and returns `true` — no throw, no poll-retry loop. A change that DOES name a head still requires a verified pair: a diff whose `head_sha` disagrees with the metadata head is retried and then fails with `The change advanced while it was loading` (lines 39-50). The headless branch was restored on `main` by the t-flow-0074 merge; the regression described in the task body never landed on `main`.
- **The previous round's test is already on `main`.** The prior session folded t-flow-0093's requested coverage into `internal/web/assets/elements.test.mjs` ("the standalone change route mounts a headless change as-is with no diff fetch"). Before this fix round, t-flow-0093 itself merged to `main` (`e9363d9`) with the identical 33-line test, so the auto-merge of this change reported `squash merge has no included changes`.
- **Remediation.** Merged `origin/main` into the branch (`e4c78d6`, clean — the duplicate test auto-merged with no conflicts), leaving the branch identical to `main` (`git diff origin/main HEAD` is empty). Per the t-flow-0085 precedent for base-already-implemented changes (its merge carried only `SUMMARY.md`), this round's committed deliverable is the change summary itself, giving the squash merge an included change.

## Remaining Work

None. All acceptance criteria are satisfied by `main` and covered by its tests; this change records the reconciliation and verification.

## Tests Run and Results

- `node --test internal/web/assets/app.test.mjs internal/web/assets/elements.test.mjs` — pass (375/375), the acceptance-criteria files. Includes the headless-mount test (`elements.test.mjs:3277`, main:3732; no `/diff` fetch asserted), the unit headless coverage (`app.test.mjs:6518`), and the head-mismatch retry coverage (`app.test.mjs:126/:6438/:6468`).
- `node --test internal/web/assets/*.test.mjs` — pass, full assets suite (427/427 as of the prior round; re-ran after the `origin/main` integration).

## Failed Approaches

None. One investigation note: the task body's premise (route throws on headless changes) matches the t-flow-0064 review head `417d424b`, not current `main` — the regression never landed because t-flow-0074 merged first and t-flow-0064's squash merge did not touch `change-route.js`.

## Important Files and Commands

- `internal/web/assets/change-route.js` — headless branch at lines 30-37; verified-pair head-equality check at line 45 (already correct on `main`).
- `internal/web/assets/elements.test.mjs` — headless-mount test at line 3277 (identical copy at main:3732 via the t-flow-0093 merge).
- `internal/web/assets/app.test.mjs` — unit headless coverage at line 6518; mismatch/retry coverage at lines 126, 6438, 6468.
- Commands: `node --test internal/web/assets/app.test.mjs internal/web/assets/elements.test.mjs`, `node --test internal/web/assets/*.test.mjs`.

## Next Recommended Action

`flow complete --summary-file SUMMARY.md`; the auto-merge should now succeed with `SUMMARY.md` as the included change.
