// A DOM small enough to hand-write and real enough to test custom elements
// against: element construction, connection callbacks, property setters,
// attributes, event dispatch with bubbling, and the subset of innerHTML
// parsing the app's own markup uses.
//
// Hand-written because the alternative is a third-party browser environment,
// and the whole point of this UI is that it ships with no dependencies.

const VOID_TAGS = new Set(["br", "hr", "img", "input", "meta", "link"]);

class ClassList {
  constructor(element) {
    this.element = element;
  }
  get #tokens() {
    return String(this.element.getAttribute("class") || "").split(/\s+/).filter(Boolean);
  }
  add(...names) {
    const tokens = new Set(this.#tokens);
    for (const name of names) tokens.add(name);
    this.element.setAttribute("class", [...tokens].join(" "));
  }
  remove(...names) {
    const tokens = new Set(this.#tokens);
    for (const name of names) tokens.delete(name);
    this.element.setAttribute("class", [...tokens].join(" "));
  }
  contains(name) {
    return this.#tokens.includes(name);
  }
}

export class TestEvent {
  constructor(type, options = {}) {
    this.type = type;
    this.bubbles = Boolean(options.bubbles);
    this.detail = options.detail;
    this.defaultPrevented = false;
    this.target = null;
    this.currentTarget = null;
  }
  preventDefault() {
    this.defaultPrevented = true;
  }
  stopPropagation() {
    this.propagationStopped = true;
  }
}

export class TestNode {
  constructor(tagName = "div") {
    this.tagName = String(tagName).toUpperCase();
    this.children = [];
    this.parentElement = null;
    this.attributes = new Map();
    this.dataset = {};
    this.listeners = new Map();
    this.classList = new ClassList(this);
    this.textContent = "";
    this.value = "";
    this.isConnected = false;
  }

  get localName() {
    return this.tagName.toLowerCase();
  }

  // parentNode is parentElement for every node the app builds.
  get parentNode() {
    return this.parentElement;
  }

  get className() {
    return this.getAttribute("class") || "";
  }

  set className(next) {
    this.setAttribute("class", next);
  }

  get firstElementChild() {
    return this.children[0] || null;
  }

  // The one piece of HTMLFormElement the app's delegated form handlers use:
  // named access to the form's controls (form.elements.body, etc.).
  get elements() {
    if (this.tagName !== "FORM") return undefined;
    const named = {};
    const walk = (node) => {
      for (const child of node.children) {
        const name = child.getAttribute?.("name");
        if (name) named[name] = child;
        walk(child);
      }
    };
    walk(this);
    return named;
  }

  get nextElementSibling() {
    const siblings = this.parentElement?.children || [];
    return siblings[siblings.indexOf(this) + 1] || null;
  }

  get previousElementSibling() {
    const siblings = this.parentElement?.children || [];
    return siblings[siblings.indexOf(this) - 1] || null;
  }

  setAttribute(name, next) {
    this.attributes.set(name, String(next));
    if (name.startsWith("data-")) this.dataset[dataKey(name)] = String(next);
  }

  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }

  hasAttribute(name) {
    return this.attributes.has(name);
  }

  removeAttribute(name) {
    this.attributes.delete(name);
    if (name.startsWith("data-")) delete this.dataset[dataKey(name)];
  }

  toggleAttribute(name, force) {
    const on = force === undefined ? !this.hasAttribute(name) : Boolean(force);
    if (on) this.setAttribute(name, "");
    else this.removeAttribute(name);
    return on;
  }

  appendChild(child) {
    child.remove();
    child.parentElement = this;
    this.children.push(child);
    setConnected(child, this.isConnected);
    return child;
  }

  insertBefore(child, reference) {
    child.remove();
    child.parentElement = this;
    const index = reference ? this.children.indexOf(reference) : -1;
    if (index >= 0) this.children.splice(index, 0, child);
    else this.children.push(child);
    setConnected(child, this.isConnected);
    return child;
  }

  after(node) {
    if (!this.parentElement) return;
    node.remove();
    node.parentElement = this.parentElement;
    this.parentElement.children.splice(this.parentElement.children.indexOf(this) + 1, 0, node);
    setConnected(node, this.parentElement.isConnected);
  }

  remove() {
    if (!this.parentElement) return;
    const index = this.parentElement.children.indexOf(this);
    if (index >= 0) this.parentElement.children.splice(index, 1);
    this.parentElement = null;
    setConnected(this, false);
  }

  set innerHTML(html) {
    for (const child of this.children.slice()) {
      child.parentElement = null;
      setConnected(child, false);
    }
    this.children = [];
    this._innerHTML = String(html);
    for (const child of parseHTML(String(html))) this.appendChild(child);
  }

  // insertAdjacentHTML supports the two positions the flow editor's add-row
  // controls use; the markup is parsed with the same subset parser as
  // innerHTML.
  insertAdjacentHTML(position, html) {
    const nodes = parseHTML(String(html));
    if (position === "beforeend") {
      for (const node of nodes) this.appendChild(node);
      return;
    }
    if (position === "afterbegin") {
      for (const node of [...nodes].reverse()) this.insertBefore(node, this.firstElementChild);
      return;
    }
    throw new Error(`insertAdjacentHTML("${position}") is not implemented in test-dom`);
  }

  get innerHTML() {
    return this._innerHTML || "";
  }

  addEventListener(type, handler) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(handler);
  }

  removeEventListener(type, handler) {
    const handlers = this.listeners.get(type) || [];
    const index = handlers.indexOf(handler);
    if (index >= 0) handlers.splice(index, 1);
  }

  dispatchEvent(event) {
    event.target = event.target || this;
    let node = this;
    while (node) {
      event.currentTarget = node;
      for (const handler of (node.listeners.get(event.type) || []).slice()) handler.call(node, event);
      if (!event.bubbles || event.propagationStopped) break;
      node = node.parentElement;
    }
    return !event.defaultPrevented;
  }

  closest(selector) {
    let node = this;
    while (node) {
      if (matches(node, selector)) return node;
      node = node.parentElement;
    }
    return null;
  }

  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }

  querySelectorAll(selector) {
    const found = [];
    const walk = (node) => {
      for (const child of node.children) {
        if (matches(child, selector)) found.push(child);
        walk(child);
      }
    };
    walk(this);
    return found;
  }

  contains(node) {
    let current = node;
    while (current) {
      if (current === this) return true;
      current = current.parentElement;
    }
    return false;
  }

  focus() {
    if (globalThis.document) globalThis.document.activeElement = this;
  }

  blur() {
    if (globalThis.document?.activeElement === this) globalThis.document.activeElement = null;
  }

  // click() dispatches a bubbling click, which is how the delegated action
  // table is exercised.
  click() {
    return this.dispatchEvent(new TestEvent("click", { bubbles: true }));
  }
}

function setConnected(node, connected) {
  if (node.isConnected === connected) return;
  node.isConnected = connected;
  if (connected) {
    node.connectedCallback?.();
    if (node.localName === "iframe") navigateIframe(node);
  } else {
    node.disconnectedCallback?.();
  }
  for (const child of node.children) setConnected(child, connected);
}

// navigateIframe models a browser navigating an iframe's nested browsing
// context to its src. Connecting an iframe loads it — and so does reconnecting
// a node that was detached, which is how a browser reloads an iframe that was
// removed and re-appended. Tests assert terminal continuity by checking that a
// preserved iframe's loadCount does not climb across a repaint.
function navigateIframe(node) {
  const src = node.getAttribute("src");
  if (src === null) return;
  node.loadCount = (node.loadCount || 0) + 1;
  (node.loads ||= []).push(src);
}

function dataKey(name) {
  return name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());
}

// unescapeEntities decodes the character references escapeHTML emits plus the
// bare apos form, mirroring how a browser's HTML parser decodes attribute
// values. It only handles the app's own escapes, not arbitrary numeric
// references.
function unescapeEntities(value) {
  return String(value).replace(/&(amp|lt|gt|quot|#39|apos);/g, (match, name) => ({
    amp: "&",
    lt: "<",
    gt: ">",
    quot: '"',
    "#39": "'",
    apos: "'",
  })[name]);
}

// matches supports the selector shapes the app actually writes: tag, .class,
// #id, [attr], [attr="value"], and comma-separated lists of those. Descendant
// combinators are deliberately unsupported — nothing in the app needs one in a
// querySelector, and pretending otherwise would hide a real bug.
function matches(node, selector) {
  return String(selector)
    .split(",")
    .map((part) => part.trim())
    .filter(Boolean)
    .some((part) => matchesSimple(node, part));
}

function matchesSimple(node, selector) {
  const parts = selector.match(/^([a-zA-Z][\w-]*)?((?:[.#][\w-]+|\[[^\]]+\])*)$/);
  if (!parts) return false;
  const [, tag, rest = ""] = parts;
  if (tag && node.tagName !== tag.toUpperCase()) return false;
  for (const token of rest.match(/[.#][\w-]+|\[[^\]]+\]/g) || []) {
    if (token.startsWith(".")) {
      if (!node.classList.contains(token.slice(1))) return false;
    } else if (token.startsWith("#")) {
      if (node.getAttribute("id") !== token.slice(1)) return false;
    } else {
      const inner = token.slice(1, -1);
      const eq = inner.indexOf("=");
      if (eq === -1) {
        if (!node.hasAttribute(inner)) return false;
      } else {
        const name = inner.slice(0, eq);
        const wanted = inner.slice(eq + 1).replace(/^["']|["']$/g, "");
        if (node.getAttribute(name) !== wanted) return false;
      }
    }
  }
  return true;
}

// parseHTML understands the markup the app emits: nested tags, attributes,
// self-closing void elements, and text. It is not a general HTML parser.
function parseHTML(html) {
  const roots = [];
  const stack = [];
  const pattern = /<(\/)?([a-zA-Z][\w-]*)((?:\s+[^\s/>"']+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*(\/)?>/g;
  let cursor = 0;
  let match;
  const push = (node) => {
    if (stack.length) stack[stack.length - 1].appendChild(node);
    else roots.push(node);
  };
  const text = (raw) => {
    const trimmed = raw.replace(/\s+/g, " ");
    if (!trimmed.trim()) return;
    const parent = stack[stack.length - 1];
    if (parent) parent.textContent += trimmed;
  };

  while ((match = pattern.exec(html))) {
    text(html.slice(cursor, match.index));
    cursor = pattern.lastIndex;
    const [, closing, tag, attrs = "", selfClosing] = match;
    if (closing) {
      while (stack.length && stack[stack.length - 1].localName !== tag.toLowerCase()) stack.pop();
      stack.pop();
      continue;
    }
    const node = createElement(tag);
    for (const attr of attrs.match(/[^\s=]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?/g) || []) {
      const eq = attr.indexOf("=");
      if (eq === -1) node.setAttribute(attr.trim(), "");
      else {
        // A real HTML parser unescapes character references in attribute
        // values. The render path escapes attribute values (escapeAttr), so an
        // id containing a quote arrives as &quot;; without unescaping, dataset
        // would hold the escaped text instead of the raw value and never match
        // the in-flight registry key or another control's dataset.
        node.setAttribute(attr.slice(0, eq).trim(), unescapeEntities(attr.slice(eq + 1).trim().replace(/^["']|["']$/g, "")));
      }
    }
    push(node);
    if (!selfClosing && !VOID_TAGS.has(tag.toLowerCase())) stack.push(node);
  }
  text(html.slice(cursor));
  return roots;
}

const registry = new Map();

function createElement(tag) {
  const Constructor = registry.get(String(tag).toLowerCase());
  if (Constructor) {
    const node = new Constructor();
    node.tagName = String(tag).toUpperCase();
    return node;
  }
  return new TestNode(tag);
}

// install wires the fake DOM onto globalThis and returns a root element that
// behaves like a connected <flow-app>.
export function installTestDOM() {
  const documentNode = new TestNode("body");
  documentNode.isConnected = true;

  globalThis.HTMLElement = TestNode;
  globalThis.CustomEvent = TestEvent;
  globalThis.Event = TestEvent;
  globalThis.customElements = {
    define(name, Constructor) {
      if (registry.has(name)) throw new Error(`${name} already defined`);
      registry.set(name, Constructor);
    },
    get(name) {
      return registry.get(name);
    },
  };
  globalThis.document = {
    body: documentNode,
    documentElement: new TestNode("html"),
    activeElement: null,
    cookie: "flow_ui_csrf=csrf-token",
    createElement,
    // The document queries the whole tree, like a real Document. Document-wide
    // lookups (e.g. restoring every live same-thread claim control on settle)
    // rely on this delegating to the body root.
    querySelectorAll: (selector) => documentNode.querySelectorAll(selector),
    addEventListener() {},
    removeEventListener() {},
  };
  globalThis.window = globalThis.window || {};
  globalThis.window.localStorage = memoryStorage();
  globalThis.window.sessionStorage = memoryStorage();
  globalThis.window.confirm = () => true;
  globalThis.window.prompt = () => "";
  globalThis.window.location = { pathname: "/ui/board" };
  globalThis.history = { pushState() {} };

  return documentNode;
}

function memoryStorage() {
  const store = new Map();
  return {
    getItem: (key) => (store.has(key) ? store.get(key) : null),
    setItem: (key, value) => store.set(key, String(value)),
    removeItem: (key) => store.delete(key),
  };
}

// mountElement creates a registered element, connects it, and returns it.
export function mountElement(root, tag, data) {
  const element = createElement(tag);
  root.appendChild(element);
  if (data !== undefined) element.data = data;
  return element;
}

// flush lets queued microtask repaints run.
export function flush() {
  return new Promise((resolve) => queueMicrotask(resolve));
}
