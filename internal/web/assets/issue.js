// Issue form/attachment free helpers (issue-state form, attachment upload form
// and list, the upload action).

import { apiPostForm, attachmentHref, isImageContentType, issueAPIBase } from "./api.js";
import { ISSUE_STATE_OPTIONS } from "./config.js";
import { formatBytes } from "./format.js";
import { escapeAttr, escapeHTML } from "./html.js";
import { value } from "./normalize.js";

export function projectButtonAttr(projectID) {
  const id = String(projectID || "").trim();
  return id ? ` data-project="${escapeAttr(id)}"` : "";
}

export function currentIssueState(scheduleState, triageState) {
  if (triageState === "rejected") return "rejected";
  if (scheduleState === "closed") return "closed";
  if (triageState === "triage") return "triage";
  if (scheduleState === "up_next") return "up_next";
  return "backlog";
}

export function renderIssueStateForm(issueID, currentState, projectID) {
  return `
    <form class="issue-state-form" data-issue-state-form="${escapeAttr(issueID)}"${projectButtonAttr(projectID)}>
      <label>
        <span>State</span>
        <select name="state">
          ${ISSUE_STATE_OPTIONS.map(([state, label]) => `<option value="${escapeAttr(state)}" ${state === currentState ? "selected" : ""}>${escapeHTML(label)}</option>`).join("")}
        </select>
      </label>
      <button class="button secondary" type="submit">Apply</button>
    </form>
  `;
}

export function renderAttachmentUploadForm(issueID, projectID) {
  return `
    <form class="attachment-upload" data-attachment-form data-issue="${escapeAttr(issueID)}"${projectButtonAttr(projectID)}>
      <label>
        <span>Stage</span>
        <select name="stage">
          <option value="initial">Initial</option>
          <option value="author">Author</option>
          <option value="reviewer">Reviewer</option>
          <option value="verifier">Verifier</option>
        </select>
      </label>
      <label class="wide">
        <span>File</span>
        <input name="file" type="file" required>
      </label>
      <div class="form-actions">
        <button class="button secondary" type="submit">Attach</button>
      </div>
    </form>
  `;
}

export function renderIssueAttachment(attachment, issueID, projectID) {
  const id = value(attachment, "id", "ID");
  const filename = value(attachment, "filename", "Filename") || id;
  const contentType = value(attachment, "content_type", "ContentType") || "application/octet-stream";
  const stage = value(attachment, "stage", "Stage") || "initial";
  const sizeBytes = Number(value(attachment, "size_bytes", "SizeBytes") || 0);
  const viewHref = attachmentHref(projectID, issueID, id);
  const downloadHref = `${viewHref}?download=1`;
  const meta = `${stage} · ${formatBytes(sizeBytes)} · ${contentType}`;
  const imageHTML = isImageContentType(contentType)
    ? `<a class="attachment-preview-link" href="${escapeAttr(viewHref)}" target="_blank" rel="noreferrer"><img class="attachment-preview" src="${escapeAttr(viewHref)}" alt="${escapeAttr(filename)}" loading="lazy"></a>`
    : "";
  return `
    <article class="feed-item attachment-item">
      <div class="attachment-main">
        <div>
          <strong>${escapeHTML(filename)}</strong>
          <p>${escapeHTML(meta)}</p>
        </div>
        <a class="button secondary" href="${escapeAttr(downloadHref)}" download>Download</a>
      </div>
      ${imageHTML}
    </article>
  `;
}

export async function uploadIssueAttachment(projectID, issueID, file, stage) {
  if (!file) {
    throw new Error("File is required");
  }
  const body = new FormData();
  body.set("stage", stage || "initial");
  body.set("file", file, file.name || "attachment");
  return apiPostForm(`${issueAPIBase(projectID)}/${encodeURIComponent(issueID)}/attachments`, body);
}
