// Hand-rolled, dependency-light Markdown renderer. Every non-markup character
// is escaped before any tag is emitted, so source HTML is always neutralized.

import { escapeHTML, escapeAttr, sanitizeURL, markdownLink } from "./html.js";

// MARKDOWN_MAX_DEPTH caps recursion for nested blockquotes, lists and inline
// markup so adversarial or garbled input (e.g. thousands of leading ">") can
// never overflow the stack — past the cap the remaining source is emitted as
// escaped text. The renderer must always return a string, never throw.
export const MARKDOWN_MAX_DEPTH = 32;

// parseInlineMarkdown turns a run of inline markdown into a safe HTML fragment.
// Every character that is not consumed as markup is funneled through escapeHTML
// before any tag is emitted, so raw HTML in the source is always neutralized.
// options.inline collapses newlines to spaces and degrades images to links;
// otherwise newlines become <br> and images render as <img>.
export function parseInlineMarkdown(text, options = {}, depth = 0) {
  const inline = Boolean(options.inline);
  const allowImages = options.allowImages !== false;
  const src = String(text == null ? "" : text);
  if (depth > MARKDOWN_MAX_DEPTH) return escapeHTML(src);
  const n = src.length;
  let out = "";
  let buf = "";
  let i = 0;
  const flush = () => {
    if (buf) {
      out += escapeHTML(buf);
      buf = "";
    }
  };
  while (i < n) {
    const ch = src[i];
    const rest = src.slice(i);

    if (ch === "`") {
      const m = /^(`+)([\s\S]*?)\1/.exec(rest);
      if (m) {
        flush();
        out += `<code>${escapeHTML(m[2])}</code>`;
        i += m[0].length;
        continue;
      }
    }

    if (ch === "!" && src[i + 1] === "[") {
      const m = /^!\[([^\]]*)\]\(([^)]*)\)/.exec(rest);
      if (m) {
        flush();
        const alt = m[1];
        const url = sanitizeURL(m[2]);
        if (!url) {
          out += escapeHTML(alt);
        } else if (inline || !allowImages) {
          out += markdownLink(url, escapeHTML(alt));
        } else {
          out += `<img src="${escapeAttr(url)}" alt="${escapeAttr(alt)}" loading="lazy">`;
        }
        i += m[0].length;
        continue;
      }
    }

    if (ch === "[") {
      const m = /^\[([^\]]*)\]\(([^)]*)\)/.exec(rest);
      if (m) {
        flush();
        const url = sanitizeURL(m[2]);
        const innerHTML = parseInlineMarkdown(m[1], options, depth + 1);
        out += url ? markdownLink(url, innerHTML) : innerHTML;
        i += m[0].length;
        continue;
      }
    }

    if (ch === "<") {
      const m = /^<((?:https?|mailto|tel):[^>\s]+)>/i.exec(rest);
      if (m) {
        flush();
        const url = sanitizeURL(m[1]);
        out += url ? markdownLink(url, escapeHTML(m[1])) : escapeHTML(m[1]);
        i += m[0].length;
        continue;
      }
      buf += ch;
      i++;
      continue;
    }

    if (ch === "h" || ch === "H") {
      const m = /^(https?:\/\/[^\s<]+)/i.exec(rest);
      if (m) {
        let url = m[1];
        let trailing = "";
        const tm = /[.,;:!?)\]]+$/.exec(url);
        if (tm) {
          trailing = url.slice(url.length - tm[0].length);
          url = url.slice(0, url.length - tm[0].length);
        }
        const safe = sanitizeURL(url);
        flush();
        out += safe ? markdownLink(safe, escapeHTML(url)) : escapeHTML(url);
        buf += trailing;
        i += m[1].length;
        continue;
      }
    }

    if (ch === "*" || ch === "_") {
      const triple = ch === "*" ? /^\*\*\*([\s\S]+?)\*\*\*/ : /^___([\s\S]+?)___/;
      const dbl = ch === "*" ? /^\*\*([\s\S]+?)\*\*/ : /^__([\s\S]+?)__/;
      const sgl = ch === "*" ? /^\*([\s\S]+?)\*/ : /^_([\s\S]+?)_/;
      let m = triple.exec(rest);
      if (m) {
        flush();
        out += `<strong><em>${parseInlineMarkdown(m[1], options, depth + 1)}</em></strong>`;
        i += m[0].length;
        continue;
      }
      m = dbl.exec(rest);
      if (m) {
        flush();
        out += `<strong>${parseInlineMarkdown(m[1], options, depth + 1)}</strong>`;
        i += m[0].length;
        continue;
      }
      m = sgl.exec(rest);
      if (m) {
        flush();
        out += `<em>${parseInlineMarkdown(m[1], options, depth + 1)}</em>`;
        i += m[0].length;
        continue;
      }
    }

    if (ch === "~") {
      const m = /^~~([\s\S]+?)~~/.exec(rest);
      if (m) {
        flush();
        out += `<del>${parseInlineMarkdown(m[1], options, depth + 1)}</del>`;
        i += m[0].length;
        continue;
      }
    }

    if (ch === "\n") {
      flush();
      out += inline ? " " : "<br>";
      i++;
      continue;
    }

    buf += ch;
    i++;
  }
  flush();
  return out;
}

export function markdownIsBlockStart(line) {
  return (
    /^(\s*)([-*+]|\d{1,9}[.)])\s+/.test(line) ||
    /^ {0,3}>/.test(line) ||
    /^(\s*)(```+|~~~+)/.test(line) ||
    /^#{1,6}\s+/.test(line) ||
    /^ {0,3}([-*_])\s*(\1\s*){2,}$/.test(line) ||
    /^ {4,}\S/.test(line)
  );
}

export function markdownSplitTableRow(row) {
  return row.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((cell) => cell.trim());
}

export function markdownIsTableDelimiterRow(line) {
  if (line.indexOf("-") === -1) return false;
  const cells = markdownSplitTableRow(line);
  return cells.length > 0 && cells.every((cell) => /^:?-+:?$/.test(cell));
}

// renderMarkdownListItem renders one <li>'s contents tightly: leading prose is
// emitted inline (no <p>), and any nested blocks (sub-lists, code) follow.
export function renderMarkdownListItem(itemLines, options, depth = 0) {
  const lines = itemLines.slice();
  while (lines.length && lines[lines.length - 1].trim() === "") lines.pop();
  let k = 0;
  const lead = [];
  while (k < lines.length && lines[k].trim() !== "" && !markdownIsBlockStart(lines[k])) {
    lead.push(lines[k]);
    k++;
  }
  let html = lead.length ? parseInlineMarkdown(lead.join("\n"), options) : "";
  const rest = lines.slice(k);
  if (rest.length) html += renderMarkdownBlocks(rest, options, depth + 1);
  return html;
}

export function parseMarkdownList(lines, start, options, depth = 0) {
  const first = /^(\s*)([-*+]|\d{1,9}[.)])(\s+)/.exec(lines[start]);
  const baseIndent = first[1].length;
  const ordered = /\d/.test(first[2]);
  const tag = ordered ? "ol" : "ul";
  let startAttr = "";
  if (ordered) {
    const num = parseInt(first[2], 10);
    if (Number.isFinite(num) && num !== 1) startAttr = ` start="${num}"`;
  }
  const items = [];
  let i = start;
  const n = lines.length;
  while (i < n) {
    const line = lines[i];
    if (line.trim() === "") {
      let j = i + 1;
      while (j < n && lines[j].trim() === "") j++;
      if (j >= n) break;
      const nextItem = /^(\s*)([-*+]|\d{1,9}[.)])\s+/.exec(lines[j]);
      const nextIndent = /^(\s*)/.exec(lines[j])[1].length;
      if (nextItem && nextItem[1].length === baseIndent) {
        i = j;
        continue;
      }
      if (nextIndent > baseIndent) {
        i = j;
        continue;
      }
      break;
    }
    const m = /^(\s*)([-*+]|\d{1,9}[.)])(\s+)/.exec(line);
    if (!m || m[1].length !== baseIndent) break;
    const markerLen = m[0].length;
    const itemLines = [line.slice(markerLen)];
    i++;
    while (i < n) {
      const cont = lines[i];
      if (cont.trim() === "") {
        let j = i + 1;
        while (j < n && lines[j].trim() === "") j++;
        const contIndent = j < n ? /^(\s*)/.exec(lines[j])[1].length : 0;
        const contItem = j < n ? /^(\s*)([-*+]|\d{1,9}[.)])\s+/.exec(lines[j]) : null;
        if (j < n && contIndent > baseIndent && !(contItem && contItem[1].length === baseIndent)) {
          itemLines.push("");
          i++;
          continue;
        }
        break;
      }
      const contIndent = /^(\s*)/.exec(cont)[1].length;
      const contItem = /^(\s*)([-*+]|\d{1,9}[.)])\s+/.exec(cont);
      if (contItem && contItem[1].length === baseIndent) break;
      if (contIndent > baseIndent) {
        itemLines.push(cont.slice(Math.min(markerLen, contIndent)));
        i++;
        continue;
      }
      break;
    }
    items.push(`<li>${renderMarkdownListItem(itemLines, options, depth)}</li>`);
  }
  return { html: `<${tag}${startAttr}>${items.join("")}</${tag}>`, next: i };
}

// renderMarkdownBlocks is the block-level pass: it walks lines and dispatches to
// the matching construct (fenced code first so its contents are never reparsed),
// recursing for blockquotes and nested lists.
export function renderMarkdownBlocks(lines, options, depth = 0) {
  if (depth > MARKDOWN_MAX_DEPTH) {
    const text = lines.join("\n");
    return text.trim() === "" ? "" : `<p>${parseInlineMarkdown(text, options)}</p>`;
  }
  const out = [];
  let i = 0;
  const n = lines.length;
  while (i < n) {
    const line = lines[i];

    if (line.trim() === "") {
      i++;
      continue;
    }

    const fence = /^(\s*)(```+|~~~+)(.*)$/.exec(line);
    if (fence) {
      const marker = fence[2][0];
      const len = fence[2].length;
      const closeRe = new RegExp("^\\s*\\" + marker + "{" + len + ",}\\s*$");
      const body = [];
      i++;
      while (i < n && !closeRe.test(lines[i])) {
        body.push(lines[i]);
        i++;
      }
      if (i < n) i++;
      out.push(`<pre><code>${escapeHTML(body.join("\n") + (body.length ? "\n" : ""))}</code></pre>`);
      continue;
    }

    const heading = /^(#{1,6})\s+(.*?)\s*#*\s*$/.exec(line);
    if (heading) {
      const level = heading[1].length;
      out.push(`<h${level}>${parseInlineMarkdown(heading[2], options)}</h${level}>`);
      i++;
      continue;
    }

    if (/^ {0,3}([-*_])\s*(\1\s*){2,}$/.test(line)) {
      out.push("<hr>");
      i++;
      continue;
    }

    if (/^ {0,3}>/.test(line)) {
      const quoteLines = [];
      while (i < n && /^ {0,3}>/.test(lines[i])) {
        quoteLines.push(lines[i].replace(/^ {0,3}>\s?/, ""));
        i++;
      }
      out.push(`<blockquote>${renderMarkdownBlocks(quoteLines, options, depth + 1)}</blockquote>`);
      continue;
    }

    if (line.indexOf("|") !== -1 && i + 1 < n && markdownIsTableDelimiterRow(lines[i + 1])) {
      const header = markdownSplitTableRow(line);
      i += 2;
      const rows = [];
      while (i < n && lines[i].trim() !== "" && lines[i].indexOf("|") !== -1) {
        rows.push(markdownSplitTableRow(lines[i]));
        i++;
      }
      let table = "<table><thead><tr>";
      header.forEach((cell) => {
        table += `<th>${parseInlineMarkdown(cell, options)}</th>`;
      });
      table += "</tr></thead>";
      if (rows.length) {
        table += "<tbody>";
        rows.forEach((row) => {
          table += "<tr>";
          for (let c = 0; c < header.length; c++) {
            table += `<td>${parseInlineMarkdown(row[c] || "", options)}</td>`;
          }
          table += "</tr>";
        });
        table += "</tbody>";
      }
      table += "</table>";
      out.push(table);
      continue;
    }

    if (/^(\s*)([-*+]|\d{1,9}[.)])\s+/.test(line)) {
      const list = parseMarkdownList(lines, i, options, depth);
      out.push(list.html);
      i = list.next;
      continue;
    }

    if (/^ {4,}\S/.test(line) || /^\t/.test(line)) {
      const codeLines = [];
      while (i < n) {
        if (lines[i].trim() === "") {
          let j = i + 1;
          while (j < n && lines[j].trim() === "") j++;
          if (j < n && (/^ {4}/.test(lines[j]) || /^\t/.test(lines[j]))) {
            codeLines.push("");
            i++;
            continue;
          }
          break;
        }
        if (!/^ {4}/.test(lines[i]) && !/^\t/.test(lines[i])) break;
        codeLines.push(lines[i].replace(/^( {4}|\t)/, ""));
        i++;
      }
      out.push(`<pre><code>${escapeHTML(codeLines.join("\n") + (codeLines.length ? "\n" : ""))}</code></pre>`);
      continue;
    }

    const para = [];
    while (
      i < n &&
      lines[i].trim() !== "" &&
      !markdownIsBlockStart(lines[i]) &&
      !(lines[i].indexOf("|") !== -1 && i + 1 < n && markdownIsTableDelimiterRow(lines[i + 1]))
    ) {
      para.push(lines[i]);
      i++;
    }
    if (para.length) {
      out.push(`<p>${parseInlineMarkdown(para.join("\n"), options)}</p>`);
    } else {
      i++;
    }
  }
  return out.join("");
}

// renderMarkdown is the reusable entry point: it converts markdown source into a
// safe HTML-subset string. Block mode wraps output in a <div class="md">; inline
// mode (options.inline) returns a bare fragment with no block elements, for
// tight single-line contexts like board cards and timeline rows. The renderer
// only ever emits a fixed whitelist of tags and routes all text/URLs through
// escapeHTML/escapeAttr/sanitizeURL, so it is XSS-safe by construction.
export function renderMarkdown(src, options = {}) {
  const normalized = String(src == null ? "" : src).replace(/\r\n?/g, "\n");
  if (options.inline) {
    return parseInlineMarkdown(normalized, { inline: true, allowImages: options.allowImages });
  }
  if (normalized.trim() === "") return "";
  const html = renderMarkdownBlocks(normalized.split("\n"), options);
  if (html.trim() === "") return "";
  return `<div class="${escapeAttr(options.className || "md")}">${html}</div>`;
}
