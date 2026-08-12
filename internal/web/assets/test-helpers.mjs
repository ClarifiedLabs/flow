// Shared harness for the app-level Node tests: a swap-in set of global stubs
// (the native-ESM replacement for the old vm sandbox), a cache-busting app.js
// importer, and the minimal inline DOM the terminal/shell tests drive.
//
// The filename deliberately does not match *.test.mjs so `node --test` never
// collects it as a test file — same reason it must never live in a directory
// named `test` (node's default patterns match `**/test/**` wholesale).

export class SmokeElement {
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

export class SmokeNav extends SmokeElement {
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

export class SmokeLink extends SmokeElement {
  constructor(href) {
    super();
    this.href = href;
  }

  getAttribute(name) {
    return name === "href" ? this.href : "";
  }
}

export class SmokeDetails extends SmokeElement {
  constructor() {
    super();
    this.open = false;
  }
}

export function inlineDocument() {
  return {
    cookie: "flow_ui_csrf=csrf-token",
    addEventListener() {},
    createElement(tagName) {
      return new InlineDOMElement(tagName);
    },
  };
}

export class InlineDOMElement extends SmokeElement {
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

export class RepaintingInlineDOMElement extends InlineDOMElement {
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

export function findInlineTerminal(element) {
  if (element.dataset?.inlineTerminal === "true") return element;
  for (const child of element.children || []) {
    const match = findInlineTerminal(child);
    if (match) return match;
  }
  return null;
}

// A bare action control: dataset plus the disabled/attributes/classList
// surface the busy registry marks and restores.
export class ActionButton {
  constructor(dataset = {}) {
    this.dataset = dataset;
    this.disabled = false;
    this.attributes = new Map();
    this.classes = new Set();
    this.classList = {
      add: (name) => this.classes.add(name),
      remove: (name) => this.classes.delete(name),
      contains: (name) => this.classes.has(name),
    };
  }
  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }
  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }
  removeAttribute(name) {
    this.attributes.delete(name);
  }
}

// Console-route harness pieces: a mount-compatible content stub and a
// document.createElement factory that builds a real FlowConsole (never
// connected by the stub, so only its data-driven poll gating runs).
// consoleImports dynamically imports the route/element modules: their import
// chain defines custom-element classes, which needs a global HTMLElement —
// only present after a scriptContext call has installed one.
export async function consoleImports() {
  const { renderConsoleRoute } = await import("./console-route.js");
  const { FlowConsole } = await import("./elements/console.js");
  return { renderConsoleRoute, FlowConsole };
}

export function mountableContent() {
  return {
    innerHTML: "",
    dataset: {},
    firstElementChild: null,
    appendChild(child) {
      this.firstElementChild = child;
    },
  };
}

export function consoleDocument(FlowConsole) {
  return {
    cookie: "flow_ui_csrf=csrf-token",
    addEventListener() {},
    createElement(tag) {
      const element = new FlowConsole();
      element.tagName = String(tag).toUpperCase();
      element.closest = () => null;
      return element;
    },
  };
}

let appLoadCount = 0;
export function loadAppModule() {
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

export async function applyContext(context) {
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
  // The static surface import is cached across tests (same semantics as the
  // submodules the old barrel re-exported); only the entry-module import is
  // query-busted per test, so FlowApp rebinds to this test's HTMLElement while
  // everything else stays shared.
  Object.assign(context, await import("./test-surface.mjs"), await loadAppModule());
  return context;
}

export async function scriptContext(windowOverrides = {}, contextOverrides = {}) {
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

export function normalize(value) {
  return JSON.parse(JSON.stringify(value));
}

export function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

// flushAsync drains the microtask queue: setImmediate is a macrotask, so every
// promise continuation queued so far runs before it fires.
export function flushAsync() {
  return new Promise((resolve) => setImmediate(resolve));
}

// statusApp is a bare app stand-in with a recording status line.
export function statusApp() {
  const statuses = [];
  return {
    statuses,
    setStatus(message) {
      statuses.push(message);
    },
    refresh() {},
  };
}

// browserSmokeHarness drives a real FlowApp through its routes with smoke DOM
// stubs and per-path canned responses; route elements mount into a
// mount-compatible content stub and paint (isConnected) so
// content.firstElementChild holds the page markup after a load.
export async function browserSmokeHarness(path, responses) {
  const [pathname, search = ""] = String(path).split("?", 2);
  const title = new SmokeElement();
  const status = new SmokeElement();
  const content = new SmokeElement();
  // Mount-compatible: routes mount their route element into content now, and
  // the mounted element paints (isConnected) so content.firstElementChild
  // holds the page markup after a load.
  content.firstElementChild = null;
  content.appendChild = (child) => {
    content.firstElementChild = child;
  };
  const { FlowFlows } = await import("./elements/flows.js");
  const createElement = (tag) => {
    if (String(tag).toLowerCase() === "flow-flows") {
      const element = new FlowFlows();
      element.tagName = String(tag).toUpperCase();
      element.isConnected = true;
      element.closest = () => null;
      return element;
    }
    const element = new SmokeElement();
    element.tagName = String(tag).toUpperCase();
    return element;
  };
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
    location: { pathname, search: search ? `?${search}` : "" },
    setTimeout() {
      return 1;
    },
    clearTimeout() {},
  }, {
    document: {
      cookie: "flow_ui_csrf=csrf-token",
      addEventListener() {},
      createElement,
    },
    HTMLElement: SmokeHTMLElement,
    fetch(requestPath) {
      fetchCalls.push(requestPath);
      if (requestPath === "/ui/api/v2/projects" && !(requestPath in responses)) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ projects: [] }),
        });
      }
      if (requestPath === "/ui/api/v2/harnesses" && !(requestPath in responses)) {
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

