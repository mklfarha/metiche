// The sign-in page (BOARD_LOGIN §2.5). The link arrives as a URL fragment,
// /signin#<secret>, because a fragment is never sent to any server: not to the
// ingress, not to a log. This file takes it out of the address bar BEFORE it
// makes any request, then sends it once, in a POST body, to this same origin.
//
// No inline script anywhere: the page runs under script-src 'self'.
(function () {
  'use strict';

  var SLOT = 'metiche.signin.pending';
  var STATES = ['signin-explain', 'signin-progress', 'signin-failed', 'signin-unavailable',
    'signin-rate-limited', 'signin-forbidden'];

  // 1. Read the fragment.
  var hash = window.location.hash || '';
  // 2. Remove it from the URL and from this history entry. Nothing above this
  //    line touches the network, and nothing below it may run first.
  window.history.replaceState(null, '', '/signin');

  var secret = hash.charAt(0) === '#' ? hash.slice(1) : hash;

  function storage(fn) {
    try { return fn(window.sessionStorage); } catch (e) { return null; }
  }
  function clearSlot() { storage(function (s) { s.removeItem(SLOT); }); }

  function show(id) {
    for (var i = 0; i < STATES.length; i++) {
      var el = document.getElementById(STATES[i]);
      if (el) el.hidden = STATES[i] !== id;
    }
  }

  // With the fragment gone, a reload after "unavailable" can only retry if the
  // link is kept somewhere. sessionStorage is this tab only, and is cleared on
  // success or when the backend refused the link, and on nothing else.
  if (secret) {
    storage(function (s) { s.setItem(SLOT, secret); });
  } else {
    secret = storage(function (s) { return s.getItem(SLOT); }) || '';
  }

  // 3. No link: the explanation page.
  if (!secret) {
    show('signin-explain');
    return;
  }

  // A redirect is followed only if it is a path on this origin.
  function safePath(p) {
    return typeof p === 'string' && p.charAt(0) === '/' && p.charAt(1) !== '/' && p.charAt(1) !== '\\';
  }

  show('signin-progress');

  // 4. The one request that carries the link: in the body, same origin.
  window.fetch('/signin', {
    method: 'POST',
    credentials: 'same-origin',
    cache: 'no-store',
    referrerPolicy: 'no-referrer',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: 'link=' + encodeURIComponent(secret)
  }).then(function (res) {
    return res.json().catch(function () { return {}; });
  }).then(function (answer) {
    // 5. Signed in: go where the server says, replacing /signin in history.
    if (answer && safePath(answer.redirect)) {
      clearSlot();
      window.location.replace(answer.redirect);
      return;
    }
    var error = answer ? answer.error : '';
    // Only {"error":"link"} means the backend saw the link and refused it (or
    // it was malformed): used, expired or unknown. That is the one failure
    // message, and nothing is worth keeping for a retry.
    if (error === 'link') {
      clearSlot();
      show('signin-failed');
      return;
    }
    // The board's 429 and 403 are answered before the link reaches the
    // backend, so the link may well be good: keep it.
    if (error === 'rate_limited') {
      show('signin-rate-limited');
      return;
    }
    if (error === 'forbidden') {
      show('signin-forbidden');
      return;
    }
    // "unavailable", or an answer this page does not understand: nothing says
    // the link is spent, so keep it for a reload.
    show('signin-unavailable');
  }, function () {
    // The request never got an answer (offline, connection reset): the link
    // may well be unused, so this reads as "unavailable" and keeps it for a
    // reload.
    show('signin-unavailable');
  });
})();
