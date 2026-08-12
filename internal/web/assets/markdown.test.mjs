// renderMarkdown tests: block rendering, XSS safety, inline mode, and the
// render paths that consume it.

import assert from "node:assert/strict";
import { test } from "node:test";
import { scriptContext } from "./test-helpers.mjs";

// --- renderMarkdown: block rendering correctness -------------------------------

test("renderMarkdown returns empty string for empty or blank input", async () => {
  const context = await scriptContext();
  assert.equal(context.renderMarkdown(""), "");
  assert.equal(context.renderMarkdown("   \n  \n"), "");
  assert.equal(context.renderMarkdown(null), "");
  assert.equal(context.renderMarkdown(undefined), "");
});

test("renderMarkdown wraps block output in a .md container", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("hello");
  assert.match(html, /^<div class="md">/);
  assert.match(html, /<\/div>$/);
  assert.match(html, /<p>hello<\/p>/);
});

test("renderMarkdown renders ATX headings h1 through h6", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("# Title"), /<h1>Title<\/h1>/);
  assert.match(context.renderMarkdown("## Title"), /<h2>Title<\/h2>/);
  assert.match(context.renderMarkdown("###### Title"), /<h6>Title<\/h6>/);
});

test("renderMarkdown renders bold, italic and bold-italic", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("**bold**"), /<strong>bold<\/strong>/);
  assert.match(context.renderMarkdown("__bold__"), /<strong>bold<\/strong>/);
  assert.match(context.renderMarkdown("*italic*"), /<em>italic<\/em>/);
  assert.match(context.renderMarkdown("_italic_"), /<em>italic<\/em>/);
  assert.match(context.renderMarkdown("***both***"), /<strong><em>both<\/em><\/strong>/);
});

test("renderMarkdown renders strikethrough", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("~~gone~~"), /<del>gone<\/del>/);
});

test("renderMarkdown renders inline code without parsing its contents", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("use `**not bold**` here");
  assert.match(html, /<code>\*\*not bold\*\*<\/code>/);
  assert.doesNotMatch(html, /<strong>/);
});

test("renderMarkdown renders fenced code blocks verbatim", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("```\nline1\n**raw**\n```");
  assert.match(html, /<pre><code>line1\n\*\*raw\*\*\n<\/code><\/pre>/);
  assert.doesNotMatch(html, /<strong>/);
});

test("renderMarkdown renders indented code blocks", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("    indented = code");
  assert.match(html, /<pre><code>indented = code/);
});

test("renderMarkdown renders unordered lists", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("- a\n- b");
  assert.match(html, /<ul>\s*<li>a<\/li>\s*<li>b<\/li>\s*<\/ul>/);
});

test("renderMarkdown renders ordered lists and honors a start", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("1. a\n2. b"), /<ol>\s*<li>a<\/li>\s*<li>b<\/li>\s*<\/ol>/);
  assert.match(context.renderMarkdown("3. a\n4. b"), /<ol start="3">/);
});

test("renderMarkdown renders nested lists", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("- a\n    - nested");
  assert.match(html, /<ul>\s*<li>a\s*<ul>\s*<li>nested<\/li>\s*<\/ul>\s*<\/li>\s*<\/ul>/);
});

test("renderMarkdown renders blockquotes", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("> quoted");
  assert.match(html, /<blockquote>[\s\S]*quoted[\s\S]*<\/blockquote>/);
});

test("renderMarkdown renders horizontal rules", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("---"), /<hr\s*\/?>/);
  assert.match(context.renderMarkdown("***"), /<hr\s*\/?>/);
});

test("renderMarkdown renders links with safe rel and no target", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("[flow](https://example.com)");
  assert.match(html, /<a href="https:\/\/example\.com" rel="noopener noreferrer ugc">flow<\/a>/);
  assert.doesNotMatch(html, /target=/);
});

test("renderMarkdown renders angle-bracket autolinks and bare URLs", async () => {
  const context = await scriptContext();
  assert.match(context.renderMarkdown("<https://example.com>"), /<a href="https:\/\/example\.com"[^>]*>https:\/\/example\.com<\/a>/);
  assert.match(context.renderMarkdown("see https://example.com now"), /<a href="https:\/\/example\.com"[^>]*>https:\/\/example\.com<\/a>/);
});

test("renderMarkdown renders GFM tables", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("| A | B |\n| --- | --- |\n| 1 | 2 |");
  assert.match(html, /<table>/);
  assert.match(html, /<th>A<\/th>/);
  assert.match(html, /<td>1<\/td>/);
});

test("renderMarkdown renders images with a fixed safe attribute set", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("![logo](https://example.com/a.png)");
  assert.match(html, /<img src="https:\/\/example\.com\/a\.png" alt="logo" loading="lazy"\s*\/?>/);
});

test("renderMarkdown preserves soft line breaks inside a paragraph", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("line one\nline two");
  assert.match(html, /line one<br>line two/);
});

test("renderMarkdown separates blank-line-delimited paragraphs", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("para one\n\npara two");
  assert.match(html, /<p>para one<\/p>\s*<p>para two<\/p>/);
});

// --- renderMarkdown: security / XSS -------------------------------------------

test("renderMarkdown escapes raw HTML tags instead of emitting them", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("<script>alert(1)</script>");
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
});

test("renderMarkdown does not emit a live img tag from raw HTML", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("<img src=x onerror=alert(1)>");
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /&lt;img/);
});

test("renderMarkdown drops javascript: link schemes", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("[x](javascript:alert(1))");
  assert.doesNotMatch(html, /href="javascript:/);
  assert.match(html, /x/);
});

test("renderMarkdown drops obfuscated javascript: schemes with embedded whitespace", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("[x](java\tscript:alert(1))");
  assert.doesNotMatch(html, /href="java/);
});

test("renderMarkdown drops data: image sources", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("![x](data:text/html;base64,PHN2Zz4=)");
  assert.doesNotMatch(html, /src="data:/);
});

test("renderMarkdown escapes content inside code spans that looks like a tag", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("`\"></code><script>`");
  assert.doesNotMatch(html, /<script>/);
});

test("renderMarkdown escapes ampersands and angle brackets in prose", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("a < b & c");
  assert.match(html, /a &lt; b &amp; c/);
});

// --- renderMarkdown: inline mode ---------------------------------------------

test("renderMarkdown inline mode renders inline markup without block elements", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("**done** with `sha`", { inline: true });
  assert.match(html, /<strong>done<\/strong>/);
  assert.match(html, /<code>sha<\/code>/);
  assert.doesNotMatch(html, /<(p|h1|h2|ul|ol|li|pre|blockquote|table|div)[ >]/);
});

test("renderMarkdown inline mode degrades a heading to plain inline text", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("# Title", { inline: true });
  assert.doesNotMatch(html, /<h1>/);
  assert.match(html, /Title/);
});

test("renderMarkdown inline mode degrades images to a link", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("![logo](https://example.com/a.png)", { inline: true });
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /<a href="https:\/\/example\.com\/a\.png"/);
});

test("renderMarkdown inline mode still neutralizes XSS", async () => {
  const context = await scriptContext();
  const html = context.renderMarkdown("<script>alert(1)</script>", { inline: true });
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
});

test("renderMarkdown does not overflow the stack on deeply nested blockquotes", async () => {
  const context = await scriptContext();
  assert.doesNotThrow(() => context.renderMarkdown(">".repeat(8000) + " deep"));
});

test("renderMarkdown does not overflow the stack on deeply nested lists", async () => {
  const context = await scriptContext();
  let md = "";
  for (let d = 0; d < 4000; d++) md += " ".repeat(d) + "- item\n";
  assert.doesNotThrow(() => context.renderMarkdown(md));
});

// --- markdown surface integration --------------------------------------------

test("human attention panel renders the question message as markdown", async () => {
  const context = await scriptContext();
  const task = { id: "t-alpha-0001", title: "Q" };
  const statusLog = [{ id: 7, kind: "question", message: "Pick **one**:\n- a\n- b", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(task, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  assert.match(html, /<strong>one<\/strong>/);
  assert.match(html, /<li>a<\/li>/);
});

test("check renders its details as markdown", async () => {
  const context = await scriptContext();
  const html = context.renderCheck({ name: "ci", kind: "test", details: "failed: **boom**" });
  assert.match(html, /class="md"/);
  assert.match(html, /<strong>boom<\/strong>/);
});

test("handoff summary renders its summary as inline markdown", async () => {
  const context = await scriptContext();
  const html = context.renderHandoffSummary({ present: true, valid: true, summary: "shipped `v1`" });
  assert.match(html, /<code>v1<\/code>/);
  assert.doesNotMatch(html, /<ul>|<h1>/);
});

