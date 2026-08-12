// ProjectCaches is the app's per-project keyed cache: one generic store per
// kind — flows, features, work items, tasks — with the fetcher and the
// empty-result fallback registered alongside. It replaces the copy-pasted
// per-kind ensure methods and the ad-hoc *ByProject Maps that views and
// action handlers used to write through directly.
//
// Cache reads are synchronous (cached/store) so views can render from a warm
// cache without an await; ensure() loads (or reloads, with {refresh: true})
// one project's entry; mutations invalidate the affected project and views
// that already hold the payload seed() it straight in.
export class ProjectCaches {
  constructor() {
    this.kinds = new Map();
  }

  register(kind, { fetch, fallback }) {
    this.kinds.set(kind, { fetch, fallback, store: new Map() });
  }

  entry(kind) {
    const entry = this.kinds.get(kind);
    if (!entry) throw new Error(`unknown project cache kind: ${kind}`);
    return entry;
  }

  // store exposes the live Map for a kind so legacy readers
  // (app.flowsByProject.get(...)) keep working while they migrate.
  store(kind) {
    return this.entry(kind).store;
  }

  // setStore re-seats a kind's Map wholesale — the test suites assign fresh
  // Maps (app.flowsByProject = new Map(...)) and the cache must follow.
  setStore(kind, map) {
    this.entry(kind).store = map || new Map();
  }

  cached(kind, projectID) {
    return this.store(kind).get(String(projectID || "").trim());
  }

  seed(kind, projectID, value) {
    this.store(kind).set(String(projectID || "").trim(), value);
  }

  invalidate(kind, projectID) {
    this.store(kind).delete(String(projectID || "").trim());
  }

  async ensure(kind, projectID, options = {}) {
    const id = String(projectID || "").trim();
    const entry = this.entry(kind);
    if (!id) return entry.fallback();
    if (entry.store.has(id) && !options.refresh) return entry.store.get(id);
    let result;
    try {
      result = await entry.fetch(id);
    } catch {
      // A failed or empty fetch caches the fallback so pickers fall back to
      // manual entry instead of re-fetching on every render.
      result = entry.fallback();
    }
    entry.store.set(id, result);
    return result;
  }
}
