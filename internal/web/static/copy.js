// anygrade web UI - copy buttons, progressive enhancement.
// Nothing here is required for the page to work: the buttons are created in
// the browser, so with JavaScript off the markup is exactly what shipped
// before and every value stays selectable by hand.
(function () {
  "use strict";

  // navigator.clipboard exists only in a secure context - HTTPS, or a loopback
  // host. `serve` still allows a plaintext non-loopback bind (with a warning),
  // and there the API is absent altogether, so bail out and render nothing
  // rather than a control that could never work. Same guard the landing script
  // uses (landing/main.js).
  if (!navigator.clipboard) return;

  // Fallback for a rejected write (denied permission, unfocused document):
  // put the value under the caret so the student can finish with Ctrl+C
  // instead of being left with a button that appears to do nothing.
  function selectText(el) {
    var sel = window.getSelection();
    if (!sel) return;
    var range = document.createRange();
    range.selectNodeContents(el);
    sel.removeAllRanges();
    sel.addRange(range);
  }

  function addCopy(el, labels) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy-btn";
    btn.textContent = labels.copy;
    // The label swap is the only feedback a click gives, and the button's own
    // text is its accessible name - announce the change instead of pinning the
    // name with an aria-label that would hide the confirmation.
    btn.setAttribute("aria-live", "polite");
    var timer;
    btn.addEventListener("click", function () {
      navigator.clipboard.writeText(el.textContent.replace(/\s+$/, "")).then(function () {
        btn.textContent = labels.done;
        btn.classList.add("copied");
        clearTimeout(timer);
        timer = setTimeout(function () {
          btn.textContent = labels.copy;
          btn.classList.remove("copied");
        }, 1500);
      }, function () {
        selectText(el);
      });
    });
    // After the block, not floating over it: the token wraps to several lines
    // in the narrow login sheet and these `pre` blocks carry no padding an
    // overlay could sit in, so a corner button would cover the very text it
    // copies.
    el.parentNode.insertBefore(btn, el.nextSibling);
  }

  Array.prototype.forEach.call(document.querySelectorAll("[data-copy]"), function (root) {
    var labels = { copy: root.dataset.copyLabel, done: root.dataset.copyDone };
    Array.prototype.forEach.call(root.querySelectorAll("code.token, pre"), function (el) {
      addCopy(el, labels);
    });
  });
})();
