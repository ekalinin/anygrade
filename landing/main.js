// anygrade landing - progressive enhancement.
// Everything here is optional: with JS disabled the page is fully usable.
(function () {
  "use strict";

  // ---- helpers ----------------------------------------------------------
  function esc(s) {
    return s.replace(/[&<>]/g, function (c) {
      return c === "&" ? "&amp;" : c === "<" ? "&lt;" : "&gt;";
    });
  }

  // Index of the first `#` that starts a comment (line start or after
  // whitespace, and not inside a quoted string).
  function commentStart(line) {
    var q = null;
    for (var i = 0; i < line.length; i++) {
      var ch = line[i];
      if (q) {
        if (ch === q) q = null;
      } else if (ch === '"' || ch === "'") {
        q = ch;
      } else if (ch === "#" && (i === 0 || /\s/.test(line[i - 1]))) {
        return i;
      }
    }
    return -1;
  }

  // Tokenize a raw (unescaped) string for quoted strings + numbers.
  // Every emitted segment is escaped, so injection is impossible.
  function inline(raw, withNumbers) {
    var re = withNumbers
      ? /("[^"]*"|'[^']*')|(\b\d[\w:+.\-]*)/g
      : /("[^"]*"|'[^']*')/g;
    var out = "";
    var last = 0;
    var m;
    while ((m = re.exec(raw))) {
      if (m.index > last) out += esc(raw.slice(last, m.index));
      if (m[1] !== undefined) {
        out += '<span class="tok-string">' + esc(m[1]) + "</span>";
      } else {
        out += '<span class="tok-num">' + esc(m[2]) + "</span>";
      }
      last = m.index + m[0].length;
    }
    out += esc(raw.slice(last));
    return out;
  }

  function highlightCode(code, lang) {
    if (lang === "yaml") {
      var key = code.match(/^(\s*)([\w.\-]+)(:)(.*)$/);
      if (key) {
        return (
          esc(key[1]) +
          '<span class="tok-key">' + esc(key[2]) + "</span>" +
          esc(key[3]) +
          inline(key[4], true)
        );
      }
      return inline(code, true);
    }
    // sh: colour the leading command word, then quoted strings.
    var cmd = code.match(/^(\s*)((?:\.\/)?[\w./\-]+)(.*)$/);
    if (cmd) {
      return (
        esc(cmd[1]) +
        '<span class="tok-cmd">' + esc(cmd[2]) + "</span>" +
        inline(cmd[3], false)
      );
    }
    return inline(code, false);
  }

  function highlight(pre) {
    var lang = pre.getAttribute("data-lang");
    var code = pre.querySelector("code");
    if (!code) return;
    var lines = code.textContent.split("\n");
    var html = lines
      .map(function (line) {
        var c = commentStart(line);
        if (c === 0) return '<span class="tok-comment">' + esc(line) + "</span>";
        if (c > 0) {
          return (
            highlightCode(line.slice(0, c), lang) +
            '<span class="tok-comment">' + esc(line.slice(c)) + "</span>"
          );
        }
        return highlightCode(line, lang);
      })
      .join("\n");
    code.innerHTML = html;
  }

  // ---- copy-to-clipboard ------------------------------------------------
  function addCopy(pre) {
    var code = pre.querySelector("code");
    if (!code || !navigator.clipboard) return;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy-btn";
    btn.textContent = "Copy";
    btn.setAttribute("aria-label", "Copy to clipboard");
    btn.addEventListener("click", function () {
      navigator.clipboard.writeText(code.textContent).then(function () {
        btn.textContent = "Copied";
        btn.classList.add("copied");
        setTimeout(function () {
          btn.textContent = "Copy";
          btn.classList.remove("copied");
        }, 1500);
      });
    });
    pre.appendChild(btn);
  }

  // ---- tabs (ARIA tabs pattern) -----------------------------------------
  function initTabs(group) {
    var tabs = Array.prototype.slice.call(group.querySelectorAll('[role="tab"]'));
    if (tabs.length < 2) return;
    group.classList.add("js-tabs");

    function panelFor(tab) {
      return document.getElementById(tab.getAttribute("aria-controls"));
    }
    function select(tab) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.tabIndex = on ? 0 : -1;
        var p = panelFor(t);
        if (p) p.hidden = !on;
      });
    }
    tabs.forEach(function (tab, i) {
      tab.addEventListener("click", function () { select(tab); });
      tab.addEventListener("keydown", function (e) {
        var next = null;
        if (e.key === "ArrowRight") next = tabs[(i + 1) % tabs.length];
        else if (e.key === "ArrowLeft") next = tabs[(i - 1 + tabs.length) % tabs.length];
        else if (e.key === "Home") next = tabs[0];
        else if (e.key === "End") next = tabs[tabs.length - 1];
        if (next) { e.preventDefault(); select(next); next.focus(); }
      });
    });
    var initial = tabs.filter(function (t) {
      return t.getAttribute("aria-selected") === "true";
    })[0] || tabs[0];
    select(initial);
  }

  // ---- boot -------------------------------------------------------------
  var blocks = document.querySelectorAll("pre[data-lang]");
  Array.prototype.forEach.call(blocks, function (pre) {
    highlight(pre);
    addCopy(pre);
  });
  Array.prototype.forEach.call(document.querySelectorAll("[data-tabs]"), initTabs);
})();
