// Everything the browser does on its own. Roughly thirty lines, no framework,
// no build step: the server sends HTML, htmx swaps it, and this file only
// handles the two things HTML cannot express — keeping the timeline scrolled
// to the newest row, and telling the viewer when the stream has dropped.
(function () {
  var timeline = function () { return document.getElementById('timeline'); };
  var MAX_ROWS = 300;

  function pinToBottom() {
    var el = timeline();
    if (!el) return;
    el.scrollTop = el.scrollHeight;
    // The log is unbounded on the server side by design; the DOM is not.
    while (el.children.length > MAX_ROWS) el.removeChild(el.firstElementChild);
  }

  function setConnection(state, label) {
    var pip = document.querySelector('.conn .pip');
    var text = document.getElementById('conn-label');
    if (text) text.textContent = label;
    if (!pip) return;
    var colors = { live: 'var(--live)', retry: 'var(--medium)', down: 'var(--critical)' };
    pip.style.background = colors[state] || 'var(--idle)';
    pip.style.boxShadow = '0 0 8px ' + (colors[state] || 'transparent');
  }

  document.body.addEventListener('htmx:sseOpen', function () { setConnection('live', 'live'); });
  document.body.addEventListener('htmx:sseError', function () { setConnection('retry', 'reconnecting'); });
  document.body.addEventListener('htmx:sseClose', function () { setConnection('down', 'stream closed'); });

  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'timeline') pinToBottom();
  });

  document.addEventListener('DOMContentLoaded', pinToBottom);
  pinToBottom();
})();
