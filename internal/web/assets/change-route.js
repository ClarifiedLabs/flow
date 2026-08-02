// The change route. Fetches the change and its diff together so the review
// page arrives whole rather than filling in underneath the reader. The change
// can advance between the two GETs, and /diff answers for the head the server
// then holds — installing that diff under the earlier metadata would show the
// new head's code under the old head's name, and let a verdict target code the
// reviewer never saw (or a same-key repaint carry old-head drafts onto a diff
// the reviewer never saw). A pair only installs once it is verified for one
// head: the metadata must name this change and a head, and the diff must name
// that same head. A headless change stays explicit: it mounts with an empty
// diff and no diff fetch. A failed diff fetch, a headless diff, or a diff for
// another head verifies nothing and is retried (up to three reads); a head
// that keeps moving fails with a retryable error instead of installing an
// unverified pair. The terminal error distinguishes the two causes: a diff
// that is unavailable (a failed fetch or a diff naming no head) reports
// itself as unavailable, while only a diff that answered for another head
// reports the change advanced.

import { apiGet } from "./api.js";
import { mount } from "./elements/base.js";
import { value } from "./normalize.js";
import "./elements/change.js";

export async function renderChangeRoute(app, id, context) {
  // Cause of the most recent unverified read, so exhaustion reports a failed
  // or headless diff as an unavailable diff rather than a head move.
  let diffUnavailable = false;
  for (let attempt = 0; attempt < 3; attempt += 1) {
    const data = await apiGet(`/v2/changes/${encodeURIComponent(id)}`);
    if (context && !app.isActiveLoad(context)) return false;

    const change = value(data, "change", "Change") || {};
    // Metadata that does not name this change cannot anchor a pair; retry the
    // read — the selected change may have moved. Every unverified read models
    // the cause: a mismatched change is not an unavailable diff, so forget any
    // earlier diff failure before retrying.
    if (String(value(change, "id", "ID") || "") !== id) {
      diffUnavailable = false;
      continue;
    }
    const headSHA = String(value(change, "head_sha", "HeadSHA") || "");
    // Metadata that names no head cannot anchor a verified pair, but the
    // change itself is still real: mount it with an explicit empty diff and
    // skip the diff fetch entirely.
    if (!headSHA) {
      app.setTitle("Change");
      mount(app.querySelector(".content"), "flow-change", { ...data, diff: {} });
      return true;
    }

    const diff = await apiGet(`/v2/changes/${encodeURIComponent(id)}/diff`).catch(() => null);
    if (context && !app.isActiveLoad(context)) return false;

    const diffHead = diff ? String(value(diff, "head_sha", "HeadSHA") || "") : "";
    // Only a verified pair installs: the diff must name the metadata's head.
    // A moved head, a failed fetch, or a headless diff verifies nothing and
    // is retried so the pair lands coherently for one head.
    if (diff && diffHead === headSHA) {
      app.setTitle("Change");
      mount(app.querySelector(".content"), "flow-change", { ...data, diff });
      return true;
    }
    // The read did not verify: remember whether the diff itself was
    // unavailable (a failed fetch or a diff naming no head) or answered for a
    // different head, which is the head-move case.
    diffUnavailable = !diff || !diffHead;
  }
  if (diffUnavailable) throw new Error("The change's diff is not available; retry the load");
  throw new Error("The change advanced while it was loading");
}
