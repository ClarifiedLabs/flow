// HTML/URL escaping and sanitization plus the shared <section> "block"
// wrapper. A dependency-free leaf imported by nearly every render module.

export function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  })[char]);
}

export function escapeAttr(value) {
  return escapeHTML(value);
}

// sanitizeURL returns a URL safe to place in an href/src, or "" if it carries a
// disallowed scheme. Control characters and whitespace are stripped first so
// obfuscated schemes (e.g. "java\tscript:") cannot slip past the whitelist.
// Only scheme-less (relative/anchor/query) URLs and http/https/mailto/tel are
// allowed; javascript:, data:, vbscript:, file: and anything else return "".
export function sanitizeURL(raw) {
  let url = String(raw == null ? "" : raw).trim();
  url = url.replace(/[\x00-\x20\x7f]+/g, "");
  if (url === "") return "";
  const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(url);
  if (scheme) {
    const name = scheme[1].toLowerCase();
    if (name === "http" || name === "https" || name === "mailto" || name === "tel") {
      return url;
    }
    return "";
  }
  return url;
}

export function markdownLink(url, innerHTML) {
  return `<a href="${escapeAttr(url)}" rel="noopener noreferrer ugc">${innerHTML}</a>`;
}

export function block(title, contents) {
  return `
    <h3>${escapeHTML(title)}</h3>
    <pre>${escapeHTML(contents || "")}</pre>
  `;
}
