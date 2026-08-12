// SettleBurst owns the settle-burst state machine: a short, bounded series of
// follow-up reloads of the current route after a successful action-triggered
// refresh (see SETTLE_BURST_DELAYS_MS). A mutation lands synchronously inside
// its request, but its visible follow-on effects (the agent session starting,
// the next gate opening, checks beginning) only settle asynchronously over
// the next few seconds. Failed actions never reach their reload at all —
// their handlers unwind first — and a cancelled confirm returns before it,
// so only a completed mutation arms the burst.
//
// Only the dispatcher can arm one: the action, form, and review dispatchers
// run their handlers against an action-scoped app whose refresh() and load()
// carry the ACTION_SETTLE provenance token (see actions/dispatch.js). A
// token-carrying reload therefore proves it belongs to one specific
// successful action run — an unrelated reload that merely *overlaps* an
// in-flight action (the board's Done filter firing while a slow POST is
// pending) carries no token and arms nothing, whether or not that action
// goes on to fail.
//
// The class owns the burst timer (its own Poller) and the burst identity; the
// app provides ports into its load machinery, so the burst reuses the load's
// own guards rather than adding parallel state.
//
// - Each schedule takes a fresh burst identity (id). A newer burst clears the
//   older burst's pending timer and owns the Poller from then on; a tick of a
//   superseded burst — one still pending, or one that was already awaiting
//   its reload when the newer burst was scheduled — recognizes the newer
//   owner and neither reloads nor re-arms, so a concurrent action can never
//   leave a timeout outside the active burst's ownership.
// - Each tick captures the load generation and path as it is armed — the
//   first from origin itself — and re-checks them through isActiveLoad when
//   it fires, so navigating to another route, or any newer load starting
//   (a poll, a manual refresh, another action), supersedes the pending
//   tick and ends the burst.
// - Every load that is not a burst's own reload also cancels the pending
//   timer outright and retires the identity (see cancelUnless), so navigation
//   and disconnects never leave a live timeout that is merely guarded into a
//   no-op.
// - A tick that finds a load still in flight skips its own reload, so the
//   burst never overlaps load() calls; the remaining ticks still fire.
// - A tick that does reload hands its own load context to the next tick:
//   the next guard is the reload's generation and path, and the tick only
//   re-arms when that reload is still the newest load on the route. A
//   reload that was superseded while awaiting (navigation, a newer load, a
//   disconnect) ends the burst instead of re-arming against the new state.
//
// Regular poll scheduling is untouched: each burst load re-arms the route's
// usual poll through finishLoad like any other load.

import { SETTLE_BURST_DELAYS_MS } from "../config.js";
import { Poller } from "../poller.js";
import { ACTION_SETTLE } from "../actions/dispatch.js";

export class SettleBurst {
  // ports plugs the burst into the app's load machinery:
  //   isActiveLoad(context)  — the load-activity guard (generation + path)
  //   reload(options)        — app.load
  //   getGeneration()        — the current load generation
  //   getPath()              — the current location path
  //   getLoadsInFlight()     — how many load() calls are running
  //   isPollingActive()      — false once the app disconnects
  constructor(ports) {
    this.ports = ports;
    this.poll = new Poller();
    // Identity of the active settle burst; each schedule (and each supersede)
    // takes a fresh id so superseded bursts can never re-arm timers or claim
    // ownership of the newer burst's timer (see schedule).
    this.id = 0;
  }

  // cancelUnless retires the active burst when any load that is not the
  // burst's own reload starts: the pending timeout is cancelled now — not
  // left live until it fires — and the identity is retired, so a tick still
  // awaiting its reload can neither reload nor re-arm. The burst's own
  // reloads carry their burst identity and are exempt.
  cancelUnless(burst) {
    if (burst === this.id) return;
    this.id += 1;
    this.poll.clear();
  }

  // supersede retires any burst still in flight because the app went away:
  // its ticks cannot re-arm after the disconnect.
  supersede() {
    this.id += 1;
    this.poll.clear();
  }

  // maybeArm decides whether a just-completed load arms the settle burst: it
  // does when — and only when — the call carried the dispatcher's
  // ACTION_SETTLE provenance token and the load is still the active one.
  // Arming only off the refresh's (or handler-owned load's) own context keeps
  // the burst off a route the action never saw: navigating away, or any newer
  // load, while the immediate load runs supersedes it, and a superseded or
  // failed load hands back no context at all.
  maybeArm(options, context) {
    if (options?.settle !== ACTION_SETTLE) return;
    if (!this.ports.isActiveLoad(context)) return;
    this.schedule(context);
  }

  // schedule arms the burst. origin is the load context that the refresh
  // produced: the burst belongs to that load's route and generation, not to
  // wherever the app happens to be when the burst is armed.
  schedule(origin) {
    const { isActiveLoad, reload, getGeneration, getPath, getLoadsInFlight, isPollingActive } = this.ports;
    if (isPollingActive() === false) return;
    this.poll.clear();
    // A new burst identity: every schedule supersedes the previous burst, so
    // an older tick that is still awaiting its reload (or one that fires
    // late) recognizes that it no longer owns the Poller.
    const burst = this.id + 1;
    this.id = burst;
    const path = origin.path;
    const delays = SETTLE_BURST_DELAYS_MS;
    const armTick = (index, guard = { generation: getGeneration(), path }) => {
      if (index >= delays.length) return;
      if (burst !== this.id) return;
      // Delays are absolute offsets from the action's refresh; the one-shot
      // Poller re-arms per tick, so each arm waits out only the delta.
      const offset = index > 0 ? delays[index] - delays[index - 1] : delays[index];
      this.poll.arm(offset, async () => {
        if (burst !== this.id) return;
        if (!isActiveLoad(guard)) return;
        let reloaded;
        let calledLoad = false;
        try {
          if (!getLoadsInFlight()) {
            calledLoad = true;
            reloaded = await reload({ fromPoll: true, burst });
          }
        } catch {
          // load() reports its own failures on the status line; a rejection
          // escaping it must not strand the remaining burst ticks.
        }
        // A superseded burst must not re-arm: if a newer action scheduled its
        // own burst while this tick was awaiting, the Poller now belongs to
        // that burst, and re-arming here would orphan the newer burst's timer.
        if (burst !== this.id) return;
        if (calledLoad) {
          // The tick's own load must still be the newest on the route: any
          // newer load (navigation, a poll, a manual refresh, another action)
          // or a disconnect while it was awaiting supersedes the burst, which
          // ends here instead of re-arming against the new state. A tick that
          // skipped its reload arms against the current state as before.
          if (getGeneration() !== guard.generation + 1) return;
          if (getPath() !== guard.path) return;
        }
        armTick(
          index + 1,
          reloaded ? { generation: reloaded.generation, path: reloaded.path } : undefined,
        );
      });
    };
    armTick(0, { generation: origin.generation, path: origin.path });
  }
}
