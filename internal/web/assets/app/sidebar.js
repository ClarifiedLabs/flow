// SidebarStatus owns the /v2/sidebar poll loop: the fetch, the generation
// guard that keeps a slow response from overwriting a newer render, the
// failure backoff, and the Poller the loop re-arms on. Rendering stays with
// the app through the render port.

import { MAX_POLL_BACKOFF_MS, SIDEBAR_STATUS_POLL_MS } from "../config.js";
import { Poller, pollDelay } from "../poller.js";

export class SidebarStatus {
  // ports plugs the loop into the app:
  //   fetchSidebar()  — GET the sidebar payload (already project-scoped)
  //   render(data)    — paint the nav and the dropdown trigger from it
  //   hasChrome()     — the shell's nav is mounted
  constructor(ports) {
    this.ports = ports;
    this.poll = new Poller();
    this.generation = 0;
    this.failures = 0;
    this.pollingActive = true;
  }

  async refresh() {
    if (this.pollingActive === false) return false;
    if (!this.ports.hasChrome()) return false;
    this.clear();
    const context = {
      generation: this.generation + 1,
    };
    this.generation = context.generation;

    try {
      const data = await this.ports.fetchSidebar();
      if (!this.isActive(context)) return false;
      this.ports.render(data);
      this.failures = 0;
      this.schedule();
      return true;
    } catch (error) {
      if (!this.isActive(context)) return false;
      this.failures += 1;
      this.schedule();
      return false;
    }
  }

  isActive(context) {
    return this.pollingActive !== false
      && context
      && context.generation === this.generation;
  }

  clear() {
    this.poll.clear();
  }

  schedule() {
    if (this.pollingActive === false) return;
    if (!this.ports.hasChrome()) return;
    const delay = pollDelay(SIDEBAR_STATUS_POLL_MS, this.failures, MAX_POLL_BACKOFF_MS);
    this.poll.arm(delay, () => this.refresh());
  }

  // start re-arms the loop on (re)connect: polling resumes with a clean
  // backoff while the generation carries over, so responses in flight from
  // before a disconnect stay superseded.
  start() {
    this.pollingActive = true;
    this.failures = 0;
  }

  stop() {
    this.pollingActive = false;
    this.generation += 1;
    this.clear();
  }
}
