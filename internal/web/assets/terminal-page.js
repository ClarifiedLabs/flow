(function () {
  "use strict";

  var terminalElement = document.getElementById("terminal");
  var dataElement = document.getElementById("terminal-page-data");
  var term = new Terminal({
    cursorBlink: true,
    fontSize: 14,
    theme: {
      background: "#0f111a",
      foreground: "#d8dee9",
      cursor: "#f8f8f2",
      selectionBackground: "#3b4252",
      black: "#2e3440",
      red: "#bf616a",
      green: "#a3be8c",
      yellow: "#ebcb8b",
      blue: "#81a1c1",
      magenta: "#b48ead",
      cyan: "#88c0d0",
      white: "#e5e9f0"
    }
  });
  var fitAddon = new FitAddon.FitAddon();

  term.loadAddon(fitAddon);
  term.loadAddon(new WebLinksAddon.WebLinksAddon());
  term.open(terminalElement);
  fitAddon.fit();
  term.focus();

  if (window.flowTerminalClipboard) {
    window.flowTerminalClipboard.register(term);
  }

  function writeStatus(message) {
    term.write("\r\n\x1b[90m[flow] " + message + "\x1b[0m\r\n");
  }

  function websocketURL(value) {
    var url = new URL(value, window.location.href);
    if (url.protocol === "http:") {
      url.protocol = "ws:";
    } else if (url.protocol === "https:") {
      url.protocol = "wss:";
    }
    return url.href;
  }

  if (!dataElement || !dataElement.dataset.wsUrl) {
    writeStatus("Terminal WebSocket URL is missing.");
    return;
  }

  var socket;
  try {
    socket = new WebSocket(websocketURL(dataElement.dataset.wsUrl));
  } catch (err) {
    writeStatus("Unable to open the terminal connection.");
    console.error("terminal-page: WebSocket setup failed", err);
    return;
  }
  socket.binaryType = "arraybuffer";

  function sendControl(message) {
    if (socket.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify(message));
    }
  }

  term.onData(function (data) {
    if (socket.readyState === WebSocket.OPEN) {
      socket.send(new TextEncoder().encode(data));
    }
  });

  term.onResize(function (size) {
    sendControl({ type: "resize", cols: size.cols, rows: size.rows });
  });

  window.addEventListener("resize", function () {
    fitAddon.fit();
  });

  socket.addEventListener("open", function () {
    sendControl({ type: "attach", cols: term.cols, rows: term.rows });
  });

  socket.addEventListener("message", function (event) {
    if (typeof event.data !== "string") {
      if (event.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(event.data));
      } else if (event.data instanceof Blob) {
        event.data.arrayBuffer().then(function (data) {
          term.write(new Uint8Array(data));
        });
      }
      return;
    }

    var message;
    try {
      message = JSON.parse(event.data);
    } catch (err) {
      console.debug("terminal-page: ignored invalid text message", err);
      return;
    }

    if (message.type === "exit") {
      writeStatus("Terminal process exited.");
    } else if (
      message.type === "osc52" &&
      typeof message.data === "string" &&
      typeof window.flowTerminalOSC52 === "function"
    ) {
      window.flowTerminalOSC52(message.data);
    }
  });

  socket.addEventListener("close", function () {
    writeStatus("Terminal connection closed.");
  });

  socket.addEventListener("error", function () {
    writeStatus("Terminal connection error.");
  });
})();
