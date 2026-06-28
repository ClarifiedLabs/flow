import assert from "node:assert/strict";
import { test } from "node:test";

const ISSUE_AGENT_DEFAULTS_STORAGE_KEY = "flow.ui.issueAgentDefaults.v1";
const DIFF_MODE_STORAGE_KEY = "flow.ui.diffMode";

test("terminal click from a card opens a modal-sized frame", async () => {
  const terminalHost = new InlineDOMElement("article");
  const content = new InlineDOMElement("main");
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.terminal = "s-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v1/sessions/s-0001/terminal-login?token=abc" } }),
      });
    },
  });

  const flowApp = new context.FlowApp();
  flowApp.querySelectorAll = (selector) => (selector === "[data-terminal]" ? [terminalButton] : []);
  flowApp.querySelector = (selector) => {
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return new InlineDOMElement();
  };
  flowApp.bindIssueActions(async () => {});
  await terminalButton.listeners.get("click")();

  assert.equal(terminalHost.children.length, 0);
  assert.equal(content.children.length, 1);
  assert.equal(content.children[0].className, "terminal-modal-layer");
  assert.equal(content.children[0].dataset.terminalModalLayer, "true");
  assert.match(content.children[0].innerHTML, /class="terminal-modal"/);
  assert.match(content.children[0].innerHTML, /Session terminal/);
  assert.match(content.children[0].innerHTML, /class="terminal-frame"/);
  assert.match(content.children[0].innerHTML, /src="\/v1\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /data-terminal-popout="\/v1\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /Pop out/);
  assert.match(content.children[0].innerHTML, /Shift\+drag to select/);
  assert.equal(status.textContent, "");
  assert.equal(fetchCalls[0].path, "/ui/api/v1/sessions/s-0001/terminal-token");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
});

test("board polling keeps an open card terminal modal mounted", async () => {
  const main = new InlineDOMElement("main");
  main.className = "main";
  const content = new RepaintingInlineDOMElement("section");
  content.className = "content";
  main.appendChild(content);
  const terminalHost = new InlineDOMElement("article");
  terminalHost.className = "card";
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.terminal = "s-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const title = { textContent: "" };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({
    location: { pathname: "/ui/board" },
    setTimeout() {
      return 1;
    },
    clearTimeout() {},
  }, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v1/sessions/s-0001/terminal-token") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ access: { login_path: "/v1/sessions/s-0001/terminal-login?token=abc" } }),
        });
      }
      if (path === "/ui/api/v1/board") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            boards: [{
              project_id: "p-alpha",
              project_name: "alpha",
              board: {},
              lane_states: {},
              issue_cards: {},
            }],
          }),
        });
      }
      return Promise.resolve({
        ok: false,
        json: () => Promise.resolve({ error: { message: `unexpected request ${path}` } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.projects = [];
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".main") return main;
    if (selector === ".content") return content;
    if (selector === ".status") return status;
    if (selector === "h1") return title;
    return null;
  };

  await app.openInlineTerminal(terminalButton, "session", "s-0001");
  const modal = main.children.find((child) => child.dataset?.terminalModalLayer === "true");
  assert.ok(modal);
  assert.match(modal.innerHTML, /class="terminal-frame"/);
  assert.match(modal.innerHTML, /Shift\+drag to select/);

  await app.load({ fromPoll: true });

  assert.equal(main.children.includes(modal), true);
  assert.match(modal.innerHTML, /src="\/v1\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(content.innerHTML, /class="board"/);
  assert.deepEqual(fetchCalls.map((call) => call.path), [
    "/ui/api/v1/sessions/s-0001/terminal-token",
    "/ui/api/v1/board",
    "/ui/api/v1/done?limit=20",
  ]);
});

test("terminal click closes an open card terminal modal without refreshing the token", async () => {
  const terminalHost = new InlineDOMElement("article");
  const content = new InlineDOMElement("main");
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.terminal = "s-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v1/sessions/s-0001/terminal-login?token=abc" } }),
      });
    },
  });

  const flowApp = new context.FlowApp();
  flowApp.querySelectorAll = (selector) => (selector === "[data-terminal]" ? [terminalButton] : []);
  flowApp.querySelector = (selector) => {
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return new InlineDOMElement();
  };
  flowApp.bindIssueActions(async () => {});
  await terminalButton.listeners.get("click")();
  assert.equal(content.children.length, 1);

  await terminalButton.listeners.get("click")();

  assert.equal(terminalHost.children.length, 0);
  assert.equal(content.children.length, 0);
  assert.equal(fetchCalls.length, 1);
  assert.equal(status.textContent, "");
});

test("job terminal click from a card opens a modal-sized frame", async () => {
  const terminalHost = new InlineDOMElement("article");
  const content = new InlineDOMElement("main");
  const terminalButton = new InlineDOMElement("button");
  terminalButton.dataset.jobTerminal = "j-0001";
  terminalButton.closest = (selector) => {
    if (selector === "tr") return null;
    if (selector === ".card, .feed-item") return terminalHost;
    if (selector === ".card, .feed-item, .detail") return terminalHost;
    return null;
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    document: inlineDocument(),
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v1/jobs/j-0001/terminal-login?token=abc" } }),
      });
    },
  });
  const flowApp = new context.FlowApp();
  flowApp.querySelectorAll = (selector) => (selector === "[data-job-terminal]" ? [terminalButton] : []);
  flowApp.querySelector = (selector) => {
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return new InlineDOMElement();
  };
  flowApp.bindIssueActions(async () => {});
  await terminalButton.listeners.get("click")();

  assert.equal(terminalHost.children.length, 0);
  assert.equal(content.children.length, 1);
  assert.match(content.children[0].innerHTML, /Job terminal/);
  assert.match(content.children[0].innerHTML, /class="terminal-frame"/);
  assert.match(content.children[0].innerHTML, /src="\/v1\/jobs\/j-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /data-terminal-popout="\/v1\/jobs\/j-0001\/terminal-login\?token=abc"/);
  assert.match(content.children[0].innerHTML, /Pop out/);
  assert.match(content.children[0].innerHTML, /Shift\+drag to select/);
  assert.equal(status.textContent, "");
  assert.equal(fetchCalls[0].path, "/ui/api/v1/jobs/j-0001/terminal-token");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
});

test("terminal buttons in table rows expand a full-width inline terminal row", async () => {
  const context = await scriptContext({}, { document: inlineDocument() });
  const row = new InlineDOMElement("tr");
  row.cells = [new InlineDOMElement("td"), new InlineDOMElement("td"), new InlineDOMElement("td")];
  const terminalButton = new InlineDOMElement("button");
  terminalButton.closest = (selector) => (selector === "tr" ? row : null);
  const app = new context.FlowApp();
  app.querySelector = () => new InlineDOMElement();

  const mount = context.inlineTerminalMount(terminalButton, app);
  const terminalRow = row.nextElementSibling;

  assert.equal(terminalRow.className, "inline-terminal-row");
  assert.equal(terminalRow.dataset.inlineTerminalRow, "true");
  assert.equal(terminalRow.children[0].colSpan, 3);
  assert.equal(terminalRow.children[0].children[0], mount);
  assert.equal(context.inlineTerminalMount(terminalButton, app), mount);
});

test("inline terminal renders a Hide button next to the pop-out button", async () => {
  const context = await scriptContext();

  const html = context.renderInlineTerminal(
    "session",
    "s-0001",
    `<iframe class="terminal-frame"></iframe>`,
    "/v1/sessions/s-0001/terminal-login?token=abc",
  );

  assert.match(html, /data-terminal-popout="\/v1\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(html, /data-terminal-hide/);
  assert.match(html, />Hide</);
  const hideIndex = html.indexOf("data-terminal-hide");
  const popOutIndex = html.indexOf("data-terminal-popout");
  assert.ok(popOutIndex >= 0 && hideIndex > popOutIndex, "Hide button follows the pop-out button");
});

test("terminal modal renders a Hide button next to the pop-out button", async () => {
  const context = await scriptContext();

  const html = context.renderTerminalDialog(
    "session",
    "s-0001",
    `<iframe class="terminal-frame"></iframe>`,
    "/v1/sessions/s-0001/terminal-login?token=abc",
  );

  assert.match(html, /data-terminal-popout="\/v1\/sessions\/s-0001\/terminal-login\?token=abc"/);
  assert.match(html, /data-terminal-close/);
  assert.match(html, />Hide</);
  assert.doesNotMatch(html, />Close</);
  const hideIndex = html.indexOf("data-terminal-close");
  const popOutIndex = html.indexOf("data-terminal-popout");
  assert.ok(popOutIndex >= 0 && hideIndex > popOutIndex, "Hide button follows the pop-out button");
});

test("inline terminal Hide button removes the terminal mount", async () => {
  const context = await scriptContext({}, { document: inlineDocument() });
  const mount = new InlineDOMElement("div");
  mount.dataset.inlineTerminal = "true";
  const removed = [];
  mount.remove = () => removed.push(mount);
  const hideButton = new InlineDOMElement("button");
  hideButton.closest = (selector) => (selector === '[data-inline-terminal="true"]' ? mount : null);

  assert.equal(context.hideInlineTerminal(hideButton), true);
  assert.deepEqual(removed, [mount]);
});

test("terminal route embeds owner-authenticated login path", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/sessions/s-0001/terminal" },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v1/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [] }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ access: { login_path: "/v1/sessions/s-0001/terminal-login?token=abc123" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  assert.equal(title.textContent, "Terminal");
  assert.equal(status.textContent, "");
  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects");
  assert.equal(fetchCalls[1].path, "/ui/api/v1/sessions/s-0001/terminal-token");
  assert.equal(fetchCalls[1].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.match(content.innerHTML, /class="detail terminal-detail"/);
  assert.match(content.innerHTML, /class="terminal-frame"/);
  assert.match(content.innerHTML, /src="\/v1\/sessions\/s-0001\/terminal-login\?token=abc123"/);
  assert.match(content.innerHTML, /data-terminal-popout="\/v1\/sessions\/s-0001\/terminal-login\?token=abc123"/);
  assert.match(content.innerHTML, /Shift\+drag to select/);
});

test("console page offers shell harness and posts selected harness", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/console", search: "" },
  }, {
    URLSearchParams,
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v1/harnesses") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            agents: [{ name: "harness", display_name: "Harness" }],
            consoles: [
              { name: "claude", display_name: "Claude" },
              { name: "harness", display_name: "Harness" },
              { name: "shell", display_name: "Shell" },
            ],
          }),
        });
      }
      if (path === "/ui/api/v1/projects/p-alpha/console" && options.method === "POST") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ active: true }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          active: false,
          project_id: "p-alpha",
          project_name: "Alpha",
        }),
      });
    },
  });
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "Alpha" }];
  let loads = 0;
  app.load = async () => {
    loads += 1;
  };
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { value: "shell", textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.renderConsole();
  assert.match(content.innerHTML, /<option value="claude" selected>Claude<\/option>/);
  assert.doesNotMatch(content.innerHTML, /<option value="codex">Codex<\/option>/);
  assert.match(content.innerHTML, /<option value="harness">Harness<\/option>/);
  assert.match(content.innerHTML, /<option value="shell">Shell<\/option>/);

  await app.startConsole("p-alpha", "shell");
  const post = fetchCalls.find((call) => call.path === "/ui/api/v1/projects/p-alpha/console" && call.options.method === "POST");
  assert.equal(post.options.headers["X-Flow-CSRF"], "csrf-token");
  assert.equal(JSON.parse(post.options.body).harness, "shell");
  assert.equal(loads, 1);
  assert.equal(status.textContent, "console starting");
});

test("terminal pop out opens a popup-style window", async () => {
  const opened = [];
  const context = await scriptContext({
    innerWidth: 1600,
    innerHeight: 1000,
    screen: { availWidth: 1600, availHeight: 1000 },
    open(url, target, features) {
      opened.push({ url, target, features });
      return {};
    },
  });

  context.openTerminalWindow("/v1/sessions/s-0001/terminal-login?token=abc123");

  assert.deepEqual(opened, [{
    url: "/v1/sessions/s-0001/terminal-login?token=abc123",
    target: "_blank",
    features: "popup=yes,noopener,noreferrer,width=1400,height=880,left=100,top=60,resizable=yes,scrollbars=yes",
  }]);
});

test("terminal route is recognized without polling", async () => {
  const context = await scriptContext();

  assert.equal(context.terminalSessionIDForPath("/ui/sessions/s-0001/terminal"), "s-0001");
  assert.equal(context.terminalSessionIDForPath("/ui/sessions/bad%ZZ/terminal"), "");
  assert.equal(context.pollConfigForPath("/ui/sessions/s-0001/terminal"), null);
});

test("theme switcher defaults to system without a stored override", async () => {
  const harness = await themeShellHarness();
  harness.rootAttributes.set("data-theme", "dark");

  harness.app.renderShell();

  assert.deepEqual(harness.pressedThemes(), ["system"]);
  assert.equal(harness.themeButtons.light.attributes.get("aria-pressed"), "false");
  assert.equal(harness.themeButtons.dark.attributes.get("aria-pressed"), "false");
  assert.equal(harness.rootAttributes.has("data-theme"), false);
  assert.match(harness.app.innerHTML, /data-theme-option/);
});

test("shell keeps the terminal-style brand and New Issue action", async () => {
  const harness = await themeShellHarness();

  harness.app.renderShell();

  assert.match(harness.app.innerHTML, /<p class="brand">flow<span class="brand-cursor">_<\/span><\/p>/);
  assert.match(harness.app.innerHTML, /<button class="button" data-action="new-issue">New Issue<\/button>/);
});

test("theme switcher applies stored overrides and persists user choices", async () => {
  const harness = await themeShellHarness("dark");

  harness.app.renderShell();

  assert.deepEqual(harness.pressedThemes(), ["dark"]);
  assert.equal(harness.rootAttributes.get("data-theme"), "dark");

  harness.themeButtons.light.listeners.get("click")();
  assert.equal(harness.storage.get("flow.ui.theme"), "light");
  assert.equal(harness.rootAttributes.get("data-theme"), "light");
  assert.deepEqual(harness.pressedThemes(), ["light"]);

  harness.themeButtons.system.listeners.get("click")();
  assert.equal(harness.storage.has("flow.ui.theme"), false);
  assert.equal(harness.rootAttributes.has("data-theme"), false);
  assert.deepEqual(harness.pressedThemes(), ["system"]);
});

test("job attach action fetches and displays the tmux attach command", async () => {
  let clickHandler;
  const attachButton = {
    dataset: { jobAttach: "j-0001" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          attach: {
            tmux_session: "flow-j-0001",
            command: ["tmux", "attach-session", "-t", "flow-j-0001"],
          },
        }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-job-attach]" ? [attachButton] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  app.bindIssueActions(async () => {});

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/jobs/j-0001/attach");
  assert.equal(fetchCalls[0].options.method, "GET");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.equal(status.textContent, "tmux attach-session -t flow-j-0001");
});

test("issue save submits patch payload and refreshes", async () => {
  const harness = await issueSaveHarness({
    agentArgs: `--model "sonnet latest"`,
  });
  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v1/issues/i-0001");
  assert.equal(harness.fetchCalls[0].options.method, "PATCH");
  assert.equal(harness.fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), {
    title: "Updated issue",
    body: "New body",
    acceptance_criteria: "New criteria",
    priority: 4,
    requires_human_review: false,
    auto_merge: true,
    agent_harness: "claude",
    harness_args: {
      codex: [],
      claude: [`--model "sonnet latest"`],
      harness: [],
    },
  });
  assert.equal(harness.refreshed(), true);
  assert.equal(harness.status.textContent, "");
});

test("issue save sends malformed quoted agent args to the server", async () => {
  const harness = await issueSaveHarness({ agentHarness: "harness", agentArgs: `--model "unterminated` });
  await harness.submit();

  assert.equal(harness.fetchCalls.length, 1);
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body).harness_args, {
    codex: [],
    claude: [],
    harness: [`--model "unterminated`],
  });
  assert.equal(harness.refreshed(), true);
  assert.equal(harness.status.textContent, "");
});

test("issue form renders harness model controls from saved args", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.harnesses = {
    agents: [{
      name: "harness",
      display_name: "Harness",
      models: [{
        target_id: "anthropic:claude-opus-4-8",
        display_name: "claude-opus-4-8",
        provider_label: "anthropic",
        model_label: "claude-opus-4-8",
        context_window: 1000000,
        reasoning: { supported: true, options: [{ type: "effort", values: ["low", "high"] }] },
      }],
    }],
  };

  const html = app.renderIssueForm({
    title: "Harness issue",
    agent_harness: "harness",
    harness_args: {
      harness: ["--provider", "anthropic", "--model", "claude-opus-4-8", "--reasoning-effort", "high", "--label", "fast"],
    },
  });

  assert.match(html, /data-harness-model-fields/);
  assert.doesNotMatch(html, /name="harness_provider"/);
  assert.match(html, /<option value="anthropic:claude-opus-4-8" selected>claude-opus-4-8 \(1M ctx\)<\/option>/);
  assert.match(html, /<option value="effort" selected>Effort<\/option>/);
  assert.match(html, /<option value="high" selected>high<\/option>/);
  assert.match(html, /<textarea name="agent_args" rows="2"[^>]*>--label fast<\/textarea>/);
});

test("issue form renders one model picker with provider labels when needed", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.harnesses = {
    agents: [{
      name: "harness",
      display_name: "Harness",
      models: [{
        provider_id: "anthropic",
        provider_name: "Anthropic",
        model_id: "claude-opus-4-8",
        qualified_id: "anthropic:claude-opus-4-8",
        model_name: "Claude Opus 4.8",
        reasoning: { supported: true, options: [{ type: "effort", values: ["low", "high"] }] },
      }, {
        provider_id: "google",
        provider_name: "Google",
        model_id: "gemini-2.5-flash",
        qualified_id: "google:gemini-2.5-flash",
        model_name: "Gemini 2.5 Flash",
        reasoning: { supported: true, options: [{ type: "budget_tokens", min: 0, max: 24576 }] },
      }],
    }],
  };

  const html = app.renderIssueForm({
    title: "Harness issue",
    agent_harness: "harness",
    harness_args: { harness: ["--model", "google:gemini-2.5-flash"] },
  });

  const modelOptions = html.match(/<select name="harness_model">([\s\S]*?)<\/select>/)?.[1] || "";
  assert.doesNotMatch(html, /name="harness_provider"/);
  assert.match(modelOptions, /<option value="anthropic:claude-opus-4-8"[^>]*>Anthropic \/ Claude Opus 4\.8<\/option>/);
  assert.match(modelOptions, /<option value="google:gemini-2.5-flash" selected>Google \/ Gemini 2\.5 Flash<\/option>/);
});

test("issue form renders saved args as shell-style strings", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();

  const html = app.renderIssueForm({
    title: "Quoted args",
    agent_harness: "claude",
    harness_args: {
      claude: ["--label", "fast mode", "it's ok", "path/ok"],
    },
  });

  assert.match(html, /<textarea name="agent_args" rows="2"[^>]*>--label &#39;fast mode&#39; &#39;it&#39;\\&#39;&#39;s ok&#39; path\/ok<\/textarea>/);
});

test("issue save generates harness model args from controls", async () => {
  const harness = await issueSaveHarness({
    agentHarness: "harness",
    agentArgs: `--label "fast mode"`,
    harnesses: {
      agents: [{
        name: "harness",
        display_name: "Harness",
        models: [{
          provider_id: "google",
          provider_name: "google",
          model_id: "gemini-2.5-flash",
          qualified_id: "google:gemini-2.5-flash",
          model_name: "gemini-2.5-flash",
          reasoning: { supported: true, options: [{ type: "budget_tokens", min: 0, max: 24576 }] },
        }],
      }],
    },
    harnessProvider: "google",
    harnessModel: "google:gemini-2.5-flash",
    harnessReasoningMode: "budget",
    harnessReasoningBudget: "2048",
  });
  await harness.submit();

  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body).harness_args.harness, [
    "--model",
    "google:gemini-2.5-flash",
    "--reasoning-budget-tokens",
    "2048",
    `--label "fast mode"`,
  ]);
  assert.equal(harness.status.textContent, "");
});

test("issue save validates harness reasoning budget range", async () => {
  const harness = await issueSaveHarness({
    agentHarness: "harness",
    harnesses: {
      agents: [{
        name: "harness",
        display_name: "Harness",
        models: [{
          provider_id: "google",
          model_id: "gemini-2.5-flash",
          qualified_id: "google:gemini-2.5-flash",
          reasoning: { supported: true, options: [{ type: "budget_tokens", min: 0, max: 24576 }] },
        }],
      }],
    },
    harnessProvider: "google",
    harnessModel: "google:gemini-2.5-flash",
    harnessReasoningMode: "budget",
    harnessReasoningBudget: "30000",
  });
  await harness.submit();

  assert.equal(harness.fetchCalls.length, 0);
  assert.equal(harness.status.textContent, "Reasoning budget must be at most 24576");
});

test("issue save does not submit invalid form or priority", async () => {
  const invalidForm = await issueSaveHarness({ valid: false });
  await invalidForm.submit();
  assert.equal(invalidForm.fetchCalls.length, 0);
  assert.equal(invalidForm.refreshed(), false);

  const invalidPriority = await issueSaveHarness({ priority: "4.5" });
  await invalidPriority.submit();
  assert.equal(invalidPriority.fetchCalls.length, 0);
  assert.equal(invalidPriority.refreshed(), false);
  assert.equal(invalidPriority.status.textContent, "Priority must be a non-negative integer");
});

test("issue save surfaces patch failures", async () => {
  const harness = await issueSaveHarness({ fetchOK: false, errorMessage: "issue title is required" });
  await harness.submit();

  assert.equal(harness.fetchCalls.length, 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "issue title is required");
});

test("issue attachment form uploads stage file and refreshes", async () => {
  let submitHandler;
  const file = { name: "review.png" };
  const form = {
    dataset: { issue: "i-0001", project: "p-demo" },
    elements: {
      stage: { value: "reviewer" },
      file: { files: [file] },
    },
    resetCalled: false,
    reportValidity() {
      return true;
    },
    reset() {
      this.resetCalled = true;
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
  };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ attachment: { id: "att-0001" } }),
      });
    },
    FormData: class {
      constructor() {
        this.fields = [];
      }
      set(name, value, filename) {
        this.fields.push({ name, value, filename });
      }
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-attachment-form]" ? [form] : []);
  app.querySelector = () => ({ textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await submitHandler({ preventDefault() {} });

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/attachments");
  assert.deepEqual(fetchCalls[0].options.body.fields, [
    { name: "stage", value: "reviewer", filename: undefined },
    { name: "file", value: file, filename: "review.png" },
  ]);
  assert.equal(form.resetCalled, true);
  assert.equal(refreshed, true);
});

test("new issue action navigates to blank issue form without posting", async () => {
  const harness = await createIssueHarness();

  await harness.create();

  assert.equal(harness.fetchCalls.length, 0);
  assert.equal(harness.pushedPath(), "/ui/issues/new");
  assert.equal(harness.loads(), 1);
  assert.equal(harness.status.textContent, "");
});

test("new issue route renders project-scoped blank form without fetching an issue", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = await scriptContext({
    location: { pathname: "/ui/issues/new" },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v1/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            projects: [
              { id: "p-alpha", name: "alpha" },
              { id: "p-beta", name: "beta" },
            ],
          }),
        });
      }
      if (path === "/ui/api/v1/harnesses") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            agents: [
              { name: "codex", display_name: "Codex", default_args: ["--model", "gpt-5"] },
              { name: "harness", display_name: "Harness" },
            ],
            consoles: [],
          }),
        });
      }
      throw new Error("new issue route should not fetch before submission");
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.renderProjectPicker = () => {};
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  await app.load();

  assert.equal(fetchCalls.length, 2);
  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects");
  assert.equal(fetchCalls[1].path, "/ui/api/v1/harnesses");
  assert.equal(title.textContent, "New Issue");
  assert.match(content.innerHTML, /data-issue-form-mode="create"/);
  assert.match(content.innerHTML, /<span>Project<\/span>/);
  assert.match(content.innerHTML, /<option value="p-alpha"/);
  assert.match(content.innerHTML, /<option value="p-beta"/);
  assert.match(content.innerHTML, /<input name="title" value="" required>/);
  assert.match(content.innerHTML, /<select name="agent_harness">/);
  assert.match(content.innerHTML, /<option value="codex" selected>Codex<\/option>/);
  assert.match(content.innerHTML, /<option value="harness">Harness<\/option>/);
  assert.match(content.innerHTML, /<textarea name="agent_args" rows="2"[^>]*><\/textarea>/);
  assert.doesNotMatch(content.innerHTML, /name="codex_args"/);
  assert.doesNotMatch(content.innerHTML, /name="claude_args"/);
  assert.doesNotMatch(content.innerHTML, /name="harness_args"/);
  assert.match(content.innerHTML, /Coordinator defaults: --model gpt-5/);
  assert.match(content.innerHTML, /<input name="plan_mode" type="checkbox" >/);
  assert.match(content.innerHTML, /<input name="queue_issue" type="checkbox" checked>/);
  assert.match(content.innerHTML, /<button class="button" type="submit">Create<\/button>/);
  assert.match(content.innerHTML, /<button class="button secondary" type="button" data-save-agent-defaults>Save as defaults<\/button>/);
  assert.equal(status.textContent, "");
});

test("new issue route renders model controls when only Harness agent is enabled", async () => {
  const harness = await newIssueRouteHarness({
    harnesses: {
      agents: [{
        name: "harness",
        display_name: "Harness",
        models: [{
          provider_id: "anthropic",
          provider_name: "Anthropic",
          model_id: "claude-opus-4-8",
          qualified_id: "anthropic:claude-opus-4-8",
          model_name: "Claude Opus 4.8",
          reasoning: { supported: true, options: [{ type: "effort", values: ["low", "high"] }] },
        }],
      }],
      consoles: [],
    },
  });

  await harness.app.load();

  assert.match(harness.content.innerHTML, /<option value="harness" selected>Harness<\/option>/);
  assert.match(harness.content.innerHTML, /data-harness-model-fields/);
  assert.doesNotMatch(harness.content.innerHTML, /data-harness-model-fields[^>]* hidden/);
  assert.doesNotMatch(harness.content.innerHTML, /name="harness_provider"/);
  assert.match(harness.content.innerHTML, /<option value="anthropic:claude-opus-4-8"[^>]*>Claude Opus 4\.8<\/option>/);
  assert.match(harness.content.innerHTML, /<select name="harness_reasoning_mode">/);
  assert.match(harness.content.innerHTML, /<option value="default" selected>Provider default<\/option>/);
  assert.equal(harness.status.textContent, "");
});

test("new issue route renders saved agent defaults from localStorage", async () => {
  const harness = await newIssueRouteHarness({
    storedDefaults: {
      version: 1,
      agent_harness: "harness",
      harness_args: {
        codex: ["--codex-fast"],
        claude: ["--claude-sonnet"],
        harness: ["--provider", "anthropic", "--model", "claude-opus-4-8", "--reasoning-effort", "high", "--label", "fast"],
      },
    },
  });

  await harness.app.load();

  assert.match(harness.content.innerHTML, /<option value="harness" selected>Harness<\/option>/);
  assert.doesNotMatch(harness.content.innerHTML, /name="harness_provider"/);
  assert.match(harness.content.innerHTML, /<option value="anthropic:claude-opus-4-8" selected>Claude Opus 4\.8<\/option>/);
  assert.match(harness.content.innerHTML, /<option value="effort" selected>Effort<\/option>/);
  assert.match(harness.content.innerHTML, /<option value="high" selected>high<\/option>/);
  assert.match(harness.content.innerHTML, /<textarea name="agent_args" rows="2"[^>]*>--label fast<\/textarea>/);
  assert.match(harness.content.innerHTML, /<input name="priority" type="number" min="0" step="1" value="0">/);
  assert.match(harness.content.innerHTML, /<input name="requires_human_review" type="checkbox" checked>/);
  assert.match(harness.content.innerHTML, /<input name="auto_merge" type="checkbox" >/);
  assert.equal(harness.status.textContent, "");
});

test("new issue route ignores corrupt or unavailable saved agent defaults", async () => {
  const cases = [
    { storedDefaultsRaw: "not-json" },
    {
      localStorage: {
        getItem() {
          throw new Error("storage unavailable");
        },
        setItem() {},
        removeItem() {},
      },
    },
  ];
  for (const options of cases) {
    const harness = await newIssueRouteHarness(options);

    await harness.app.load();

    assert.match(harness.content.innerHTML, /<option value="codex" selected>Codex<\/option>/);
    assert.match(harness.content.innerHTML, /<textarea name="agent_args" rows="2"[^>]*><\/textarea>/);
    assert.match(harness.content.innerHTML, /Coordinator defaults: --model gpt-5/);
    assert.equal(harness.status.textContent, "");
  }
});

test("new issue form shows the project field even with one project", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  app.projects = [{ id: "p-alpha", name: "alpha" }];

  const html = app.renderIssueForm({ title: "", priority: 0 }, { mode: "create", submitLabel: "Create" });

  assert.match(html, /<span>Project<\/span>/);
  assert.match(html, /<select name="project" required>/);
  assert.match(html, /<option value="p-alpha" selected>alpha<\/option>/);
  assert.ok(html.indexOf('class="issue-field-project"') < html.indexOf('class="issue-field-priority"'));
  assert.ok(html.indexOf('class="issue-field-priority"') < html.indexOf('class="issue-field-agent"'));
  assert.ok(html.indexOf('class="issue-field-agent"') < html.indexOf('class="issue-field-title wide"'));
});

test("new issue form submits queued create payload then navigates to created issue", async () => {
  const harness = await issueSaveHarness({ mode: "create", title: "  Browser issue  ", planMode: true });

  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues");
  assert.equal(harness.fetchCalls[0].options.method, "POST");
  assert.equal(harness.fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), {
    title: "Browser issue",
    body: "New body",
    acceptance_criteria: "New criteria",
    priority: 4,
    requires_human_review: false,
    auto_merge: true,
    plan_mode: true,
    agent_harness: "claude",
    harness_args: {
      codex: [],
      claude: [],
      harness: [],
    },
    schedule_state: "up_next",
  });
  assert.equal(harness.pushedPath(), "/ui/projects/p-demo/issues/i-0001");
  assert.equal(harness.loads(), 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "");
  assert.deepEqual(JSON.parse(harness.storage.get(ISSUE_AGENT_DEFAULTS_STORAGE_KEY)), {
    version: 1,
    agent_harness: "claude",
    harness_args: {
      codex: [],
      claude: [],
      harness: [],
    },
  });
});

test("new issue save defaults button writes agent defaults without posting", async () => {
  const harness = await issueSaveHarness({
    mode: "create",
    agentHarness: "harness",
    agentArgs: `--label "fast mode"`,
    agentArgsValues: {
      codex: "--codex-fast",
      claude: "--claude-sonnet",
      harness: "--old-harness-arg",
    },
    harnesses: {
      agents: [{
        name: "harness",
        display_name: "Harness",
        models: [{
          provider_id: "google",
          provider_name: "Google",
          model_id: "gemini-2.5-flash",
          qualified_id: "google:gemini-2.5-flash",
          model_name: "Gemini 2.5 Flash",
          reasoning: { supported: true, options: [{ type: "budget_tokens", min: 0, max: 24576 }] },
        }],
      }],
    },
    harnessProvider: "google",
    harnessModel: "google:gemini-2.5-flash",
    harnessReasoningMode: "budget",
    harnessReasoningBudget: "2048",
  });

  await harness.saveDefaults();

  assert.equal(harness.fetchCalls.length, 0);
  assert.equal(harness.pushedPath(), "");
  assert.equal(harness.loads(), 0);
  assert.equal(harness.status.textContent, "Agent defaults saved");
  assert.deepEqual(JSON.parse(harness.storage.get(ISSUE_AGENT_DEFAULTS_STORAGE_KEY)), {
    version: 1,
    agent_harness: "harness",
    harness_args: {
      codex: ["--codex-fast"],
      claude: ["--claude-sonnet"],
      harness: ["--model", "google:gemini-2.5-flash", "--reasoning-budget-tokens", "2048", `--label "fast mode"`],
    },
  });
});

test("new issue save defaults button preserves stored defaults on invalid reasoning", async () => {
  const originalDefaults = {
    version: 1,
    agent_harness: "claude",
    harness_args: {
      codex: [],
      claude: ["--existing"],
      harness: [],
    },
  };
  const harness = await issueSaveHarness({
    mode: "create",
    agentHarness: "harness",
    harnesses: {
      agents: [{
        name: "harness",
        display_name: "Harness",
        models: [{
          provider_id: "google",
          model_id: "gemini-2.5-flash",
          qualified_id: "google:gemini-2.5-flash",
          reasoning: { supported: true, options: [{ type: "budget_tokens", min: 0, max: 24576 }] },
        }],
      }],
    },
    harnessProvider: "google",
    harnessModel: "google:gemini-2.5-flash",
    harnessReasoningMode: "budget",
    harnessReasoningBudget: "30000",
  });
  harness.storage.set(ISSUE_AGENT_DEFAULTS_STORAGE_KEY, JSON.stringify(originalDefaults));

  await harness.saveDefaults();

  assert.equal(harness.fetchCalls.length, 0);
  assert.equal(harness.status.textContent, "Reasoning budget must be at most 24576");
  assert.deepEqual(JSON.parse(harness.storage.get(ISSUE_AGENT_DEFAULTS_STORAGE_KEY)), originalDefaults);
});

test("new issue form uploads selected initial attachments after create", async () => {
  const file = { name: "screenshot.png" };
  const harness = await issueSaveHarness({ mode: "create", files: [file] });

  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues");
  assert.equal(harness.fetchCalls[1].path, "/ui/api/v1/projects/p-demo/issues/i-0001/attachments");
  const body = harness.fetchCalls[1].options.body;
  assert.deepEqual(body.fields, [
    { name: "stage", value: "initial", filename: undefined },
    { name: "file", value: file, filename: "screenshot.png" },
  ]);
  assert.equal(harness.loads(), 1);
});

test("new issue form can save without queueing", async () => {
  const harness = await issueSaveHarness({ mode: "create", queueIssue: false });

  await harness.submit();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), {
    title: "Updated issue",
    body: "New body",
    acceptance_criteria: "New criteria",
    priority: 4,
    requires_human_review: false,
    auto_merge: true,
    plan_mode: false,
    agent_harness: "claude",
    harness_args: {
      codex: [],
      claude: [],
      harness: [],
    },
    schedule_state: "backlog",
  });
  assert.equal(harness.pushedPath(), "/ui/projects/p-demo/issues/i-0001");
  assert.equal(harness.loads(), 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "");
});

test("new issue form surfaces create failures and missing created issue id", async () => {
  const missingProject = await issueSaveHarness({ mode: "create", projectID: "" });
  await missingProject.submit();

  assert.equal(missingProject.fetchCalls.length, 0);
  assert.equal(missingProject.pushedPath(), "");
  assert.equal(missingProject.loads(), 0);
  assert.equal(missingProject.status.textContent, "Project is required");

  const failedCreate = await issueSaveHarness({ mode: "create", fetchOK: false, errorMessage: "issue title is required" });
  await failedCreate.submit();

  assert.equal(failedCreate.fetchCalls.length, 1);
  assert.equal(failedCreate.pushedPath(), "");
  assert.equal(failedCreate.loads(), 0);
  assert.equal(failedCreate.status.textContent, "issue title is required");

  const missingID = await issueSaveHarness({ mode: "create", responseIssue: {} });
  await missingID.submit();

  assert.equal(missingID.fetchCalls.length, 1);
  assert.equal(missingID.pushedPath(), "");
  assert.equal(missingID.loads(), 0);
  assert.equal(missingID.status.textContent, "Created issue ID unavailable");
});

test("triage card exposes accept reject and edit actions", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueCard({
    id: "i-0001",
    title: "Agent finding",
    schedule_state: "backlog",
    triage_state: "triage",
    priority: 1,
  }, {}, "triage", false);

  assert.match(html, /data-triage="accepted"/);
  assert.match(html, /data-triage="rejected"/);
  assert.match(html, /data-issue-edit="i-0001"/);
  assert.match(html, /data-issue-title="Agent finding"/);
});

test("triage action posts triage state and refreshes", async () => {
  for (const state of ["accepted", "rejected"]) {
    let clickHandler;
    const button = {
      dataset: { issue: "i-0001", triage: state },
      addEventListener(event, handler) {
        if (event === "click") clickHandler = handler;
      },
    };
    const fetchCalls = [];
    const context = await scriptContext({}, {
      fetch(path, options) {
        fetchCalls.push({ path, options });
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ issue: { id: "i-0001" } }),
        });
      },
    });
    const app = new context.FlowApp();
    app.querySelectorAll = (selector) => (selector === "[data-triage]" ? [button] : []);
    app.querySelector = () => ({ textContent: "" });
    let refreshed = false;
    app.bindIssueActions(async () => {
      refreshed = true;
    });

    await clickHandler();

    assert.equal(fetchCalls[0].path, "/ui/api/v1/issues/i-0001/triage");
    assert.equal(fetchCalls[0].options.method, "POST");
    assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
    assert.deepEqual(JSON.parse(fetchCalls[0].options.body), { state });
    assert.equal(refreshed, true);
  }
});

test("close action requires confirmation before posting", async () => {
  for (const confirmed of [false, true]) {
    let clickHandler;
    let confirmCalls = 0;
    const button = {
      dataset: { close: "i-0001", project: "p-demo" },
      addEventListener(event, handler) {
        if (event === "click") clickHandler = handler;
      },
    };
    const fetchCalls = [];
    const context = await scriptContext({
      confirm(message) {
        confirmCalls += 1;
        assert.equal(message, "Close this issue?");
        return confirmed;
      },
    }, {
      fetch(path, options) {
        fetchCalls.push({ path, options });
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ issue: { id: "i-0001" } }),
        });
      },
    });
    const app = new context.FlowApp();
    app.querySelectorAll = (selector) => (selector === "[data-close]" ? [button] : []);
    app.querySelector = () => ({ textContent: "" });
    let refreshed = false;
    app.bindIssueActions(async () => {
      refreshed = true;
    });

    await clickHandler();

    assert.equal(confirmCalls, 1);
    assert.equal(refreshed, confirmed);
    if (!confirmed) {
      assert.equal(fetchCalls.length, 0);
      continue;
    }
    assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/close");
    assert.equal(fetchCalls[0].options.method, "POST");
    assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
    assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  }
});

test("pause action requires confirmation before posting", async () => {
  for (const confirmed of [false, true]) {
    let clickHandler;
    let confirmCalls = 0;
    const button = {
      dataset: { pause: "i-0001", project: "p-demo" },
      addEventListener(event, handler) {
        if (event === "click") clickHandler = handler;
      },
    };
    const fetchCalls = [];
    const context = await scriptContext({
      confirm(message) {
        confirmCalls += 1;
        assert.equal(message, "Pause this task?");
        return confirmed;
      },
    }, {
      fetch(path, options) {
        fetchCalls.push({ path, options });
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ issue: { id: "i-0001" } }),
        });
      },
    });
    const app = new context.FlowApp();
    app.querySelectorAll = (selector) => (selector === "[data-pause]" ? [button] : []);
    app.querySelector = () => ({ textContent: "" });
    let refreshed = false;
    app.bindIssueActions(async () => {
      refreshed = true;
    });

    await clickHandler();

    assert.equal(confirmCalls, 1);
    assert.equal(refreshed, confirmed);
    if (!confirmed) {
      assert.equal(fetchCalls.length, 0);
      continue;
    }
    assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/pause");
    assert.equal(fetchCalls[0].options.method, "POST");
    assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
    assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  }
});

test("resume action posts and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: { resume: "i-0001", project: "p-demo" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ issue: { id: "i-0001" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-resume]" ? [button] : []);
  app.querySelector = () => ({ textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/resume");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  assert.equal(refreshed, true);
});

test("crash retry action posts and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: { retryCrash: "i-0001", project: "p-demo" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ issue: { id: "i-0001" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-retry-crash]" ? [button] : []);
  app.querySelector = () => ({ textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/retry");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  assert.equal(refreshed, true);
});

test("issue state form posts manual state and refreshes", async () => {
  let submitHandler;
  const form = {
    dataset: { issueStateForm: "i-0001", project: "p-demo" },
    elements: {
      state: { value: "backlog" },
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ issue: { id: "i-0001", schedule_state: "backlog" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-issue-state-form]" ? [form] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await submitHandler({ preventDefault() {} });

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/state");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), { state: "backlog" });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("issue state form requires confirmation before closing", async () => {
  for (const confirmed of [false, true]) {
    let submitHandler;
    let confirmCalls = 0;
    const form = {
      dataset: { issueStateForm: "i-0001", project: "p-demo" },
      elements: {
        state: { value: "closed" },
      },
      addEventListener(event, handler) {
        if (event === "submit") submitHandler = handler;
      },
    };
    const fetchCalls = [];
    const context = await scriptContext({
      confirm(message) {
        confirmCalls += 1;
        assert.equal(message, "Close this issue?");
        return confirmed;
      },
    }, {
      fetch(path, options) {
        fetchCalls.push({ path, options });
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ issue: { id: "i-0001", schedule_state: "closed" } }),
        });
      },
    });
    const app = new context.FlowApp();
    app.querySelectorAll = (selector) => (selector === "[data-issue-state-form]" ? [form] : []);
    app.querySelector = () => ({ textContent: "" });
    let refreshed = false;
    app.bindIssueActions(async () => {
      refreshed = true;
    });

    await submitHandler({ preventDefault() {} });

    assert.equal(confirmCalls, 1);
    assert.equal(refreshed, confirmed);
    if (!confirmed) {
      assert.equal(fetchCalls.length, 0);
      continue;
    }
    assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/state");
    assert.equal(fetchCalls[0].options.method, "POST");
    assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
    assert.deepEqual(JSON.parse(fetchCalls[0].options.body), { state: "closed" });
  }
});

test("issue state form surfaces failures without refreshing", async () => {
  let submitHandler;
  const form = {
    dataset: { issueStateForm: "i-0001" },
    elements: {
      state: { value: "up_next" },
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
  };
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch() {
      return Promise.resolve({
        ok: false,
        json: () => Promise.resolve({ error: { message: "merged issues cannot be moved" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-issue-state-form]" ? [form] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await submitHandler({ preventDefault() {} });

  assert.equal(refreshed, false);
  assert.equal(status.textContent, "merged issues cannot be moved");
});

test("review run action posts to issue review endpoint and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: { reviewRun: "i-0001", project: "p-demo" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ review_state: "in_review" }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-review-run]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/review/run");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("triage edit action patches issue title and refreshes", async () => {
  const harness = await triageEditHarness({ promptValue: "  New triage title  " });

  await harness.click();

  assert.equal(harness.fetchCalls[0].path, "/ui/api/v1/issues/i-0001");
  assert.equal(harness.fetchCalls[0].options.method, "PATCH");
  assert.equal(harness.fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(harness.fetchCalls[0].options.body), { title: "New triage title" });
  assert.equal(harness.refreshed(), true);
  assert.equal(harness.status.textContent, "");
});

test("triage edit action ignores cancel and blank titles", async () => {
  const cancelled = await triageEditHarness({ promptValue: null });
  await cancelled.click();
  assert.equal(cancelled.fetchCalls.length, 0);
  assert.equal(cancelled.refreshed(), false);
  assert.equal(cancelled.status.textContent, "");

  const blank = await triageEditHarness({ promptValue: "   " });
  await blank.click();
  assert.equal(blank.fetchCalls.length, 0);
  assert.equal(blank.refreshed(), false);
  assert.equal(blank.status.textContent, "Issue title is required");
});

test("triage edit action surfaces patch failures", async () => {
  const harness = await triageEditHarness({
    promptValue: "Updated",
    fetchOK: false,
    errorMessage: "issue title is required",
  });

  await harness.click();

  assert.equal(harness.fetchCalls.length, 1);
  assert.equal(harness.refreshed(), false);
  assert.equal(harness.status.textContent, "issue title is required");
});

test("change detail fetches read model and renders checks and merge action", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const diffContainer = { innerHTML: "" };
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/changes/ch-0001" },
      addEventListener() {},
      open() {
        throw new Error("window.open should not be used for change detail");
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      if (path === "/ui/api/v1/changes/ch-0001/diff") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change_id: "ch-0001",
            head_sha: "1234567890abcdef",
            available: true,
            total_files: 1,
            additions: 3,
            deletions: 1,
            files: [{
              path: "app.go",
              additions: 3,
              deletions: 1,
              hunks: [{
                header: "@@ -1,2 +1,4 @@",
                lines: [
                  { kind: "context", old_line: 1, new_line: 1, text: "package app" },
                  { kind: "delete", old_line: 2, text: "const Old = 1" },
                  { kind: "meta", text: "\\ No newline at end of file" },
                  { kind: "add", new_line: 2, text: "const New = 1" },
                ],
              }],
            }],
          }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change: {
            id: "ch-0001",
            issue_id: "i-0001",
            branch: "issue/i-0001",
            base: "main",
            head_sha: "1234567890abcdef",
            updated_at: "2026-06-07T12:00:00Z",
            ready_at: "2026-06-07T12:01:00Z",
          },
          issue: { id: "i-0001", title: "Ship web UI" },
          review_state: "approved",
          required_checks: { total: 1, satisfied: 1 },
          checks: [{ name: "unit", kind: "ci", required: true, verdict: "satisfied", details: "ok" }],
          threads: [{
            id: "th-0001",
            state: "open",
            anchor_commit_sha: "9876543210abcdef",
            file_path: "app.go",
            line: 4,
            context: "const Value = 1",
            comments: [
              { actor: "reviewer", body: "Review note", created_at: "2026-06-07T12:02:00Z" },
              { actor: "author", body: "I will address it.", created_at: "2026-06-07T12:03:00Z" },
            ],
          }],
          can_merge: true,
        }),
      });
    },
    console,
  };

  await applyContext(context);

  const flowApp = new context.FlowApp();
  flowApp.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    if (selector === '[data-change-diff="ch-0001"]') return diffContainer;
    return { textContent: "" };
  };
  flowApp.querySelectorAll = () => [];
  await flowApp.renderChange("ch-0001");

  assert.equal(fetchCalls[0].path, "/ui/api/v1/changes/ch-0001");
  assert.equal(fetchCalls[1].path, "/ui/api/v1/changes/ch-0001/diff");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.equal(title.textContent, "Change");
  assert.match(content.innerHTML, /ch-0001/);
  assert.match(content.innerHTML, /Ship web UI/);
  assert.match(content.innerHTML, /data-merge-change="ch-0001"/);
  assert.match(diffContainer.innerHTML, /files 1/);
  assert.match(diffContainer.innerHTML, /app.go/);
  assert.match(diffContainer.innerHTML, /<pre class="diff-unified" style="--diff-unified-width: 27ch;">/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-hunk-header">@@ -1,2 \+1,4 @@/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-del">-const Old = 1/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-meta">\\ No newline at end of file/);
  assert.doesNotMatch(diffContainer.innerHTML, /diff-unified-line diff-meta">\\\\ No newline at end of file/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-add">\+const New = 1/);
  assert.doesNotMatch(diffContainer.innerHTML, /\s+1\s+2 \+const New = 1/);
  assert.match(content.innerHTML, /unit/);
  assert.match(content.innerHTML, /Review note/);
  assert.match(content.innerHTML, /1234567890ab/);
  assert.match(content.innerHTML, /9876543210ab/);
  assert.match(content.innerHTML, /outdated anchor/);
  assert.match(content.innerHTML, /const Value = 1/);
  assert.match(content.innerHTML, /reviewer/);
  assert.match(content.innerHTML, /data-thread-claim="th-0001"/);
  assert.match(content.innerHTML, /data-thread-reply="th-0001"/);
  assert.match(content.innerHTML, /data-claim-kind="fixed"/);
  assert.match(content.innerHTML, /data-claim-commit="1234567890abcdef"/);
  assert.match(content.innerHTML, /I will address it\./);
  // Diff mode toggle renders and defaults to unified without refetching.
  assert.match(diffContainer.innerHTML, /data-diff-mode-toggle/);
  assert.match(diffContainer.innerHTML, /data-diff-mode="unified"/);
  assert.match(diffContainer.innerHTML, /data-diff-mode="split"/);
  assert.match(diffContainer.innerHTML, /aria-pressed="true"[^>]*>Unified/);
  assert.match(diffContainer.innerHTML, /aria-pressed="false"[^>]*>Split/);
  assert.match(diffContainer.innerHTML, /data-diff-mode="unified">/);
  assert.match(diffContainer.innerHTML, /diff-unified-line diff-add">\+const New = 1/);
  assert.equal(fetchCalls.filter((call) => call.path === "/ui/api/v1/changes/ch-0001/diff").length, 1);
});

test("change detail diff uses the same width for all files and hunks", async () => {
  const longLine = "const LongerValue = \"wide\";";
  const diff = {
    change_id: "ch-0001",
    head_sha: "1234567890abcdef",
    available: true,
    total_files: 2,
    additions: 1,
    deletions: 0,
    files: [
      {
        path: "app.go",
        additions: 0,
        deletions: 0,
        hunks: [
          {
            header: "@@ -1 +1 @@",
            lines: [{ kind: "context", old_line: 1, new_line: 1, text: "short" }],
          },
          {
            header: "@@ -20 +20 @@",
            lines: [{ kind: "context", old_line: 20, new_line: 20, text: "also short" }],
          },
        ],
      },
      {
        path: "wide.go",
        additions: 1,
        deletions: 0,
        hunks: [{
          header: "@@ -1 +1 @@",
          lines: [{ kind: "add", new_line: 1, text: longLine }],
        }],
      },
    ],
  };
  const context = await scriptContext();

  const unifiedHTML = context.renderDiffSummary(diff, "unified");
  const unifiedWidths = [...unifiedHTML.matchAll(/--diff-unified-width: (\d+)ch/g)].map((match) => match[1]);
  assert.deepEqual(unifiedWidths, [String(longLine.length + 1), String(longLine.length + 1), String(longLine.length + 1)]);

  const splitHTML = context.renderDiffSummary(diff, "split");
  const splitWidths = [...splitHTML.matchAll(/--diff-split-width: (\d+)ch/g)].map((match) => match[1]);
  assert.deepEqual(splitWidths, [String((longLine.length + 4) * 2), String((longLine.length + 4) * 2), String((longLine.length + 4) * 2)]);
});

test("change detail diff renders side-by-side split mode", async () => {
  const diff = {
    change_id: "ch-0001",
    head_sha: "1234567890abcdef",
    available: true,
    total_files: 1,
    additions: 3,
    deletions: 1,
    files: [{
      path: "app.go",
      additions: 3,
      deletions: 1,
      hunks: [{
        header: "@@ -1,2 +1,4 @@",
        lines: [
          { kind: "context", old_line: 1, new_line: 1, text: "package app" },
          { kind: "delete", old_line: 2, text: "const Old = 1" },
          { kind: "add", new_line: 2, text: "const New = 1" },
          { kind: "add", new_line: 3, text: "const Extra = 2" },
          { kind: "context", old_line: 4, new_line: 4, text: "var Value = 0" },
        ],
      }],
    }],
  };
  const context = await scriptContext();
  const html = context.renderDiffSummary(diff, "split");

  assert.match(html, /data-diff-mode="split"/);
  assert.match(html, /<table class="diff-split" style="--diff-split-width: 38ch;">/);
  assert.match(html, /<th class="diff-col-old">Old<\/th>/);
  assert.match(html, /<th class="diff-col-new">New<\/th>/);
  assert.match(html, /diff-row-pair/);
  assert.match(html, /diff-text diff-del">const Old = 1<\/span><\/td><td class="diff-col-new"/);
  assert.match(html, /diff-text diff-add">const New = 1<\/span>/);
  assert.match(html, /diff-row-add[^"]*"[^>]*><td class="diff-col-old empty"/);
  assert.match(html, /diff-text diff-add">const Extra = 2<\/span>/);
  assert.match(html, /diff-row-context/);
  assert.match(html, /package app/);
  assert.match(html, /var Value = 0/);
  assert.doesNotMatch(html, /<pre class="thread-context"/);
});

test("change detail diff toggle re-renders cached diff in the new mode without refetching", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const storage = new Map();
  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const splitButton = {
    listener: null,
    getAttribute(name) { return name === "data-diff-mode" ? "split" : null; },
    closest() { return diffContainer; },
    addEventListener(event, listener) { if (event === "click") this.listener = listener; },
  };
  const diffContainer = {
    innerHTML: "",
    attributes: new Map(),
    setAttribute(name, value) { this.attributes.set(name, value); },
    getAttribute(name) { return this.attributes.get(name) || null; },
    closest() { return this; },
    querySelectorAll(selector) { return selector === "[data-diff-mode-toggle] button" ? [splitButton] : []; },
  };
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: { cookie: "flow_ui_csrf=csrf-token", addEventListener() {} },
    history: { pushState() {} },
    window: { location: { pathname: "/ui/changes/ch-0001" }, addEventListener() {}, open() {}, localStorage },
    fetch(path) {
      fetchCalls.push(path);
      if (path === "/ui/api/v1/changes/ch-0001/diff") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            change_id: "ch-0001",
            head_sha: "1234567890abcdef",
            available: true,
            total_files: 1,
            additions: 3,
            deletions: 1,
            files: [{
              path: "app.go",
              additions: 3,
              deletions: 1,
              hunks: [{
                header: "@@ -1,2 +1,4 @@",
                lines: [
                  { kind: "context", old_line: 1, new_line: 1, text: "package app" },
                  { kind: "delete", old_line: 2, text: "const Old = 1" },
                  { kind: "add", new_line: 2, text: "const New = 1" },
                ],
              }],
            }],
          }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change: { id: "ch-0001", issue_id: "i-0001", branch: "issue/i-0001", base: "main", head_sha: "1234567890abcdef", updated_at: "2026-06-07T12:00:00Z" },
          issue: { id: "i-0001", title: "Ship web UI" },
          review_state: "approved",
          required_checks: { total: 0, satisfied: 0 },
          checks: [],
          threads: [],
          can_merge: false,
        }),
      });
    },
    console,
  };

  await applyContext(context);
  const flowApp = new context.FlowApp();
  flowApp.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    if (selector === '[data-change-diff="ch-0001"]') return diffContainer;
    return { textContent: "" };
  };
  flowApp.querySelectorAll = () => [];
  await flowApp.renderChange("ch-0001");

  assert.match(diffContainer.innerHTML, /data-diff-mode="unified"/);
  assert.match(diffContainer.innerHTML, /<pre class="diff-unified"/);
  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), undefined);
  assert.equal(fetchCalls.filter((p) => p === "/ui/api/v1/changes/ch-0001/diff").length, 1);
  assert.equal(typeof splitButton.listener, "function");

  splitButton.listener();

  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), "split");
  assert.match(diffContainer.innerHTML, /data-diff-mode="split"/);
  assert.match(diffContainer.innerHTML, /<table class="diff-split"/);
  assert.doesNotMatch(diffContainer.innerHTML, /<pre class="thread-context"/);
  assert.equal(fetchCalls.filter((p) => p === "/ui/api/v1/changes/ch-0001/diff").length, 1);
});

test("readDiffMode round-trips split and falls back to unified for invalid values", async () => {
  const storage = new Map();
  const localStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const context = await scriptContext({ localStorage });

  assert.equal(context.readDiffMode(), "unified");
  context.writeDiffMode("split");
  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), "split");
  assert.equal(context.readDiffMode(), "split");

  context.writeDiffMode("bogus");
  assert.equal(storage.get(DIFF_MODE_STORAGE_KEY), "split");
  assert.equal(context.readDiffMode(), "split");

  storage.set(DIFF_MODE_STORAGE_KEY, "not-a-mode");
  assert.equal(context.readDiffMode(), "unified");
});

test("thread claim action posts claim payload and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadClaim: "th-0001",
      claimKind: "not_warranted",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return "Intentional behavior.";
    },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ thread: { id: "th-0001", state: "claimed" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-claim]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/threads/th-0001/claims");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    kind: "not_warranted",
    body: "Intentional behavior.",
    claim_commit_sha: "",
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("thread fixed claim posts current head without prompting", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadClaim: "th-0001",
      claimKind: "fixed",
      claimCommit: "1234567890abcdef",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const context = await scriptContext({
    prompt() {
      throw new Error("fixed claims should not prompt");
    },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ thread: { id: "th-0001", state: "claimed" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-claim]" ? [button] : []);
  app.querySelector = () => ({ textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    kind: "fixed",
    body: "",
    claim_commit_sha: "1234567890abcdef",
  });
  assert.equal(refreshed, true);
});

test("thread claim action requires rationale for non-fixed claims", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadClaim: "th-0001",
      claimKind: "superseded",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return "";
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-claim]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(status.textContent, "Thread claim rationale is required");
  assert.equal(refreshed, false);
});

test("thread reply action posts comment payload and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadReply: "th-0001",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return "I can reproduce this.";
    },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ thread: { id: "th-0001", comments: [{ body: "I can reproduce this." }] } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-reply]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/threads/th-0001/comments");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    body: "I can reproduce this.",
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("thread reply action requires comment text", async () => {
  let clickHandler;
  const button = {
    dataset: {
      threadReply: "th-0001",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return " ";
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-thread-reply]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(status.textContent, "Thread reply is required");
  assert.equal(refreshed, false);
});

test("plan approve action posts to issue plan endpoint and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: { planApprove: "i-0001", project: "p-demo" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ issue: { id: "i-0001" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-plan-approve]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/plan/approve");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {});
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "plan approved");
});

test("plan reject action posts comments to issue plan endpoint and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: { planReject: "i-0001", project: "p-demo" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({
    prompt() {
      return "Please narrow the first step.";
    },
  }, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ issue: { id: "i-0001" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-plan-reject]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/plan/reject");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), { comments: "Please narrow the first step." });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "plan rejected");
});

test("attention reply form posts message and status log id", async () => {
  let submitHandler;
  const form = {
    dataset: { attentionReplyForm: "i-0001", statusLogId: "42", project: "p-demo" },
    elements: {
      message: { value: "Use the smaller scope." },
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ queued: true }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-attention-reply-form]" ? [form] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await submitHandler({ preventDefault() {} });

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-demo/issues/i-0001/attention/reply");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    message: "Use the smaller scope.",
    status_log_id: 42,
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "reply sent");
});

test("human review check renders approval action only while unsatisfied", async () => {
  const context = await scriptContext();
  const pendingHTML = context.renderCheck({
    issue_id: "i-0001",
    name: "human-review",
    kind: "human",
    required: true,
    verdict: "pending",
  });
  const satisfiedHTML = context.renderCheck({
    issue_id: "i-0001",
    name: "human-review",
    kind: "human",
    required: true,
    verdict: "satisfied",
  });
  const ciHTML = context.renderCheck({
    issue_id: "i-0001",
    name: "unit",
    kind: "ci",
    required: true,
    verdict: "pending",
  });

  assert.match(pendingHTML, /data-human-review-approve="i-0001"/);
  assert.match(pendingHTML, /data-check-name="human-review"/);
  assert.match(pendingHTML, />Approve<\/button>/);
  assert.doesNotMatch(satisfiedHTML, /data-human-review-approve/);
  assert.doesNotMatch(ciHTML, /data-human-review-approve/);
});

test("human review approval action reports satisfied check and refreshes", async () => {
  let clickHandler;
  const button = {
    dataset: {
      humanReviewApprove: "i-0001",
      checkName: "human-review",
    },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const fetchCalls = [];
  const status = { textContent: "" };
  const context = await scriptContext({}, {
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          check: { name: "human-review", kind: "human", required: true, verdict: "satisfied" },
          review_state: "approved",
        }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-human-review-approve]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  await clickHandler();

  assert.equal(fetchCalls[0].path, "/ui/api/v1/issues/i-0001/checks/human-review");
  assert.equal(fetchCalls[0].options.method, "POST");
  assert.equal(fetchCalls[0].options.headers["X-Flow-CSRF"], "csrf-token");
  assert.deepEqual(JSON.parse(fetchCalls[0].options.body), {
    kind: "human",
    required: true,
    verdict: "satisfied",
    details: "approved via web UI",
    reporter: "web-ui",
  });
  assert.equal(refreshed, true);
  assert.equal(status.textContent, "");
});

test("claimed threads render reply without claim actions", async () => {
  const context = await scriptContext();
  const html = context.renderThread({
    id: "th-0001",
    state: "claimed",
    claim_kind: "fixed",
    claim_commit_sha: "1234567890abcdef",
    file_path: "app.go",
    line: 4,
    comments: [{ actor: "author", body: "Fixed.", created_at: "2026-06-07T12:00:00Z" }],
  }, "1234567890abcdef");

  assert.match(html, /claim fixed/);
  assert.match(html, /data-thread-reply="th-0001"/);
  assert.doesNotMatch(html, /data-thread-claim="th-0001"/);
});

test("review threads mark anchors that differ from the current change head", async () => {
  const context = await scriptContext();
  const staleHTML = context.renderThread({
    id: "th-0001",
    state: "open",
    anchor_commit_sha: "oldhead1234567890",
    file_path: "app.go",
    line: 4,
    context: "const Value = 1",
  }, "newhead1234567890");
  const currentHTML = context.renderThread({
    id: "th-0002",
    state: "open",
    anchor_commit_sha: "newhead1234567890",
    file_path: "app.go",
    line: 5,
    context: "const Value = 2",
  }, "newhead1234567890");

  assert.match(staleHTML, /outdated anchor/);
  assert.match(staleHTML, /oldhead12345/);
  assert.match(staleHTML, /const Value = 1/);
  assert.doesNotMatch(currentHTML, /outdated anchor/);
  assert.match(currentHTML, /newhead12345/);
});

test("change diff cache refetches only when head changes", async () => {
  let fetchCount = 0;
  const context = await scriptContext({}, {
    fetch(path) {
      fetchCount += 1;
      assert.equal(path, "/ui/api/v1/changes/ch-0001/diff");
      const headSHA = fetchCount === 1 ? "head-1" : "head-2";
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change_id: "ch-0001",
          head_sha: headSHA,
          available: true,
          total_files: 1,
          files: [{ path: "one.go", additions: 1, deletions: 0 }],
        }),
      });
    },
  });
  const containers = [{ innerHTML: "" }, { innerHTML: "" }, { innerHTML: "" }];
  let containerIndex = 0;
  const app = new context.FlowApp();
  app.querySelector = () => containers[containerIndex++];

  await app.renderChangeDiff("ch-0001", "head-1");
  await app.renderChangeDiff("ch-0001", "head-1");
  await app.renderChangeDiff("ch-0001", "head-2");

  assert.match(containers[0].innerHTML, /one.go/);
  assert.match(containers[1].innerHTML, /one.go/);
  assert.match(containers[2].innerHTML, /one.go/);
  assert.equal(fetchCount, 2);
});

test("change diff ignores payload for a different head", async () => {
  const context = await scriptContext({}, {
    fetch(path) {
      assert.equal(path, "/ui/api/v1/changes/ch-0001/diff");
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          change_id: "ch-0001",
          head_sha: "new-head",
          available: true,
          files: [{ path: "stale.go", additions: 1, deletions: 0 }],
        }),
      });
    },
  });
  const container = { innerHTML: "" };
  const app = new context.FlowApp();
  app.querySelector = () => container;

  const rendered = await app.renderChangeDiff("ch-0001", "old-head");

  assert.equal(rendered, true);
  assert.match(container.innerHTML, /waiting for refresh/);
  assert.equal(app.changeDiffCache?.has("ch-0001:old-head"), false);
});

test("issue detail renders owner metadata, relations, sessions, changes, and checks", async () => {
  const fetchCalls = [];
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/projects/p-alpha/issues/i-0001" },
      addEventListener() {},
      open() {
        throw new Error("window.open should not be used for issue detail render");
      },
    },
    fetch(path, options) {
      fetchCalls.push({ path, options });
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          project_id: "p-alpha",
          project_name: "alpha",
          issue: {
            id: "i-0001",
            title: "Issue detail",
            body: "Body",
            acceptance_criteria: "Done",
            priority: 2,
            schedule_state: "up_next",
            triage_state: "accepted",
            created_by: "agent",
            source_issue_id: "i-0000",
            source_change_id: "ch-0000",
            updated_at: "2026-06-07T12:00:00Z",
          },
          issue_detail: {
            tags: [{ slug: "web-ui" }],
            relations: [{ source_issue_id: "i-0002", target_issue_id: "i-0001", kind: "blocks" }],
            active_session: { id: "s-0001", state: "waiting", worker_id: "w-local", branch: "issue/i-0001" },
            terminal_available: true,
            sessions: [{ id: "s-0001", state: "waiting", worker_id: "w-local", branch: "issue/i-0001", terminal_available: true, transcript_available: true, updated_at: "2026-06-07T12:01:00Z" }],
            changes: [{ id: "ch-0001", branch: "issue/i-0001", head_sha: "abcdef1234567890", updated_at: "2026-06-07T12:02:00Z" }],
            ready_change: { id: "ch-ready", branch: "issue/i-0001", head_sha: "1234567890abcdef", ready_at: "2026-06-07T12:03:00Z" },
            review_state: "changes_requested",
            required_checks: { total: 1, blocked: 1 },
            checks: [{ name: "unit", kind: "ci", required: true, verdict: "blocked", details: "failed" }],
            attachments: [{ id: "att-0001", issue_id: "i-0001", stage: "reviewer", filename: "review.png", content_type: "image/png", size_bytes: 2048 }],
            transitions: [
              { seq: 2, from_phase: "up_next", event_kind: "ensure_author_job", to_phase: "authoring", actor: "owner:cli", created_at: "2026-06-07T12:04:00Z" },
            ],
            lifecycle_graph: {
              current_phase: "authoring",
              edges: [{ from_phase: "up_next", to_phase: "authoring", count: 1 }],
              reviewer_sends: 0,
              verifier_sends: 0,
            },
          },
        }),
      });
    },
    console,
  };

  await applyContext(context);

  const flowApp = new context.FlowApp();
  flowApp.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  flowApp.querySelectorAll = () => [];
  await flowApp.renderIssue("i-0001", undefined, "p-alpha");

  assert.equal(fetchCalls[0].path, "/ui/api/v1/projects/p-alpha/issues/i-0001");
  assert.match(content.innerHTML, /class="detail issue-detail"/);
  assert.match(content.innerHTML, /class="issue-detail-grid"/);
  assert.match(content.innerHTML, /class="issue-detail-column issue-detail-editor"/);
  assert.match(content.innerHTML, /class="issue-detail-column issue-detail-activity"/);
  assert.match(content.innerHTML, /class="issue-detail-column issue-detail-system"/);
  assert.match(content.innerHTML, /class="issue-detail-lifecycle"><h3>Lifecycle<\/h3><div class="lifecycle-chart"><svg/);
  // Unified timeline: sessions, transitions and status all render inside it.
  assert.match(content.innerHTML, /<div class="feed timeline-feed" data-timeline>/);
  assert.match(content.innerHTML, /data-timeline-glyph="session"/);
  assert.match(content.innerHTML, /data-timeline-glyph="transition"/);
  // Standalone Sessions/Status feeds are gone (folded into the timeline).
  assert.doesNotMatch(content.innerHTML, /<h3>Sessions<\/h3>/);
  assert.doesNotMatch(content.innerHTML, /<h3>Status<\/h3>/);
  // Read-only detail summary with an Edit toggle that reveals the form.
  assert.match(content.innerHTML, /data-issue-read-only/);
  assert.match(content.innerHTML, /data-issue-edit-toggle/);
  assert.match(content.innerHTML, /data-issue-edit-form/);
  assert.match(content.innerHTML, /web-ui/);
  assert.match(content.innerHTML, /href="\/ui\/projects\/p-alpha\/issues\/i-0002"/);
  assert.match(content.innerHTML, /data-terminal="s-0001"/);
  assert.equal(content.innerHTML.match(/data-terminal="s-0001"/g)?.length, 2);
  assert.match(content.innerHTML, /data-session-transcript="s-0001"/);
  assert.match(content.innerHTML, /data-issue-state-form="i-0001"/);
  assert.match(content.innerHTML, /<option value="up_next" selected>Up Next<\/option>/);
  assert.match(content.innerHTML, /data-pause="i-0001"/);
  assert.doesNotMatch(content.innerHTML, /data-close="i-0001"/);
  assert.match(content.innerHTML, /data-review-run="i-0001"/);
  assert.match(content.innerHTML, /ch-0001/);
  assert.match(content.innerHTML, /Ready Change/);
  assert.match(content.innerHTML, /ch-ready/);
  assert.match(content.innerHTML, /s-0001/);
  assert.match(content.innerHTML, /unit/);
  assert.match(content.innerHTML, /review\.png/);
  assert.match(content.innerHTML, /class="attachment-preview"/);
  assert.match(content.innerHTML, /\/ui\/api\/v1\/projects\/p-alpha\/issues\/i-0001\/attachments\/att-0001\?download=1/);
  assert.match(content.innerHTML, /<span class="badge warn">changes requested<\/span>/);
  assert.match(content.innerHTML, /Source Issue/);
  assert.match(content.innerHTML, /Lifecycle/);
  assert.match(content.innerHTML, /<div class="lifecycle-chart"><svg/);
  assert.match(content.innerHTML, /ensure_author_job|up_next/);
});

test("issue detail renders pending plan as a full-width human attention panel", async () => {
  const harness = await browserSmokeHarness("/ui/projects/p-alpha/issues/i-0001", {
    "/ui/api/v1/projects/p-alpha/issues/i-0001": {
      project_id: "p-alpha",
      issue: {
        id: "i-0001",
        title: "Plan issue",
        schedule_state: "up_next",
        triage_state: "accepted",
        priority: 1,
        created_by: "agent",
        agent_harness: "harness",
        plan_body: "1. Inspect state\n2. Patch the UI",
        plan_submitted_at: "2026-06-18T12:36:46Z",
        updated_at: "2026-06-18T12:37:00Z",
      },
      issue_detail: {},
    },
  });

  await harness.app.load();

  const html = harness.content.innerHTML;
  assert.match(html, /class="human-attention-panel"/);
  assert.match(html, /Plan Review/);
  assert.match(html, /data-plan-approve="i-0001"/);
  assert.match(html, /data-plan-reject="i-0001"/);
  assert.match(html, /<ol>\s*<li>Inspect state<\/li>\s*<li>Patch the UI<\/li>\s*<\/ol>/);
  assert.ok(html.indexOf("summary-grid") < html.indexOf("human-attention-panel"));
  assert.ok(html.indexOf("human-attention-panel") < html.indexOf("issue-detail-grid"));
});

test("issue detail renders resume for a paused task", async () => {
  const harness = await browserSmokeHarness("/ui/projects/p-alpha/issues/i-0001", {
    "/ui/api/v1/projects/p-alpha/issues/i-0001": {
      project_id: "p-alpha",
      issue: {
        id: "i-0001",
        title: "Paused issue",
        schedule_state: "up_next",
        triage_state: "accepted",
        priority: 1,
        created_by: "human",
        updated_at: "2026-06-07T12:00:00Z",
      },
      issue_detail: {
        paused: true,
        sessions: [{ id: "s-0001", state: "abandoned", worker_id: "w-local", branch: "issue/i-0001", updated_at: "2026-06-07T12:01:00Z" }],
      },
    },
  });

  await harness.app.load();

  assert.match(harness.content.innerHTML, /data-resume="i-0001"/);
  assert.doesNotMatch(harness.content.innerHTML, /data-pause="i-0001"/);
  assert.doesNotMatch(harness.content.innerHTML, /data-close="i-0001"/);
});

test("issue detail renders retry for a crash-held task", async () => {
  const harness = await browserSmokeHarness("/ui/projects/p-alpha/issues/i-0001", {
    "/ui/api/v1/projects/p-alpha/issues/i-0001": {
      project_id: "p-alpha",
      issue: {
        id: "i-0001",
        title: "Crash held issue",
        schedule_state: "up_next",
        triage_state: "accepted",
        priority: 1,
        created_by: "human",
        updated_at: "2026-06-07T12:00:00Z",
      },
      issue_detail: {
        wait_reason: "crash_loop",
      },
    },
  });

  await harness.app.load();

  assert.match(harness.content.innerHTML, /data-retry-crash="i-0001"/);
});

test("attachment previews are limited to safe raster image types", async () => {
  const context = await scriptContext();

  assert.equal(context.isImageContentType("image/png"), true);
  assert.equal(context.isImageContentType("image/jpeg; charset=binary"), true);
  assert.equal(context.isImageContentType("text/html"), false);
  assert.equal(context.isImageContentType("image/svg+xml"), false);
});

test("issueHref requires project context for issue detail links", async () => {
  const context = await scriptContext();

  assert.equal(context.issueHref("p-alpha", "i-0001"), "/ui/projects/p-alpha/issues/i-0001");
  assert.equal(context.issueHref("", "i-0001"), "#");
});

test("renderLifecycleChart draws the canonical graph with counts and highlights", async () => {
  const context = await scriptContext();
  const html = context.renderLifecycleChart({
    current_phase: "critique",
    edges: [
      { from_phase: "triage", to_phase: "backlog", count: 1 },
      { from_phase: "authoring", to_phase: "critique", count: 4 },
      { from_phase: "critique", to_phase: "authoring", count: 3 },
    ],
    reviewer_sends: 2,
    verifier_sends: 1,
  });
  assert.match(html, /<svg/);
  const phases = [
    "backlog", "triage", "up_next", "planning", "authoring", "critique",
    "acceptance", "approved", "merged_closed", "rejected_closed", "abandoned",
  ];
  for (const phase of phases) {
    assert.match(html, new RegExp(`data-node="${phase}"`), `missing node ${phase}`);
  }
  assert.match(html, /data-node="critique"[^>]*\bis-current\b|class="[^"]*is-current[^"]*"[^>]*data-node="critique"/);
  assert.match(html, /data-node="critique"[\s\S]*class="lifecycle-current-halo"/);
  assert.equal((html.match(/class="lifecycle-current-halo"/g) || []).length, 1);
  assert.match(html, /×4/);
  assert.match(html, /×3/);
  // triage→backlog is a canonical (accept) edge, not a dashed overlay.
  assert.match(html, /×1/);
  assert.doesNotMatch(html, /is-extra/);
  assert.match(html, /is-untaken/);
  assert.match(html, /reviewer ×2/);
  assert.match(html, /verifier ×1/);
});

test("renderLifecycleChart overlays observed edges missing from the canonical graph", async () => {
  const context = await scriptContext();
  const html = context.renderLifecycleChart({
    current_phase: "authoring",
    edges: [{ from_phase: "merged_closed", to_phase: "authoring", count: 2 }],
    reviewer_sends: 0,
    verifier_sends: 0,
  });
  assert.match(html, /is-extra/);
  assert.match(html, /×2/);
});

test("renderLifecycleChart tolerates a missing or empty graph", async () => {
  const context = await scriptContext();
  for (const graph of [null, undefined, {}]) {
    const html = context.renderLifecycleChart(graph);
    assert.match(html, /<svg/);
    assert.doesNotMatch(html, /is-current/);
    assert.doesNotMatch(html, /Sent back/);
  }
});

test("diagnostics rows render queue, lease, tmux, session, and taints", async () => {
  const context = await scriptContext();
  const queueHTML = context.renderQueueSummary({
    queued: 2,
    persistent_agent: 1,
    ephemeral: 1,
    author: 1,
    ci: 1,
  });
  assert.match(queueHTML, /queued 2/);
  assert.match(queueHTML, /persistent 1/);

  const workerHTML = context.renderWorkerRow({
    id: "w-local",
    status: "registered",
    capacity_persistent_agent: 2,
    capacity_ephemeral: 1,
    labels: { "agent.harness.codex": "true" },
    taints: [{ key: "gpu", value: "false", effect: "NoSchedule" }],
    last_seen_at: "2026-06-07T12:00:00Z",
  }, {
    live_jobs: 1,
    live_persistent_agent: 1,
    live_ephemeral: 0,
    expired_unreleased_jobs: 1,
    expired_unreleased_persistent_agent: 1,
  });
  assert.match(workerHTML, /1 jobs/);
  assert.match(workerHTML, /expired 1/);
  assert.match(workerHTML, /held 1\/0/);
  assert.match(workerHTML, /agent\.harness\.codex=true/);
  assert.match(workerHTML, /gpu=false:NoSchedule/);

  const jobHTML = context.renderJobRow({
    id: "j-0001",
    state: "running",
    role: "ci",
    capacity_bucket: "ephemeral",
    issue_id: "i-0001",
    change_id: "ch-0001",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    project_id: "p-alpha",
    project_name: "alpha",
    lease: { id: "l-0001", worker_id: "w-local" },
    live_lease: true,
    lease_status: "live",
    tmux_session: "flow-j-0001",
    session: { id: "s-0001", state: "working", terminal_available: true, transcript_available: true },
    change: { id: "ch-0001" },
  });
  assert.match(jobHTML, /alpha/);
  assert.match(jobHTML, /class="row-run"/);
  assert.match(jobHTML, /l-0001/);
  assert.match(jobHTML, /live/);
  assert.match(jobHTML, /flow-j-0001/);
  assert.match(jobHTML, /working/);
  assert.match(jobHTML, /data-terminal="s-0001"/);
  assert.match(jobHTML, /data-job-attach="j-0001"/);
  assert.match(jobHTML, /data-session-transcript="s-0001"/);
  assert.match(jobHTML, /\/ui\/projects\/p-alpha\/issues\/i-0001/);
  assert.match(jobHTML, /\/ui\/changes\/ch-0001/);

  const jobTranscriptHTML = context.renderJobRow({
    id: "j-0004",
    state: "finished",
    role: "reviewer",
    capacity_bucket: "ephemeral",
    issue_id: "i-0001",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    lease: { id: "l-0004", worker_id: "w-local" },
    transcript_available: true,
  });
  assert.match(jobTranscriptHTML, /data-job-transcript="j-0004"/);

  const reviewerJobHTML = context.renderJobRow({
    id: "j-0003",
    state: "running",
    role: "reviewer",
    capacity_bucket: "persistent_agent",
    issue_id: "i-0001",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    lease: { id: "l-0003", worker_id: "w-local" },
    live_lease: true,
    lease_status: "live",
    tmux_session: "flow-j-0003",
    terminal_available: true,
  });
  assert.match(reviewerJobHTML, /data-job-terminal="j-0003"/);
  assert.match(reviewerJobHTML, /data-job-attach="j-0003"/);

  const expiredJobHTML = context.renderJobRow({
    id: "j-0002",
    state: "claimed",
    role: "ci",
    capacity_bucket: "persistent_agent",
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    lease: { id: "l-0002", worker_id: "w-local" },
    live_lease: false,
    lease_status: "expired",
  });
  assert.match(expiredJobHTML, /l-0002/);
  assert.match(expiredJobHTML, /expired/);
});

test("board cards render tags and relationship indicators", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueCard({
    id: "i-0001",
    title: "Tagged issue",
    schedule_state: "backlog",
    triage_state: "triage",
    priority: 1,
    created_by: "agent",
  }, {
    tags: [{ slug: "web-ui" }],
    relations: {
      parents: 1,
      children: 2,
      blocks: 1,
      blocked_by: 1,
      related: 3,
    },
  }, "triage", false);

  assert.match(html, /web-ui/);
  assert.match(html, /parent 1/);
  assert.match(html, /child 2/);
  assert.match(html, /blocks 1/);
  assert.match(html, /blocked by 1/);
  assert.match(html, /related 3/);
});

test("board cards suppress duplicate visible status labels", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = {
    id: "i-0001",
    title: "Needs changes",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  };
  const render = (tags) => app.renderIssueCard(issue, {
    review_state: "changes_requested",
    tags,
  }, "changes_requested", false);

  const dashedHTML = render([{ slug: "changes-requested" }]);
  assert.equal(dashedHTML.match(/changes requested/g)?.length, 1);
  assert.doesNotMatch(dashedHTML, /changes-requested/);
  assert.doesNotMatch(dashedHTML, /<span class="badge warn">changes requested<\/span>/);

  const reportedHTML = render([{ slug: "* CHANGES REQUESTED" }]);
  assert.equal(reportedHTML.match(/changes requested/g)?.length, 1);
  assert.doesNotMatch(reportedHTML, /\* CHANGES REQUESTED/);
  assert.doesNotMatch(reportedHTML, /<span class="badge warn">changes requested<\/span>/);
});

test("board cards suppress duplicate visible labels from all badge sources", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueCard({
    id: "i-0001",
    title: "Badge sources",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  }, {
    required_checks: { total: 2, satisfied: 1 },
    blockers: { count: 2, issues: [] },
    latest_status: { message: "cannot continue", kind: "blocker" },
    blocking_reason: "waiting for response",
    primary_action: "Resume",
    tags: [
      { slug: "in-review" },
      { slug: "checks 1/2" },
      { slug: "blockers 2" },
      { slug: "waiting_for_response" },
      { slug: "blocker" },
      { slug: "resume" },
      { slug: "visible-tag" },
    ],
  }, "in_review", false, 0, null, "question");

  assert.equal(html.match(/in review/g)?.length, 1);
  assert.equal(html.match(/checks 1\/2/g)?.length, 1);
  assert.equal(html.match(/blockers 2/g)?.length, 1);
  assert.equal(html.match(/waiting for response/g)?.length, 1);
  assert.equal(html.match(/>blocker<\/span>/g)?.length, 1);
  assert.equal(html.match(/Resume/g)?.length, 1);
  assert.match(html, /visible-tag/);
  assert.doesNotMatch(html, /in-review/);
  assert.doesNotMatch(html, /waiting_for_response/);
});

test("board cards render job terminal actions without active sessions", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueCard({
    id: "i-0001",
    title: "Review running",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  }, {
    terminal_available: true,
    terminal_job_id: "j-0001",
  }, "in_review", false);

  assert.match(html, /class="button secondary terminal-button icon-button"/);
  assert.match(html, /data-job-terminal="j-0001"/);
  assert.match(html, /aria-label="Open terminal"/);
  assert.match(html, /<svg class="button-icon"/);
  assert.doesNotMatch(html, /data-terminal=/);
});

test("board cards render session terminal icon actions in every running board state", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const states = ["planning", "in_progress", "in_review", "changes_requested", "ready_to_merge"];

  for (const state of states) {
    const html = app.renderIssueCard({
      id: `i-${state}`,
      title: `${state} issue`,
      schedule_state: "up_next",
      triage_state: "accepted",
      priority: 1,
    }, {
      active_session: { id: `s-${state}`, state: "working", terminal_available: true },
    }, state, false);

    assert.match(html, new RegExp(`data-terminal="s-${state}"`));
    assert.match(html, /class="button secondary terminal-button icon-button"/);
    assert.match(html, /aria-label="Open terminal"/);
    assert.match(html, /<svg class="button-icon"/);
  }
});

test("ready to merge cards render diff stats and head sha", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueCard({
    id: "i-0001",
    title: "Merge me",
    schedule_state: "accepted",
    triage_state: "accepted",
    priority: 1,
    updated_at: "2026-06-07T12:00:00Z",
  }, {
    change: {
      id: "ch-0001",
      branch: "issue/i-0001",
      head_sha: "abcdef1234567890",
    },
    diff_stats: {
      head_sha: "abcdef1234567890",
      total_files: 2,
      additions: 12,
      deletions: 3,
    },
    review_state: "approved",
  }, "ready_to_merge", false);

  assert.match(html, /head abcdef123456/);
  assert.match(html, /files 2/);
  assert.match(html, /<span class="diff-add">\+12<\/span>/);
  assert.match(html, /<span class="diff-del">-3<\/span>/);
  assert.match(html, /data-merge="i-0001"/);
});

test("ready to merge cards hide merge action unless approved", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = {
    id: "i-0001",
    title: "Not approved",
    schedule_state: "accepted",
    triage_state: "accepted",
    priority: 1,
  };
  const change = {
    id: "ch-0001",
    branch: "issue/i-0001",
    head_sha: "abcdef1234567890",
  };

  assert.doesNotMatch(app.renderIssueCard(issue, { change }, "ready_to_merge", false), /data-merge="i-0001"/);
  assert.doesNotMatch(app.renderIssueCard(issue, { change, review_state: "changes_requested" }, "ready_to_merge", false), /data-merge="i-0001"/);
  assert.match(app.renderIssueCard(issue, { change, review_state: "approved" }, "ready_to_merge", false), /data-merge="i-0001"/);
});

test("cards carry phase identity as data-phase and a phase badge", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = {
    id: "i-0001",
    title: "Phase identity",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  };

  const inReview = app.renderIssueCard(issue, {}, "in_review", false);
  assert.match(inReview, /<article class="card" data-phase="critique"/);
  assert.match(inReview, /<span class="badge" data-phase="critique"><span class="dot"><\/span>in review<\/span>/);

  const queued = app.renderIssueCard(issue, {}, "", false);
  assert.match(queued, /<article class="card" data-phase="up_next"/);

  const triage = app.renderIssueCard({ ...issue, triage_state: "triage" }, {}, "triage", false);
  assert.match(triage, /<article class="card" data-phase="triage"/);
});

test("blocked cards render the blocked overlay phase and badge", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = {
    id: "i-0001",
    title: "Blocked work",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  };

  const blocked = app.renderIssueCard(issue, {}, "in_progress", true);
  assert.match(blocked, /<article class="card" data-phase="blocked"/);
  assert.match(blocked, /<span class="badge blocked">blocked<\/span>/);
  assert.match(blocked, /<span class="badge" data-phase="authoring">/);

  const withBlockers = app.renderIssueCard(issue, { blockers: { count: 2, issues: [] } }, "in_progress", false);
  assert.match(withBlockers, /<article class="card" data-phase="blocked"/);
  assert.match(withBlockers, /<span class="badge blocked">blockers 2<\/span>/);
});

test("lifecycle transitions render phase badges around an arrow", async () => {
  const context = await scriptContext();
  const html = context.renderTransition({
    event_kind: "advance",
    from_phase: "authoring",
    to_phase: "critique",
    actor: "workflow",
    created_at: "2026-06-07T12:00:00Z",
  });

  assert.match(html, /<span class="badge" data-phase="authoring"><span class="dot"><\/span>authoring<\/span>/);
  assert.match(html, /<span class="arrow">→<\/span>/);
  assert.match(html, /<span class="badge" data-phase="critique"><span class="dot"><\/span>critique<\/span>/);

  const initial = context.renderTransition({ event_kind: "create", to_phase: "triage", created_at: "2026-06-07T12:00:00Z" });
  assert.match(initial, /<span class="badge idle">—<\/span>/);
});

test("unified timeline merges sessions, transitions, and status by time with a show-more cap", async () => {
  const context = await scriptContext();
  const sessions = [
    { id: "s-1", state: "working", worker_id: "w-local", branch: "issue/i-0001", terminal_available: true, transcript_available: true, updated_at: "2026-06-07T12:05:00Z" },
  ];
  const transitions = [
    { seq: 1, event_kind: "session_ready", session_id: "s-1", head_sha: "abcdef1234567890", change_id: "ch-1", created_at: "2026-06-07T12:03:00Z" },
    { seq: 2, event_kind: "session_state_changed", session_id: "s-1", session_state: "working", created_at: "2026-06-07T12:04:00Z" },
    { seq: 3, event_kind: "ensure_author_job", from_phase: "up_next", to_phase: "authoring", actor: "owner:cli", created_at: "2026-06-07T12:01:00Z" },
  ];
  const statusLog = [
    { id: 9, kind: "progress", actor: "author", message: "editing files", created_at: "2026-06-07T12:02:00Z" },
  ];

  const html = context.renderTimeline({ sessions, transitions, statusLog });

  // All three types render inside the timeline feed.
  assert.match(html, /data-timeline-glyph="session"/);
  assert.match(html, /data-timeline-glyph="transition"/);
  assert.match(html, /data-timeline-glyph="status"/);
  // Session controls render from the top-N session list...
  assert.match(html, /data-terminal="s-1"/);
  assert.match(html, /data-session-transcript="s-1"/);
  // ...and a session_ready transition row enriched with a session_id also offers
  // a transcript control for that exact session.
  const transcriptButtons = html.match(/data-session-transcript="s-1"/g) || [];
  assert.ok(transcriptButtons.length >= 2, `expected at least 2 transcript buttons, got ${transcriptButtons.length}`);
  // Times are relative with the absolute value available on hover.
  assert.match(html, /<time title="[^"]+">/);
  // No raw session id as a headline and no issue id echoed.
  assert.doesNotMatch(html, /<strong>s-1<\/strong>/);

  // Entries interleave by time, newest first: s-1 (12:05), then
  // session_state_changed (12:04), session_ready (12:03), status (12:02),
  // ensure_author_job (12:01).
  const stateIdx = html.indexOf("session state");
  const readyIdx = html.indexOf("session ready");
  assert.ok(stateIdx > -1 && readyIdx > -1, "session transition rows should render");
  assert.ok(stateIdx < readyIdx, "newer session_state_changed should precede the older session_ready row");
});

test("unified timeline caps rendered rows and offers a show-more control", async () => {
  const context = await scriptContext();
  const sessions = [];
  const transitions = [];
  // Exceed TIMELINE_CAP (20) so the cap and hidden batch both engage.
  for (let i = 0; i < 25; i += 1) {
    transitions.push({
      seq: i + 1,
      event_kind: "ensure_author_job",
      from_phase: "up_next",
      to_phase: "authoring",
      actor: "owner:cli",
      created_at: `2026-06-07T12:${String(i).padStart(2, "0")}:00Z`,
    });
  }
  const html = context.renderTimeline({ sessions, transitions, statusLog: [] });
  assert.match(html, /data-timeline-show-more/);
  assert.match(html, /Show 5 more/);
  assert.match(html, /class="timeline-hidden" hidden/);
  // Only TIMELINE_CAP (20) rows render before the hidden batch; the remaining 5
  // sit inside the hidden container until "Show more" expands them.
  const [visible, hidden] = html.split("timeline-hidden");
  const visibleRows = (visible.match(/class="feed-item timeline-row"/g) || []).length;
  const hiddenRows = (hidden.match(/class="feed-item timeline-row"/g) || []).length;
  assert.equal(visibleRows, 20);
  assert.equal(hiddenRows, 5);
});

test("unified timeline renders an empty state when there is no activity", async () => {
  const context = await scriptContext();
  const html = context.renderTimeline({ sessions: [], transitions: [], statusLog: [] });
  assert.match(html, /No timeline activity yet/);
  assert.doesNotMatch(html, /data-timeline-show-more/);
});

test("unified timeline collapses consecutive session_state_changed rows for one session into a run", async () => {
  const context = await scriptContext();
  const transitions = [
    { seq: 3, event_kind: "session_state_changed", session_id: "s-1", session_state: "working", created_at: "2026-06-07T12:05:00Z" },
    { seq: 2, event_kind: "session_state_changed", session_id: "s-1", session_state: "waiting", created_at: "2026-06-07T12:04:00Z" },
    { seq: 1, event_kind: "session_state_changed", session_id: "s-1", session_state: "working", created_at: "2026-06-07T12:03:00Z" },
  ];
  const html = context.renderTimeline({ sessions: [], transitions, statusLog: [] });

  // The three same-session state changes collapse into a single summary row.
  assert.match(html, /data-timeline-run-toggle/);
  assert.match(html, /3 session state changes/);
  // The individual rows are hidden behind the toggle until expanded.
  assert.match(html, /class="timeline-run-rows" hidden/);
  const plainStateRows = (html.match(/<span class="badge idle session-state">session state<\/span>/g) || []).length;
  assert.ok(plainStateRows >= 3, "collapsed run should still render its child rows inside the hidden container");
});

test("unified timeline leaves a single session_state_changed row ungrouped", async () => {
  const context = await scriptContext();
  const transitions = [
    { seq: 1, event_kind: "session_state_changed", session_id: "s-1", session_state: "waiting", created_at: "2026-06-07T12:03:00Z" },
  ];
  const html = context.renderTimeline({ sessions: [], transitions, statusLog: [] });
  assert.doesNotMatch(html, /data-timeline-run-toggle/);
  assert.match(html, /session state/);
});

test("status entries render a kind badge", async () => {
  const context = await scriptContext();
  const blocker = context.renderStatus({
    actor: "author",
    message: "stuck on auth",
    kind: "blocker",
    created_at: "2026-06-07T12:00:00Z",
  });
  assert.match(blocker, /<span class="badge danger">blocker<\/span>/);
  assert.match(blocker, /stuck on auth/);

  const question = context.renderStatus({
    actor: "author",
    message: "which db?",
    kind: "question",
    created_at: "2026-06-07T12:00:00Z",
  });
  assert.match(question, /<span class="badge warn">question<\/span>/);

  const noKind = context.renderStatus({
    actor: "author",
    message: "default note",
    created_at: "2026-06-07T12:00:00Z",
  });
  assert.match(noKind, /<span class="badge idle">note<\/span>/);
});

test("feedback cards surface a non-note status kind badge", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = { id: "i-0001", title: "Blocked issue", schedule_state: "up_next", triage_state: "accepted" };
  const blockerCard = {
    active_session: { id: "s-0001", state: "waiting" },
    latest_status: { message: "stuck on auth", kind: "blocker" },
  };
  const blockerHTML = app.renderIssueCard(issue, blockerCard, "in_progress", false, 0, null, "question");
  assert.match(blockerHTML, /<span class="badge danger">blocker<\/span>/);
  assert.match(blockerHTML, /waiting for response/);

  const noteCard = { latest_status: { message: "running tests", kind: "note" } };
  const noteHTML = app.renderIssueCard(issue, noteCard, "in_progress", false, 0, null, "question");
  assert.doesNotMatch(noteHTML, /class="badge danger"/);
  assert.match(noteHTML, /running tests/);
});

test("crash-loop cards render retry action", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = { id: "i-0001", title: "Crash issue", schedule_state: "up_next", triage_state: "accepted" };
  const html = app.renderIssueCard(issue, {}, "up_next", false, 0, { id: "p-demo" }, "crash_loop");

  assert.match(html, /data-retry-crash="i-0001"/);
  assert.match(html, /\/ui\/projects\/p-demo\/issues\/i-0001/);
});

test("crash retry card action renders when another wait reason masks crash loop", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = { id: "i-0001", title: "Review issue", schedule_state: "up_next", triage_state: "accepted" };
  const card = { crash_retry_available: true };
  const html = app.renderIssueCard(issue, card, "in_review", false, 0, { id: "p-demo" }, "human_review");

  assert.match(html, /data-retry-crash="i-0001"/);
  assert.match(html, /waiting for human review/);
});

test("statusbar reflects poll state and interval", async () => {
  const timers = [];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  });
  const app = new context.FlowApp();
  const label = { textContent: "" };
  const meta = { textContent: "" };
  const bar = {
    dataset: {},
    querySelector: (selector) => (selector === ".sb-label" ? label : null),
  };
  app.querySelector = (selector) => {
    if (selector === ".statusbar") return bar;
    if (selector === ".sb-meta") return meta;
    return null;
  };

  app.setPollState("live", "live");
  assert.equal(bar.dataset.state, "live");
  assert.equal(label.textContent, "live");

  app.setPollState("error", "retry 3");
  assert.equal(bar.dataset.state, "error");
  assert.equal(label.textContent, "retry 3");

  app.pollFailures = 0;
  app.schedulePolling("/ui/jobs");
  assert.equal(meta.textContent, "poll 30s");

  app.schedulePolling("/ui/projects/p-alpha/issues/i-0001");
  assert.equal(meta.textContent, "");
});

test("worker and job state badges map states to status classes", async () => {
  const context = await scriptContext();

  assert.equal(context.renderStateBadge("ready"), `<span class="badge ok">ready</span>`);
  assert.equal(context.renderStateBadge("succeeded"), `<span class="badge ok">succeeded</span>`);
  assert.equal(context.renderStateBadge("failed"), `<span class="badge danger">failed</span>`);
  assert.equal(context.renderStateBadge("expired"), `<span class="badge danger">expired</span>`);
  assert.equal(context.renderStateBadge("running"), `<span class="badge run">running</span>`);
  assert.equal(context.renderStateBadge("claimed"), `<span class="badge idle">claimed</span>`);
  assert.equal(context.renderStateBadge("finished"), `<span class="badge ok">finished</span>`);
  assert.equal(context.renderStateBadge("crashed"), `<span class="badge danger">crashed</span>`);
  assert.equal(context.renderStateBadge("canceled"), `<span class="badge warn">canceled</span>`);
  assert.equal(context.renderStateBadge(""), "");

  assert.equal(context.jobStateClass("finished"), "ok");
  assert.equal(context.jobStateClass("failed"), "danger");
  assert.equal(context.jobStateClass("crashed"), "danger");
  assert.equal(context.jobStateClass("canceled"), "warn");
  assert.equal(context.jobStateClass("running"), "run");
  assert.equal(context.jobStateClass("claimed"), "idle");
  assert.equal(context.jobStateClass("queued"), "idle");

  assert.match(
    context.renderJobRow({ id: "j-0001", state: "finished", role: "author" }),
    /class="row-ok"/,
  );
  assert.match(
    context.renderJobRow({ id: "j-0001", state: "failed", role: "author" }),
    /class="row-danger"/,
  );
  assert.match(
    context.renderJobRow({ id: "j-0001", state: "canceled", role: "author" }),
    /class="row-warn"/,
  );

  assert.match(
    context.renderWorkerRow({ id: "w-local", status: "registered" }),
    /<td><span class="badge idle">registered<\/span><\/td>/,
  );
  assert.match(
    context.renderJobRow({ id: "j-0001", state: "running", role: "author" }),
    /<td><span class="badge run">running<\/span><\/td>/,
  );
});

test("jobs view shows project column, filters by project, and sorts by updated", async () => {
  const context = await scriptContext({}, {
    fetch(path) {
      assert.equal(path, "/ui/api/v1/jobs");
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          jobs: [
            // Intentionally out of updated order across two projects to prove
            // the view re-sorts globally rather than trusting server order.
            { id: "j-old", state: "finished", role: "ci", updated_at: "2026-06-01T00:00:00Z" },
            { id: "j-mid", state: "running", role: "author", updated_at: "2026-06-05T00:00:00Z" },
            { id: "j-new", state: "failed", role: "reviewer", updated_at: "2026-06-09T00:00:00Z" },
          ],
          diagnostics: {
            "j-old": { project_name: "beta" },
            "j-mid": { project_name: "alpha" },
            "j-new": { project_name: "beta" },
          },
        }),
      });
    },
  });

  const content = { innerHTML: "" };
  const app = new context.FlowApp();
  app.setTitle = () => {};
  app.bindIssueActions = () => {};
  app.isActiveLoad = () => true;
  app.querySelector = () => content;
  // Stub the per-view control listeners so change handlers do not blow up;
  // the table body is rendered into content.innerHTML up front.
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    return null;
  };

  await context.renderJobsView(app);

  const html = content.innerHTML;
  // Project column renders the per-job project name.
  assert.match(html, /<th>Project<\/th>/);
  assert.match(html, /alpha/);
  assert.match(html, /beta/);
  // Default sort is updated desc, so j-new (Jun 9) precedes j-mid (Jun 5)
  // which precedes j-old (Jun 1).
  const newIdx = html.indexOf("j-new");
  const midIdx = html.indexOf("j-mid");
  const oldIdx = html.indexOf("j-old");
  assert.ok(newIdx > -1 && midIdx > -1 && oldIdx > -1, "all job rows rendered");
  assert.ok(newIdx < midIdx, "j-new before j-mid");
  assert.ok(midIdx < oldIdx, "j-mid before j-old");
  // Filter and sort controls are present with the default selection.
  assert.match(html, /data-jobs-filter/);
  assert.match(html, /data-jobs-sort-field/);
  assert.match(html, /data-jobs-sort-order/);
  assert.match(html, /<option value="updated" selected>Updated<\/option>/);
  assert.match(html, /<option value="desc" selected>Newest first<\/option>/);
  // State colors render via row tint classes.
  assert.match(html, /class="row-ok"/);
  assert.match(html, /class="row-run"/);
  assert.match(html, /class="row-danger"/);
});

test("jobs view filter selects only the chosen project", async () => {
  const context = await scriptContext({}, {
    fetch(path) {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          jobs: [
            { id: "j-a", state: "running", role: "author", updated_at: "2026-06-05T00:00:00Z" },
            { id: "j-b", state: "running", role: "author", updated_at: "2026-06-09T00:00:00Z" },
          ],
          diagnostics: {
            "j-a": { project_name: "alpha" },
            "j-b": { project_name: "beta" },
          },
        }),
      });
    },
  });

  const content = { innerHTML: "" };
  const app = new context.FlowApp();
  app.setTitle = () => {};
  app.bindIssueActions = () => {};
  app.isActiveLoad = () => true;
  app.querySelector = (selector) => (selector === ".content" ? content : null);

  // Pretend the user picked the "beta" project filter before this render.
  app.jobsView = { filter: "beta", sort: { field: "updated", order: "desc" } };
  await context.renderJobsView(app);

  const html = content.innerHTML;
  assert.match(html, /j-b/);
  assert.doesNotMatch(html, /j-a/);
  // The beta option is the selected one.
  assert.match(html, /<option value="beta" selected>beta<\/option>/);
});

test("check verdict badges map verdicts to status classes with pending fallback", async () => {
  const context = await scriptContext();

  assert.equal(context.renderVerdictBadge("satisfied"), `<span class="badge ok">satisfied</span>`);
  assert.equal(context.renderVerdictBadge("blocked"), `<span class="badge danger">blocked</span>`);
  assert.equal(context.renderVerdictBadge("failed"), `<span class="badge danger">failed</span>`);
  assert.equal(context.renderVerdictBadge("rejected"), `<span class="badge danger">rejected</span>`);
  assert.equal(context.renderVerdictBadge("needs_rerun"), `<span class="badge idle">needs rerun</span>`);
  assert.equal(context.renderVerdictBadge(""), `<span class="badge idle">pending</span>`);
});

test("board lanes carry data-lane attributes for the lane accent system", async () => {
  const harness = await browserSmokeHarness("/ui/board", {
    "/ui/api/v1/board": {
      boards: [{
        project_id: "p-alpha",
        project_name: "alpha",
        board: {
          backlog: [{
            id: "i-0001",
            title: "Backlog issue",
            schedule_state: "backlog",
            triage_state: "accepted",
            priority: 1,
          }],
        },
        lane_states: { "i-0001": "backlog" },
        issue_cards: {},
      }],
    },
  });

  await harness.app.load();

  for (const lane of ["backlog", "up_next", "in_progress", "needs_attention"]) {
    assert.match(harness.content.innerHTML, new RegExp(`<section class="lane" data-lane="${lane}">`));
  }
});

test("load marks content as nav or poll refresh and reports live poll state", async () => {
  const harness = await browserSmokeHarness("/ui/board", {
    "/ui/api/v1/board": { boards: [{ project_id: "p-alpha", project_name: "alpha", board: {}, lane_states: {}, issue_cards: {} }] },
  });

  await harness.app.load();
  assert.equal(harness.content.dataset.refresh, "nav");
  assert.equal(harness.statusbar.dataset.state, "live");
  assert.equal(harness.sbLabel.textContent, "live");
  assert.equal(harness.sbMeta.textContent, "poll 10s");

  await harness.app.load({ fromPoll: true });
  assert.equal(harness.content.dataset.refresh, "poll");
});

test("non-polling routes report static instead of live", async () => {
  const harness = await browserSmokeHarness("/ui/issues/new", {});

  await harness.app.load();

  assert.equal(harness.statusbar.dataset.state, "idle");
  assert.equal(harness.sbLabel.textContent, "static");
  assert.equal(harness.sbMeta.textContent, "");
  assert.deepEqual(harness.fetchCalls, ["/ui/api/v1/projects", "/ui/api/v1/harnesses"]);
});

test("unscoped issue detail route requires a project-scoped URL", async () => {
  const harness = await browserSmokeHarness("/ui/issues/i-0001", {});

  await harness.app.load();

  assert.equal(harness.title.textContent, "Issue");
  assert.match(harness.content.innerHTML, /Project-scoped issue URL required/);
  assert.match(harness.content.innerHTML, /\/ui\/projects\/&lt;project-id&gt;\/issues\/&lt;issue-id&gt;/);
  assert.deepEqual(harness.fetchCalls, ["/ui/api/v1/projects"]);
});

test("load failures surface error then retry state in the statusbar", async () => {
  const harness = await browserSmokeHarness("/ui/board", {});

  await harness.app.load();
  assert.match(harness.status.textContent, /missing smoke response/);
  assert.equal(harness.statusbar.dataset.state, "error");
  assert.equal(harness.sbLabel.textContent, "error");

  await harness.app.load({ fromPoll: true });
  assert.equal(harness.statusbar.dataset.state, "error");
  assert.equal(harness.sbLabel.textContent, "retry 2");

  harness.fetchCalls.length = 0;
});

test("feedback cards render handoff summaries", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const issue = {
    id: "i-0001",
    title: "Needs feedback",
    schedule_state: "up_next",
    triage_state: "accepted",
    priority: 1,
  };
  const card = {
    active_session: { id: "s-0001", state: "waiting", branch: "issue/i-0001" },
    terminal_available: true,
    latest_status: { message: "Waiting on product decision" },
    handoff: {
      present: true,
      valid: true,
      summary: "Waiting for product decision before final polish.",
    },
  };
  const html = app.renderIssueCard(issue, card, "in_progress", false, 0, null, "question");
  const inProgressHTML = app.renderIssueCard(issue, card, "in_progress", false);

  assert.match(html, /Waiting on product decision/);
  assert.match(html, /handoff: Waiting for product decision before final polish\./);
  assert.match(html, /data-terminal="s-0001"/);
  assert.doesNotMatch(inProgressHTML, /handoff: Waiting for product decision before final polish\./);
});

test("browser smoke loads board and inbox direct routes", async () => {
  for (const route of [
    {
      path: "/ui/board",
      title: "Board",
      present: [/Backlog issue/, /Discovered issue/, /Waiting issue/, /Merge issue/],
      absent: [],
      activeHref: "/ui/board",
    },
    {
      path: "/ui/",
      title: "Board",
      present: [/Backlog issue/, /Discovered issue/, /Waiting issue/, /Merge issue/],
      absent: [],
      activeHref: "/ui/board",
    },
    {
      path: "/ui/triage",
      title: "Triage",
      present: [/Discovered issue/],
      absent: [/Backlog issue/, /Waiting issue/, /Merge issue/],
      activeHref: "/ui/triage",
    },
    {
      path: "/ui/feedback",
      title: "Needs Attention",
      present: [/Waiting issue/, /Merge issue/],
      absent: [/Backlog issue/, /Discovered issue/],
      activeHref: "/ui/feedback",
    },
    {
      path: "/ui/merge",
      title: "Merge",
      present: [/Merge issue/],
      absent: [/Backlog issue/, /Discovered issue/, /Waiting issue/],
      activeHref: "/ui/merge",
    },
  ]) {
    const harness = await browserSmokeHarness(route.path, {
      "/ui/api/v1/board": {
        boards: [{
          project_id: "p-alpha",
          project_name: "alpha",
          board: {
            backlog: [{
              id: "i-0001",
              title: "Backlog issue",
              schedule_state: "backlog",
              triage_state: "accepted",
              priority: 1,
            }, {
              id: "i-0002",
              title: "Discovered issue",
              schedule_state: "backlog",
              triage_state: "triage",
              priority: 2,
              created_by: "agent",
            }],
            needs_attention: [{
              id: "i-0003",
              title: "Waiting issue",
              schedule_state: "up_next",
              triage_state: "accepted",
              priority: 1,
            }, {
              id: "i-0004",
              title: "Merge issue",
              schedule_state: "accepted",
              triage_state: "accepted",
              priority: 1,
            }],
          },
          lane_states: {
            "i-0001": "backlog",
            "i-0002": "triage",
            "i-0003": "in_progress",
            "i-0004": "ready_to_merge",
          },
          wait_reasons: {
            "i-0003": "question",
          },
          issue_cards: {
            "i-0001": { tags: [{ slug: "planned" }] },
            "i-0002": { tags: [{ slug: "agent" }] },
            "i-0003": {
              active_session: { id: "s-0003", state: "waiting" },
              latest_status: { message: "Need a decision", kind: "question" },
            },
            "i-0004": {
              change: { id: "ch-0004", branch: "issue/i-0004", head_sha: "abcdef1234567890" },
              diff_stats: { total_files: 1, additions: 2, deletions: 0 },
              review_state: "approved",
            },
          },
        }],
      },
    });

    await harness.app.load();

    assert.equal(harness.title.textContent, route.title);
    for (const pattern of route.present) assert.match(harness.content.innerHTML, pattern);
    for (const pattern of route.absent) assert.doesNotMatch(harness.content.innerHTML, pattern);
    assert.equal(harness.activeNavHref(), route.activeHref);
    // The unfiltered board (showDone) also fetches the Done lane preview; the
    // filtered inbox routes (triage/feedback/merge) do not.
    const expectedCalls = ["/ui/api/v1/projects", "/ui/api/v1/board"];
    if (route.path === "/ui/board" || route.path === "/ui/") {
      expectedCalls.push("/ui/api/v1/done?limit=20");
    }
    assert.deepEqual(harness.fetchCalls, expectedCalls);
    assert.equal(harness.status.textContent, "");
  }
});

test("browser smoke loads issue and change deep links", async () => {
  const issueHarness = await browserSmokeHarness("/ui/projects/p-alpha/issues/i-0001", {
    "/ui/api/v1/projects/p-alpha/issues/i-0001": {
      project_id: "p-alpha",
      issue: {
        id: "i-0001",
        title: "Issue detail",
        body: "Issue body",
        acceptance_criteria: "Done",
        schedule_state: "up_next",
        triage_state: "accepted",
        priority: 3,
        requires_human_review: false,
        auto_merge: true,
        created_by: "human",
        updated_at: "2026-06-07T12:00:00Z",
      },
      issue_detail: {
        tags: [{ slug: "web-ui" }],
        required_checks: { total: 1, satisfied: 1 },
        review_state: "approved",
        checks: [{ name: "unit", kind: "ci", required: true, verdict: "satisfied" }],
      },
      status_log: [{ message: "Ready for review", created_at: "2026-06-07T12:01:00Z" }],
    },
  });

  await issueHarness.app.load();

  assert.equal(issueHarness.title.textContent, "Issue");
  assert.match(issueHarness.content.innerHTML, /Issue detail/);
  assert.match(issueHarness.content.innerHTML, /web-ui/);
  assert.deepEqual(issueHarness.fetchCalls, ["/ui/api/v1/projects", "/ui/api/v1/projects/p-alpha/issues/i-0001", "/ui/api/v1/harnesses"]);

  const changeHarness = await browserSmokeHarness("/ui/changes/ch-0001", {
    "/ui/api/v1/changes/ch-0001": {
      change: {
        id: "ch-0001",
        branch: "issue/i-0001",
        base: "main",
        head_sha: "abcdef1234567890",
        updated_at: "2026-06-07T12:00:00Z",
      },
      issue: { id: "i-0001", title: "Change detail" },
      checks: [{ name: "unit", kind: "ci", required: true, verdict: "satisfied" }],
      required_checks: { total: 1, satisfied: 1 },
      review_state: "approved",
      can_merge: true,
      threads: [{
        state: "open",
        file_path: "app.go",
        line: 4,
        anchor_commit_sha: "abcdef1234567890",
        context: "const value = 1",
        comments: [{ body: "Review note" }],
      }],
    },
    "/ui/api/v1/changes/ch-0001/diff": {
      change_id: "ch-0001",
      head_sha: "abcdef1234567890",
      available: true,
      total_files: 1,
      additions: 4,
      deletions: 1,
      files: [{ path: "app.go", additions: 4, deletions: 1 }],
    },
  });

  await changeHarness.app.load();

  assert.equal(changeHarness.title.textContent, "Change");
  assert.match(changeHarness.content.innerHTML, /Change detail/);
  assert.match(changeHarness.content.innerHTML, /Review note/);
  assert.match(changeHarness.diffContainer("ch-0001").innerHTML, /files 1/);
  assert.match(changeHarness.diffContainer("ch-0001").innerHTML, /app.go/);
  assert.deepEqual(changeHarness.fetchCalls, [
    "/ui/api/v1/projects",
    "/ui/api/v1/changes/ch-0001",
    "/ui/api/v1/changes/ch-0001/diff",
  ]);
});

test("polling policy matches board, diagnostics, and change routes", async () => {
  const context = await scriptContext();

  assert.deepEqual(normalize(context.pollConfigForPath("/ui/")), {
    interval: 10000,
    maxInterval: 10000,
    backoff: false,
  });
  assert.deepEqual(normalize(context.pollConfigForPath("/ui/board")), {
    interval: 10000,
    maxInterval: 10000,
    backoff: false,
  });
  assert.deepEqual(normalize(context.pollConfigForPath("/ui/triage")), {
    interval: 10000,
    maxInterval: 10000,
    backoff: false,
  });
  assert.deepEqual(normalize(context.pollConfigForPath("/ui/changes/ch-0001")), {
    interval: 15000,
    maxInterval: 15000,
    backoff: false,
  });
  assert.deepEqual(normalize(context.pollConfigForPath("/ui/jobs")), {
    interval: 30000,
    maxInterval: 120000,
    backoff: true,
  });
  assert.equal(context.pollConfigForPath("/ui/projects/p-alpha/issues/i-0001"), null);
});

test("diagnostics polling backs off and clears prior timer", async () => {
  const timers = [];
  const cleared = [];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout(id) {
      cleared.push(id);
    },
  });
  const app = new context.FlowApp();

  app.pollFailures = 0;
  app.schedulePolling("/ui/jobs");
  assert.equal(timers[0].delay, 30000);

  app.clearPolling();
  assert.deepEqual(cleared, [1]);
  assert.equal(app.mainPoll.timer, 0);

  app.pollFailures = 3;
  app.schedulePolling("/ui/jobs");
  assert.equal(timers[1].delay, 120000);

  app.pollFailures = 5;
  app.schedulePolling("/ui/merge");
  assert.equal(timers[2].delay, 10000);
  assert.deepEqual(cleared, [1, 2]);
});

test("pollDelay applies capped exponential backoff", async () => {
  const { pollDelay } = await scriptContext();
  assert.equal(pollDelay(30000, 0, 120000), 30000); // no failures -> base interval
  assert.equal(pollDelay(30000, 1, 120000), 60000); // one failure -> doubled
  assert.equal(pollDelay(30000, 2, 120000), 120000); // would be 120000, at the cap
  assert.equal(pollDelay(30000, 3, 120000), 120000); // capped, not 240000
  assert.equal(pollDelay(10000, 0, 120000), 10000); // backoff disabled -> base
});

test("sidebar status refresh renders live nav badges and polls", async () => {
  const timers = [];
  const fetchCalls = [];
  const nav = new SmokeNav();
  const refresh = new SmokeElement();
  const newIssue = new SmokeElement();

  class SidebarHTMLElement extends SmokeElement {
    querySelector(selector) {
      if (selector === ".nav") return nav;
      if (selector === '[data-action="refresh"]') return refresh;
      if (selector === '[data-action="new-issue"]') return newIssue;
      return new SmokeElement();
    }

    querySelectorAll(selector) {
      if (selector === ".nav a") return nav.links;
      if (selector === "[data-theme-option]") return [];
      return [];
    }
  }

  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    HTMLElement: SidebarHTMLElement,
    fetch(path) {
      fetchCalls.push(path);
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
          triage: 3,
          feedback: 4,
          merge: 5,
          workers: { in_use: 2, capacity: 5 },
          jobs: { active: 6, queued: 7 },
        }),
      });
    },
  });
  const app = new context.FlowApp();
  app.sidebarStatusPollingActive = true;
  app.renderShell();

  await app.refreshSidebarStatus();

  assert.deepEqual(fetchCalls, ["/ui/api/v1/sidebar"]);
  assert.match(nav.innerHTML, /title="3 triage items">3<\/span>/);
  assert.match(nav.innerHTML, /title="4 needs attention items">4<\/span>/);
  assert.match(nav.innerHTML, /title="5 merge items">5<\/span>/);
  assert.match(nav.innerHTML, /title="2 in use of 5 worker slots">2\/5<\/span>/);
  assert.match(nav.innerHTML, /data-job-status="active">6<\/span>/);
  assert.match(nav.innerHTML, /data-job-status="queued">7<\/span>/);
  assert.equal(timers[0].delay, 10000);
});

test("stale poll load does not repaint issue route or rearm board polling", async () => {
  const timers = [];
  const status = { textContent: "" };
  const title = { textContent: "" };
  const content = { innerHTML: "issue edit form" };
  const boardResponse = deferred();
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    fetch(path) {
      assert.equal(path, "/ui/api/v1/board");
      return boardResponse.promise;
    },
  });
  context.window.location.pathname = "/ui/";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    if (selector === ".status") return status;
    if (selector === "h1") return title;
    return { textContent: "" };
  };

  const loadPromise = app.load({ fromPoll: true });
  context.window.location.pathname = "/ui/projects/p-alpha/issues/i-0001";
  boardResponse.resolve({
    ok: true,
    json: () => Promise.resolve({ board: { backlog: [{ id: "i-0002", title: "Board issue" }] } }),
  });
  await loadPromise;

  assert.equal(content.innerHTML, "issue edit form");
  assert.equal(title.textContent, "");
  assert.equal(timers.length, 0);
  assert.equal(status.textContent, "");
});

test("disconnect during pending load prevents polling rearm", async () => {
  const timers = [];
  const jobsResponse = deferred();
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    fetch(path) {
      assert.equal(path, "/ui/api/v1/jobs");
      return jobsResponse.promise;
    },
  });
  context.window.location.pathname = "/ui/jobs";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = () => ({ textContent: "", innerHTML: "" });

  const loadPromise = app.load({ fromPoll: true });
  app.disconnectedCallback();
  jobsResponse.resolve({
    ok: true,
    json: () => Promise.resolve({ jobs: [] }),
  });
  await loadPromise;

  assert.equal(timers.length, 0);
});

test("connected callback preserves monotonic load generation", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  let loadCalled = false;
  app.loadGeneration = 7;
  app.renderShell = () => {};
  app.load = () => {
    loadCalled = true;
  };

  app.connectedCallback();

  assert.equal(loadCalled, true);
  assert.equal(app.loadGeneration, 7);
});

test("pre-disconnect load stays stale after reconnect-style load", async () => {
  const timers = [];
  const content = { innerHTML: "" };
  const oldJobs = deferred();
  const newJobs = deferred();
  const responses = [oldJobs.promise, newJobs.promise];
  const context = await scriptContext({
    setTimeout(callback, delay) {
      timers.push({ callback, delay });
      return timers.length;
    },
    clearTimeout() {},
  }, {
    fetch(path) {
      assert.equal(path, "/ui/api/v1/jobs");
      const response = responses.shift();
      if (!response) throw new Error("unexpected fetch");
      return response;
    },
  });
  context.window.location.pathname = "/ui/jobs";
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.querySelectorAll = () => [];
  app.querySelector = (selector) => {
    if (selector === ".content") return content;
    return { textContent: "", innerHTML: "" };
  };

  const oldLoad = app.load({ fromPoll: true });
  app.disconnectedCallback();
  app.pollingActive = true;
  const newLoad = app.load();
  oldJobs.resolve({
    ok: true,
    json: () => Promise.resolve({ jobs: [{ id: "old-job", state: "running" }] }),
  });
  await oldLoad;
  assert.equal(content.innerHTML, "");
  assert.equal(timers.length, 0);

  newJobs.resolve({
    ok: true,
    json: () => Promise.resolve({ jobs: [] }),
  });
  await newLoad;
  assert.match(content.innerHTML, /No jobs/);
  assert.equal(timers[0].delay, 30000);
});

async function newIssueRouteHarness(options = {}) {
  const title = { textContent: "" };
  const status = { textContent: "" };
  const content = { innerHTML: "" };
  const fetchCalls = [];
  const storage = new Map();
  if (options.storedDefaultsRaw !== undefined) {
    storage.set(ISSUE_AGENT_DEFAULTS_STORAGE_KEY, options.storedDefaultsRaw);
  } else if (options.storedDefaults) {
    storage.set(ISSUE_AGENT_DEFAULTS_STORAGE_KEY, JSON.stringify(options.storedDefaults));
  }
  const localStorage = options.localStorage || {
    getItem(key) {
      return storage.has(key) ? storage.get(key) : null;
    },
    setItem(key, value) {
      storage.set(key, String(value));
    },
    removeItem(key) {
      storage.delete(key);
    },
  };
  const harnesses = options.harnesses || {
    agents: [
      { name: "codex", display_name: "Codex", default_args: ["--model", "gpt-5"] },
      {
        name: "harness",
        display_name: "Harness",
        models: [{
          provider_id: "anthropic",
          provider_name: "Anthropic",
          model_id: "claude-opus-4-8",
          qualified_id: "anthropic:claude-opus-4-8",
          model_name: "Claude Opus 4.8",
          reasoning: { supported: true, options: [{ type: "effort", values: ["low", "high"] }] },
        }],
      },
    ],
    consoles: [],
  };
  const context = await scriptContext({
    location: { pathname: "/ui/issues/new" },
    localStorage,
  }, {
    fetch(path, fetchOptions) {
      fetchCalls.push({ path, options: fetchOptions });
      if (path === "/ui/api/v1/projects") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            projects: [
              { id: "p-alpha", name: "alpha" },
              { id: "p-beta", name: "beta" },
            ],
          }),
        });
      }
      if (path === "/ui/api/v1/harnesses") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve(harnesses),
        });
      }
      throw new Error("new issue route should not fetch before submission");
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.renderProjectPicker = () => {};
  app.querySelector = (selector) => {
    if (selector === "h1") return title;
    if (selector === ".status") return status;
    if (selector === ".content") return content;
    return { textContent: "" };
  };
  app.querySelectorAll = () => [];

  return { app, content, fetchCalls, status, storage, title };
}

async function issueSaveHarness(options = {}) {
  let submitHandler;
  let saveDefaultsHandler;
  const mode = options.mode || "edit";
  const projectID = options.projectID ?? (mode === "create" ? "p-demo" : "");
  const saveDefaultsButton = {
    addEventListener(event, handler) {
      if (event === "click") saveDefaultsHandler = handler;
    },
  };
  const form = {
    dataset: { issueForm: mode === "create" ? "" : "i-0001", issueFormMode: mode },
    elements: {
      title: { value: options.title ?? "Updated issue" },
      body: { value: "New body" },
      acceptance_criteria: { value: "New criteria" },
      priority: { value: options.priority ?? "4" },
      requires_human_review: { checked: false },
      auto_merge: { checked: true },
      agent_harness: { value: options.agentHarness || "claude" },
      agent_args: {
        value: options.agentArgs || "",
        dataset: options.agentArgsValues ? { agentArgsValues: JSON.stringify(options.agentArgsValues) } : {},
      },
      queue_issue: { checked: options.queueIssue !== false },
    },
    reportValidity() {
      return options.valid !== false;
    },
    addEventListener(event, handler) {
      if (event === "submit") submitHandler = handler;
    },
    querySelector(selector) {
      if (selector === "[data-save-agent-defaults]") return mode === "create" ? saveDefaultsButton : null;
      return null;
    },
  };
  if (mode === "create") {
    form.elements.project = { value: projectID };
    form.elements.plan_mode = { checked: options.planMode === true };
    form.elements.attachments = { files: options.files || [] };
  } else if (projectID) {
    form.dataset.project = projectID;
  }
  if (options.harnessModel) {
    form.elements.harness_provider = { value: options.harnessProvider || "" };
    form.elements.harness_model = { value: options.harnessModel };
    form.elements.harness_reasoning_mode = { value: options.harnessReasoningMode || "default" };
    form.elements.harness_reasoning_effort = { value: options.harnessReasoningEffort || "" };
    form.elements.harness_reasoning_budget_tokens = { value: options.harnessReasoningBudget || "" };
  }
  const status = { textContent: "" };
  const fetchCalls = [];
  const storage = new Map();
  let pushedPath = "";
  let loads = 0;
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/" },
      addEventListener() {},
      localStorage: {
        getItem(key) {
          return storage.has(key) ? storage.get(key) : null;
        },
        setItem(key, value) {
          storage.set(key, String(value));
        },
        removeItem(key) {
          storage.delete(key);
        },
      },
      open() {
        throw new Error("window.open should not be used for issue save");
      },
    },
    fetch(path, fetchOptions) {
      fetchCalls.push({ path, options: fetchOptions });
      return Promise.resolve({
        ok: options.fetchOK !== false,
        json: () => Promise.resolve(options.fetchOK === false
          ? { error: { message: options.errorMessage || "request failed" } }
          : { issue: options.responseIssue || { id: "i-0001" } }),
      });
    },
    FormData: class {
      constructor() {
        this.fields = [];
      }
      set(name, value, filename) {
        this.fields.push({ name, value, filename });
      }
    },
    console,
  };
  context.history.pushState = (_state, _title, path) => {
    pushedPath = path;
  };

  await applyContext(context);

  const flowApp = new context.FlowApp();
  if (options.harnesses) {
    flowApp.harnesses = options.harnesses;
  }
  flowApp.querySelectorAll = (selector) => (selector === "[data-issue-form]" ? [form] : []);
  flowApp.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  flowApp.bindIssueActions(async () => {
    refreshed = true;
  });
  flowApp.load = async () => {
    loads += 1;
  };

  return {
    fetchCalls,
    status,
    storage,
    refreshed: () => refreshed,
    pushedPath: () => pushedPath,
    loads: () => loads,
    saveDefaults: () => saveDefaultsHandler(),
    submit: () => submitHandler({ preventDefault() {} }),
  };
}

async function createIssueHarness() {
  const status = { textContent: "" };
  const fetchCalls = [];
  let pushedPath = "";
  let loads = 0;
  const context = await scriptContext({}, {
    history: {
      pushState(_state, _title, path) {
        pushedPath = path;
      },
    },
    fetch(path, fetchOptions) {
      fetchCalls.push({ path, options: fetchOptions });
      throw new Error("new issue action should not fetch before submission");
    },
  });
  const app = new context.FlowApp();
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  app.load = async () => {
    loads += 1;
  };

  return {
    fetchCalls,
    status,
    create: () => app.createIssue(),
    pushedPath: () => pushedPath,
    loads: () => loads,
  };
}

async function triageEditHarness(options = {}) {
  let clickHandler;
  const button = {
    dataset: { issueEdit: "i-0001", issueTitle: "Old title" },
    addEventListener(event, handler) {
      if (event === "click") clickHandler = handler;
    },
  };
  const status = { textContent: "" };
  const fetchCalls = [];
  const context = await scriptContext({
    prompt(message, initial) {
      assert.equal(message, "Title");
      assert.equal(initial, "Old title");
      return options.promptValue;
    },
  }, {
    fetch(path, fetchOptions) {
      fetchCalls.push({ path, options: fetchOptions });
      return Promise.resolve({
        ok: options.fetchOK !== false,
        json: () => Promise.resolve(options.fetchOK === false
          ? { error: { message: options.errorMessage || "request failed" } }
          : { issue: { id: "i-0001" } }),
      });
    },
  });
  const app = new context.FlowApp();
  app.querySelectorAll = (selector) => (selector === "[data-issue-edit]" ? [button] : []);
  app.querySelector = (selector) => (selector === ".status" ? status : { textContent: "" });
  let refreshed = false;
  app.bindIssueActions(async () => {
    refreshed = true;
  });

  return {
    fetchCalls,
    status,
    refreshed: () => refreshed,
    click: () => clickHandler(),
  };
}

async function browserSmokeHarness(path, responses) {
  const title = new SmokeElement();
  const status = new SmokeElement();
  const content = new SmokeElement();
  const refresh = new SmokeElement();
  const nav = new SmokeNav();
  const statusbar = new SmokeElement();
  const sbLabel = new SmokeElement();
  const sbMeta = new SmokeElement();
  statusbar.querySelector = (selector) => (selector === ".sb-label" ? sbLabel : null);
  const diffContainers = new Map();
  const fetchCalls = [];

  class SmokeHTMLElement extends SmokeElement {
    querySelector(selector) {
      if (selector === "h1") return title;
      if (selector === ".status") return status;
      if (selector === ".content") return content;
      if (selector === ".nav") return nav;
      if (selector === ".statusbar") return statusbar;
      if (selector === ".sb-meta") return sbMeta;
      if (selector === '[data-action="refresh"]') return refresh;
      if (selector.startsWith("[data-change-diff=")) {
        const id = selector.match(/"([^"]+)"/)?.[1] || selector;
        if (!diffContainers.has(id)) diffContainers.set(id, new SmokeElement());
        return diffContainers.get(id);
      }
      return new SmokeElement();
    }

    querySelectorAll(selector) {
      if (selector === ".nav a") return nav.links;
      return [];
    }
  }

  const context = await scriptContext({
    location: { pathname: path },
    setTimeout() {
      return 1;
    },
    clearTimeout() {},
  }, {
    HTMLElement: SmokeHTMLElement,
    fetch(requestPath) {
      fetchCalls.push(requestPath);
      if (requestPath === "/ui/api/v1/projects" && !(requestPath in responses)) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [] }),
        });
      }
      if (requestPath === "/ui/api/v1/harnesses" && !(requestPath in responses)) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ agents: [], consoles: [] }),
        });
      }
      if (!(requestPath in responses)) {
        return Promise.resolve({
          ok: false,
          json: () => Promise.resolve({ error: { message: `missing smoke response for ${requestPath}` } }),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve(responses[requestPath]),
      });
    },
  });
  const app = new context.FlowApp();
  app.pollingActive = true;
  app.renderShell();

  return {
    app,
    title,
    status,
    content,
    statusbar,
    sbLabel,
    sbMeta,
    fetchCalls,
    activeNavHref() {
      return nav.links.find((link) => link.attributes.get("aria-current") === "page")?.href || "";
    },
    diffContainer(id) {
      return diffContainers.get(id) || new SmokeElement();
    },
  };
}

class SmokeElement {
  constructor() {
    this.innerHTML = "";
    this.textContent = "";
    this.dataset = {};
    this.attributes = new Map();
    this.listeners = new Map();
  }

  addEventListener(event, handler) {
    this.listeners.set(event, handler);
  }

  setAttribute(name, value) {
    this.attributes.set(name, value);
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  querySelector() {
    return null;
  }

  querySelectorAll() {
    return [];
  }
}

class SmokeNav extends SmokeElement {
  constructor() {
    super();
    this.links = [];
  }

  set innerHTML(html) {
    this._innerHTML = html;
    this.links = [...String(html).matchAll(/href="([^"]+)"/g)].map((match) => new SmokeLink(match[1]));
  }

  get innerHTML() {
    return this._innerHTML || "";
  }

  querySelectorAll(selector) {
    return selector === "a" ? this.links : [];
  }
}

class SmokeLink extends SmokeElement {
  constructor(href) {
    super();
    this.href = href;
  }

  getAttribute(name) {
    return name === "href" ? this.href : "";
  }
}

function inlineDocument() {
  return {
    cookie: "flow_ui_csrf=csrf-token",
    addEventListener() {},
    createElement(tagName) {
      return new InlineDOMElement(tagName);
    },
  };
}

class InlineDOMElement extends SmokeElement {
  constructor(tagName = "div") {
    super();
    this.tagName = String(tagName).toUpperCase();
    this.className = "";
    this.children = [];
    this.parentElement = null;
    this.previousElementSibling = null;
    this.nextElementSibling = null;
    this.cells = [];
    this.colSpan = 0;
  }

  appendChild(child) {
    child.parentElement = this;
    this.children.push(child);
    return child;
  }

  remove() {
    if (this.previousElementSibling) this.previousElementSibling.nextElementSibling = this.nextElementSibling;
    if (this.nextElementSibling) this.nextElementSibling.previousElementSibling = this.previousElementSibling;
    if (this.parentElement?.children) {
      const index = this.parentElement.children.indexOf(this);
      if (index >= 0) this.parentElement.children.splice(index, 1);
    }
    this.parentElement = null;
    this.previousElementSibling = null;
    this.nextElementSibling = null;
  }

  after(element) {
    element.parentElement = this.parentElement;
    element.previousElementSibling = this;
    element.nextElementSibling = this.nextElementSibling;
    this.nextElementSibling = element;
  }

  querySelector(selector) {
    if (selector === "[data-inline-terminal]") return findInlineTerminal(this);
    return null;
  }

  querySelectorAll(selector) {
    if (selector === "td, th") return this.cells;
    return [];
  }
}

class RepaintingInlineDOMElement extends InlineDOMElement {
  set innerHTML(html) {
    this._innerHTML = String(html);
    if (!this.children) return;
    for (const child of this.children) child.parentElement = null;
    this.children = [];
  }

  get innerHTML() {
    return this._innerHTML || "";
  }
}

function findInlineTerminal(element) {
  if (element.dataset?.inlineTerminal === "true") return element;
  for (const child of element.children || []) {
    const match = findInlineTerminal(child);
    if (match) return match;
  }
  return null;
}

async function themeShellHarness(storedTheme = "") {
  const storage = new Map();
  if (storedTheme) storage.set("flow.ui.theme", storedTheme);
  const rootAttributes = new Map();
  const themeButtons = {};
  for (const option of ["system", "light", "dark"]) {
    const button = new SmokeElement();
    button.dataset.themeOption = option;
    themeButtons[option] = button;
  }
  const refresh = new SmokeElement();
  const newIssue = new SmokeElement();
  const nav = new SmokeNav();

  class ThemeHTMLElement extends SmokeElement {
    querySelector(selector) {
      if (selector === ".nav") return nav;
      if (selector === '[data-action="refresh"]') return refresh;
      if (selector === '[data-action="new-issue"]') return newIssue;
      return new SmokeElement();
    }

    querySelectorAll(selector) {
      if (selector === ".nav a") return nav.links;
      if (selector === "[data-theme-option]") return Object.values(themeButtons);
      return [];
    }
  }

  const context = await scriptContext({
    localStorage: {
      getItem(key) {
        return storage.has(key) ? storage.get(key) : null;
      },
      setItem(key, value) {
        storage.set(key, String(value));
      },
      removeItem(key) {
        storage.delete(key);
      },
    },
  }, {
    HTMLElement: ThemeHTMLElement,
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
      documentElement: {
        setAttribute(name, value) {
          rootAttributes.set(name, value);
        },
        removeAttribute(name) {
          rootAttributes.delete(name);
        },
      },
    },
  });

  return {
    app: new context.FlowApp(),
    rootAttributes,
    storage,
    themeButtons,
    pressedThemes() {
      return Object.entries(themeButtons)
        .filter(([, button]) => button.attributes.get("aria-pressed") === "true")
        .map(([option]) => option);
    },
  };
}

let appLoadCount = 0;
function loadAppModule() {
  // Import a fresh entry-module instance per call (cache-busting query) so
  // `class FlowApp extends HTMLElement` re-binds to THIS test's globalThis
  // .HTMLElement — tests like themeShellHarness inject a custom HTMLElement
  // subclass to give the FlowApp instance querySelector/querySelectorAll. The
  // old vm sandbox re-evaluated the source per test; this reproduces that.
  // Pure submodules imported by app.js use unqueried specifiers, so they load
  // once and stay shared.
  appLoadCount += 1;
  return import(`./app.js?test=${appLoadCount}`);
}

// Native-ESM replacement for the old vm sandbox. app.js reads `fetch` as a bare
// global and everything else through `window`/`document`/`customElements`/
// `history`/`HTMLElement`, so install the per-test stubs as real globals, then
// dynamic-import a fresh entry module and copy its exports onto `context` so
// existing `context.X` call-sites keep working. The entry's load-time side
// effects (customElements.define no-op stub, document listeners) re-run per
// import against the current stubs; node:test runs top-level tests sequentially,
// so the per-test global assignment below is race-free.
const CORE_GLOBAL_KEYS = new Set([
  "HTMLElement", "customElements", "document", "history", "window", "fetch",
]);

async function applyContext(context) {
  // Reset the core stubs to the provided value or a safe default on every call,
  // so nothing leaks between sequential tests.
  globalThis.HTMLElement = context.HTMLElement ?? class {};
  globalThis.customElements = context.customElements ?? { define() {} };
  globalThis.document = context.document ?? { cookie: "", addEventListener() {} };
  globalThis.history = context.history ?? { pushState() {} };
  globalThis.window = context.window ?? {};
  globalThis.fetch = context.fetch ?? (() => {
    throw new Error("fetch should not be used");
  });
  // Expose any extra stubs the test supplies (e.g. FormData) as bare globals,
  // matching the old vm sandbox where the whole context object was the global
  // scope. app.js reads these (new FormData(), etc.) off the global.
  for (const [key, value] of Object.entries(context)) {
    if (!CORE_GLOBAL_KEYS.has(key)) globalThis[key] = value;
  }
  Object.assign(context, await loadAppModule());
  return context;
}

async function scriptContext(windowOverrides = {}, contextOverrides = {}) {
  const context = {
    HTMLElement: class {},
    customElements: { define() {} },
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
    },
    history: { pushState() {} },
    window: {
      location: { pathname: "/ui/" },
      addEventListener() {},
      setTimeout() {
        throw new Error("setTimeout should not be used");
      },
      clearTimeout() {},
      open() {
        throw new Error("window.open should not be used");
      },
      ...windowOverrides,
    },
    fetch() {
      throw new Error("fetch should not be used");
    },
    console,
    ...contextOverrides,
  };
  return applyContext(context);
}

test("human attention panel hides the reply form once the agent resumes", async () => {
  const context = await scriptContext();
  const issue = { id: "i-0001", title: "Working" };
  const statusLog = [{ id: 7, kind: "question", message: "which db?", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(issue, statusLog, "p-alpha", { id: "s-0001", state: "working" });
  assert.doesNotMatch(html, /data-attention-reply-form/);
  assert.doesNotMatch(html, /Needs Human Response/);
});

test("human attention panel renders the reply form while the session waits", async () => {
  const context = await scriptContext();
  const issue = { id: "i-0001", title: "Waiting" };
  const statusLog = [{ id: 7, kind: "question", message: "which db?", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(issue, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  assert.match(html, /Needs Human Response/);
  assert.match(html, /which db\?/);
  assert.match(html, /data-attention-reply-form="i-0001"/);
  assert.match(html, /data-status-log-id="7"/);
});

test("human attention panel renders both plan review and a waiting question", async () => {
  const context = await scriptContext();
  const issue = { id: "i-0001", title: "Plan plus question", plan_body: "Step 1\nStep 2", plan_submitted_at: "2026-06-07T11:00:00Z" };
  const statusLog = [{ id: 9, kind: "question", message: "which db?", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(issue, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  assert.match(html, /Plan Review/);
  assert.match(html, /data-plan-approve="i-0001"/);
  assert.match(html, /Step 1/);
  assert.match(html, /Needs Human Response/);
  assert.match(html, /data-attention-reply-form="i-0001"/);
  assert.match(html, /data-status-log-id="9"/);
});

test("phaseKey does not map crash_loop", async () => {
  const context = await scriptContext();
  assert.equal(context.phaseKey("crash_loop"), "");
});

function normalize(value) {
  return JSON.parse(JSON.stringify(value));
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

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

test("human attention panel renders the plan body as markdown", async () => {
  const context = await scriptContext();
  const issue = { id: "i-0001", title: "Plan", plan_body: "## Plan\n- step one" };
  const html = context.renderHumanAttentionPanel(issue, [], "p-alpha", null);
  assert.match(html, /class="human-attention-body md"/);
  assert.match(html, /<h2>Plan<\/h2>/);
  assert.match(html, /<li>step one<\/li>/);
  assert.doesNotMatch(html, /<pre class="human-attention-body"/);
});

test("human attention panel renders the question message as markdown", async () => {
  const context = await scriptContext();
  const issue = { id: "i-0001", title: "Q" };
  const statusLog = [{ id: 7, kind: "question", message: "Pick **one**:\n- a\n- b", created_at: "2026-06-07T12:00:00Z" }];
  const html = context.renderHumanAttentionPanel(issue, statusLog, "p-alpha", { id: "s-0001", state: "waiting" });
  assert.match(html, /<strong>one<\/strong>/);
  assert.match(html, /<li>a<\/li>/);
});

test("issue read-only detail renders the body as markdown but keeps the edit textarea raw", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueReadOnlyDetail(
    { id: "i-0001", title: "T", body: "## Body\n- item", acceptance_criteria: "- done" },
    { issueID: "i-0001" },
  );
  assert.match(html, /<h2>Body<\/h2>/);
  assert.match(html, /<li>item<\/li>/);
  assert.match(html, /<li>done<\/li>/);
  assert.match(html, /<textarea[^>]*name="body"[^>]*>## Body/);
});

test("issue read-only detail shows a dash placeholder for an empty body", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueReadOnlyDetail({ id: "i-0001", title: "T" }, { issueID: "i-0001" });
  assert.match(html, /Body<\/span>[\s\S]*?—/);
});

test("issue card renders the latest status message as inline markdown", async () => {
  const context = await scriptContext();
  const app = new context.FlowApp();
  const html = app.renderIssueCard(
    { id: "i-0001", title: "T" },
    { latest_status: { message: "**done** `sha`", kind: "progress" } },
    "working",
    false,
  );
  assert.match(html, /<strong>done<\/strong>/);
  assert.match(html, /<code>sha<\/code>/);
  assert.doesNotMatch(html, /<ul>|<h1>|<pre>/);
});

test("timeline status row renders the message as inline markdown", async () => {
  const context = await scriptContext();
  const html = context.renderTimelineStatusRow({ message: "see `x` now", kind: "note", actor: "agent", created_at: "2026-06-07T12:00:00Z" });
  assert.match(html, /<code>x<\/code>/);
  assert.doesNotMatch(html, /<ul>|<h1>/);
});

test("thread comment renders the body as markdown", async () => {
  const context = await scriptContext();
  const html = context.renderThreadComment({ actor: "rev", body: "- a\n- b", created_at: "2026-06-07T12:00:00Z" });
  assert.match(html, /class="md"/);
  assert.match(html, /<li>a<\/li>/);
  assert.doesNotMatch(html, /<p>- a/);
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

test("block markdown surfaces do not double-wrap the .md container", async () => {
  const context = await scriptContext();
  const comment = context.renderThreadComment({ actor: "r", body: "## H", created_at: "2026-06-07T12:00:00Z" });
  assert.doesNotMatch(comment, /class="md">\s*<div class="md"/);
  const app = new context.FlowApp();
  const detail = app.renderIssueReadOnlyDetail({ id: "i-1", title: "T", body: "## H" }, { issueID: "i-1" });
  assert.doesNotMatch(detail, /class="md">\s*<div class="md"/);
  const panel = context.renderHumanAttentionPanel({ id: "i-1", title: "T", plan_body: "## H" }, [], "p", null);
  assert.match(panel, /class="human-attention-body md"/);
  assert.doesNotMatch(panel, /class="md">\s*<div class="md"/);
});
