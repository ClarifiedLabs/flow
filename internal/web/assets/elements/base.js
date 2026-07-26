// The custom-element foundation.
//
// Two rules make the whole tree work:
//
//   1. An element paints itself from its own `data` property. Setting `data`
//      schedules a repaint on a microtask, so a parent can set several
//      properties in a row and pay for one paint.
//   2. An element listens on itself, once, in connectedCallback. Delegation on
//      the element survives that element replacing its own innerHTML, which is
//      what lets state (an open transcript, the active tab, a draft comment)
//      outlive a poll.
//
// Light DOM only. Design tokens live on :root and the build step scopes
// selectors by tag name, so a shadow root would sever both and force every
// component to carry its own stylesheet.
//
// Render logic lives in exported pure functions; the element class is a thin
// shell over one. That keeps the markup testable as a string in Node without a
// DOM, which is the only way to test it without pulling in a third-party
// browser environment.

export class FlowElement extends HTMLElement {
  #data = null;
  #painted = null;
  #pending = false;
  #bound = false;

  set data(next) {
    this.#data = next;
    this.requestPaint();
  }

  get data() {
    return this.#data;
  }

  connectedCallback() {
    if (!this.#bound) {
      this.#bound = true;
      this.addEventListener("click", (event) => this.handleClick(event));
      this.bind();
    }
    this.paint();
  }

  // requestPaint coalesces a burst of property writes into one repaint.
  requestPaint() {
    if (this.#pending) return;
    this.#pending = true;
    queueMicrotask(() => {
      this.#pending = false;
      if (this.isConnected) this.paint();
    });
  }

  paint() {
    const html = this.render(this.#data);
    // Polling re-renders the same markup most of the time. Skipping the write
    // keeps focus, selection and scroll position intact for free.
    if (html === this.#painted) return;
    this.#painted = html;
    this.innerHTML = html;
    this.afterPaint();
  }

  // invalidate forces the next paint to write, for when render() depends on
  // instance state rather than on `data`.
  invalidate() {
    this.#painted = null;
    this.requestPaint();
  }

  render() {
    return "";
  }

  // bind installs listeners other than click. Called once.
  bind() {}

  // afterPaint runs after every write to innerHTML — the place to mount a
  // child element or restore scroll.
  afterPaint() {}

  // handleClick receives every click inside the element. Subclasses read
  // event.target.closest("[data-…]") to decide what was pressed.
  handleClick() {}

  // app walks up to the owning <flow-app> for shared services: routing, the
  // statusbar, the API project scope.
  get app() {
    return this.closest("flow-app");
  }
}

// define registers an element once. Modules are imported from several entry
// points, and customElements.define throws on a repeat.
export function define(name, constructor) {
  const registry = globalThis.customElements;
  if (!registry || typeof registry.define !== "function") return constructor;
  if (typeof registry.get === "function" && registry.get(name)) return constructor;
  try {
    registry.define(name, constructor);
  } catch {
    // Already defined by another import of this module.
  }
  return constructor;
}

// reconcile matches existing children to items by key, so an element that is
// still on screen keeps its instance — and therefore its state — across a
// poll. Rebuilding the list wholesale would throw away exactly the state the
// custom-element conversion exists to preserve.
export function reconcile(container, items, { tag, key, apply }) {
  if (!container) return [];
  const existing = new Map();
  for (const child of Array.from(container.children)) {
    const childKey = child.dataset?.key;
    if (childKey !== undefined) existing.set(childKey, child);
  }

  const ordered = [];
  for (const item of items) {
    const itemKey = String(key(item));
    let element = existing.get(itemKey);
    if (element) {
      existing.delete(itemKey);
    } else {
      element = document.createElement(tag);
      element.dataset.key = itemKey;
    }
    if (apply) apply(element, item);
    else element.data = item;
    ordered.push(element);
  }

  // Re-append in order. appendChild moves an existing child rather than
  // copying it, so survivors keep their identity.
  for (const element of ordered) container.appendChild(element);
  for (const stale of existing.values()) stale.remove();
  return ordered;
}

// mount replaces a container's children with one element of the given tag,
// reusing the existing instance when the tag already matches.
export function mount(container, tag, data) {
  if (!container) return null;
  let element = container.firstElementChild;
  if (!element || element.tagName?.toLowerCase() !== tag) {
    container.innerHTML = "";
    element = document.createElement(tag);
    container.appendChild(element);
  }
  element.data = data;
  return element;
}
