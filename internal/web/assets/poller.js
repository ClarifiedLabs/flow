// Polling delay math: exponential backoff (doubling per consecutive failure)
// capped at a ceiling. Shared by the main and sidebar status schedulers so the
// backoff curve stays identical in one place.
export function pollDelay(base, failures, max) {
  const factor = failures ? 2 ** failures : 1;
  return Math.min(base * factor, max);
}

// Poller owns a single setTimeout handle. arm() schedules a one-shot callback
// (clearing the handle just before it fires, so the callback can safely re-arm),
// and clear() cancels a pending one. The main, sidebar and console loops each own
// a Poller, keeping the timer bookkeeping in one place; their stale-guard and
// reschedule policies stay with the caller.
export class Poller {
  constructor() {
    this.timer = 0;
  }

  arm(delay, callback) {
    this.timer = window.setTimeout(() => {
      this.timer = 0;
      callback();
    }, delay);
  }

  clear() {
    if (!this.timer) return;
    window.clearTimeout(this.timer);
    this.timer = 0;
  }
}
