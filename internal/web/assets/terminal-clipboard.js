// Bridges OSC 52 clipboard sequences from xterm.js to the browser clipboard.
// This must remain a classic script because terminal.html loads it directly.
(function () {
  "use strict";

  function decodeBase64UTF8(payload) {
    var binary = atob(payload);
    var bytes = new Uint8Array(binary.length);
    for (var i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    return new TextDecoder("utf-8").decode(bytes);
  }

  function writeClipboard(text) {
    // Clipboard access requires a secure context and may be denied by browser
    // permissions. There is no useful execCommand fallback for async PTY data.
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

  // xterm.js passes the OSC body after command 52: "Pc;Pd", where Pc is the
  // clipboard selection and Pd is the base64 payload (or "?" for a query).
  function handleOsc52(data) {
    var separator = data.indexOf(";");
    if (separator < 0) {
      return false;
    }

    var payload = data.slice(separator + 1);
    if (payload === "?") {
      // Do not answer clipboard queries; that would expose local clipboard data
      // to the remote process.
      return true;
    }

    try {
      window.flowTerminalOSC52(decodeBase64UTF8(payload));
    } catch (err) {
      console.debug("terminal-clipboard: decode failed", err);
    }
    return true;
  }

  function register(term) {
    try {
      return term.parser.registerOscHandler(52, handleOsc52);
    } catch (err) {
      console.debug("terminal-clipboard: registerOscHandler failed", err);
    }
  }

  // Control messages carrying an already-decoded OSC 52 payload use this
  // callback directly. PTY output reaches it through the parser handler above.
  window.flowTerminalOSC52 = writeClipboard;
  window.flowTerminalClipboard = { register: register };
})();
