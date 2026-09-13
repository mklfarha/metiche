// metiche — the landing page's only script: a copy button for the install line.
//
// Progressive enhancement. The button ships with the `hidden` attribute and is
// revealed here, so with JS off there is no dead control and the command in
// the <pre> is still plain, selectable text. What gets copied is that text
// node's content — the same bytes a reader would select by hand.
(function () {
  document.querySelectorAll('button[data-copy]').forEach(function (btn) {
    var src = document.getElementById(btn.getAttribute('data-copy'));
    if (!src) return;
    var label = btn.textContent;
    var timer;
    btn.hidden = false;
    btn.addEventListener('click', function () {
      var text = src.textContent.trim();
      var done = function (msg) {
        btn.textContent = msg;
        clearTimeout(timer);
        timer = setTimeout(function () { btn.textContent = label; }, 1600);
      };
      // Fallback for browsers without the async clipboard (or a non-secure
      // origin): select the line and try the legacy copy. If even that is
      // refused, the line is left selected for a manual copy.
      var legacy = function () {
        var range = document.createRange();
        range.selectNodeContents(src);
        var sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        var ok = false;
        try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
        done(ok ? 'Copied' : 'Selected');
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { done('Copied'); }, legacy);
      } else {
        legacy();
      }
    });
  });
})();
