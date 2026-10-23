/* The copy button on a console block copies the commands only: prompts and
   output are left out, and continuation lines and heredoc bodies are kept. */
(function (root) {
  var PROMPT = /^[$#] /;
  var HEREDOC = /<<(-?)\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)\2/;

  function commandsOf(text) {
    var out = [];
    var continued = false;
    var heredoc = null;
    text.replace(/\n+$/, "").split("\n").forEach(function (line) {
      if (heredoc) {
        out.push(line);
        var body = heredoc.tabs ? line.replace(/^\t+/, "") : line;
        if (body === heredoc.word) heredoc = null;
        return;
      }
      var command;
      if (continued) {
        command = line;
      } else if (PROMPT.test(line)) {
        command = line.slice(2);
      } else {
        return;
      }
      out.push(command);
      continued = /\\$/.test(command);
      var h = HEREDOC.exec(command);
      if (h) heredoc = { word: h[3], tabs: h[1] === "-" };
    });
    return out.join("\n");
  }

  function mark() {
    var blocks = document.querySelectorAll(".language-console.highlight code, .highlight.language-console code");
    Array.prototype.forEach.call(blocks, function (code) {
      if (code.hasAttribute("data-copy")) return;
      code.setAttribute("data-copy", commandsOf(code.textContent));
    });
  }

  root.stratumCommandsOf = commandsOf;
  if (typeof document === "undefined") return;
  if (root.document$ && typeof root.document$.subscribe === "function") {
    root.document$.subscribe(mark);
  } else if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", mark);
  } else {
    mark();
  }
})(typeof window !== "undefined" ? window : globalThis);
