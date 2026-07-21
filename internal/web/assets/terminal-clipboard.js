// Bridges tmux's OSC 52 clipboard sequences to the browser clipboard.
//
// tmux is configured with `set-clipboard on` + `terminal-features *:clipboard`
// (see configureTmuxForJob in internal/worker/execution/execution.go), so it
// emits an OSC 52 sequence (ESC ] 52 ; c ; <base64> ST) every time a selection
// completes — both a plain mouse drag (tmux owns the mouse because `mouse on`
// keeps wheel scrollback working) and a copy-mode keyboard copy. ttyd embeds
// xterm.js but ships no OSC 52 handler, so those sequences would otherwise be
// dropped and a plain drag would not copy. This script registers an OSC 52
// parser on the xterm.js instance ttyd exposes as window.term and forwards the
// payload to navigator.clipboard.writeText, so a plain drag-select copies to
// the local clipboard while tmux keeps owning the mouse.
//
// The coordinator terminal proxy injects this as a classic <script src> into
// ttyd's terminal page and serves it from the terminal path; it must stay a
// plain script with no ES module syntax.
(function () {
  "use strict";

  var POLL_INTERVAL_MS = 50;
  var POLL_TIMEOUT_MS = 15000;

  function decodeBase64UTF8(payload) {
    var binary = atob(payload);
    var bytes = new Uint8Array(binary.length);
    for (var i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    return new TextDecoder("utf-8").decode(bytes);
  }

  function writeClipboard(text) {
    // No execCommand fallback: it needs user activation, which an async
    // websocket message does not carry.
    if (!navigator.clipboard || typeof navigator.clipboard.writeText !== "function") {
      return;
    }
    try {
      navigator.clipboard.writeText(text).catch(function (err) {
        console.debug("terminal-clipboard: writeText failed", err);
      });
    } catch (err) {
      console.debug("terminal-clipboard: writeText threw", err);
    }
  }

  // xterm.js passes the OSC body after the command id, i.e. "Pc;Pd" where Pc is
  // the clipboard selection (usually "c") and Pd is the base64 payload, or "?"
  // for a clipboard query.
  function handleOsc52(data) {
    var separator = data.indexOf(";");
    if (separator < 0) {
      return false;
    }
    var payload = data.slice(separator + 1);
    if (payload === "?") {
      // Clipboard query: acknowledge as handled but never answer, so the local
      // clipboard is not exfiltrated into the pty.
      return true;
    }
    try {
      writeClipboard(decodeBase64UTF8(payload));
    } catch (err) {
      console.debug("terminal-clipboard: decode failed", err);
    }
    return true;
  }

  function register(term) {
    try {
      term.parser.registerOscHandler(52, handleOsc52);
    } catch (err) {
      console.debug("terminal-clipboard: registerOscHandler failed", err);
    }
  }

  // ttyd assigns the xterm.js instance to window.term asynchronously after the
  // page loads, so poll for it and register once it exists.
  function waitForTerm() {
    var waited = 0;
    var timer = setInterval(function () {
      var term = window.term;
      if (term && term.parser && typeof term.parser.registerOscHandler === "function") {
        clearInterval(timer);
        register(term);
        return;
      }
      waited += POLL_INTERVAL_MS;
      if (waited >= POLL_TIMEOUT_MS) {
        clearInterval(timer);
      }
    }, POLL_INTERVAL_MS);
  }

  waitForTerm();
})();
