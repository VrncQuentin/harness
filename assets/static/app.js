// Live updates. One EventSource carries every server-pushed update so we
// don't burn 4 of the browser's ~6 HTTP/1.1 connection slots per origin and
// stall navigation behind held-open SSE connections.
//
// The server tags each frame with `event: <type>`:
//   - state        → patches status-page badges, counters, queue, uptime
//   - llama-log    → appended to the llama log card (status page only)
//   - embed-log    → appended to the embedder log card (status page only)
//   - harness-log  → appended to the harness log card (status page only)
//
// Log handlers no-op when their target DOM nodes don't exist, so non-status
// pages still get state updates over the same connection at no extra cost.
(function () {
  if (typeof EventSource === 'undefined') return;
  var es = new EventSource('/events');

  es.addEventListener('state', function (evt) {
    var d;
    try { d = JSON.parse(evt.data); } catch (e) { return; }
    setBadge('llama-badge', d.llama_healthy, d.llama_failed);
    setBadge('embed-badge', d.embed_healthy, d.embed_failed);
    setText('llama-running', d.llama_running ? 'Yes' : 'No');
    setText('embed-running', d.embed_running ? 'Yes' : 'No');
    setText('llama-restarts', d.llama_restarts);
    setText('embed-restarts', d.embed_restarts);
    toggleHidden('llama-restart-form', !d.llama_failed);
    toggleHidden('embed-restart-form', !d.embed_failed);
    setQueue(d.queue_depth, d.queue_max);
    setUptime(d.uptime_seconds);
  });

  es.addEventListener('llama-log', logEventHandler('llama-log'));
  es.addEventListener('embed-log', logEventHandler('embed-log'));
  es.addEventListener('harness-log', logEventHandler('harness-log'));

  // The harness log card has a connection indicator. With one shared stream
  // it now reflects the /events connection itself.
  es.onopen = function () { setHarnessConnState('live', false); };
  es.onerror = function () { setHarnessConnState('disconnected', true); };
})();

function setHarnessConnState(text, disconnected) {
  var el = document.getElementById('harness-log-status');
  if (!el) return;
  el.textContent = text;
  if (disconnected) el.classList.add('is-disconnected');
  else el.classList.remove('is-disconnected');
}

function setBadge(id, ok, failed) {
  var el = document.getElementById(id);
  if (!el) return;
  var text = failed ? 'Failed' : (ok ? 'Healthy' : 'Unhealthy');
  el.textContent = text;
  el.className = 'badge ' + (ok ? 'badge-ok' : 'badge-err');
}

function toggleHidden(id, hidden) {
  var el = document.getElementById(id);
  if (!el) return;
  if (hidden) el.setAttribute('hidden', ''); else el.removeAttribute('hidden');
}

function setText(id, v) {
  var el = document.getElementById(id);
  if (el) el.textContent = v;
}

function setQueue(depth, max) {
  var qd = document.getElementById('queue-depth');
  if (qd) {
    qd.innerHTML = depth + '<span class="num-max"> / ' + max + '</span>';
  }
  var m = document.getElementById('queue-meter');
  if (m) {
    var pct = max > 0 ? (depth * 100 / max) : 0;
    m.style.width = pct + '%';
  }
}

function setUptime(s) {
  var el = document.getElementById('uptime');
  if (!el) return;
  el.textContent = formatUptime(s);
}

function formatUptime(s) {
  s = Math.max(0, Math.floor(s));
  var d = Math.floor(s / 86400); s -= d * 86400;
  var h = Math.floor(s / 3600);  s -= h * 3600;
  var m = Math.floor(s / 60);    s -= m * 60;
  if (d > 0) return d + 'd ' + h + 'h';
  if (h > 0) return h + 'h ' + m + 'm';
  if (m > 0) return m + 'm ' + s + 's';
  return s + 's';
}

// subscribeLogStream wires one log box to its SSE endpoint. Appends a row per
// entry, caps DOM size so a noisy run doesn't grow unbounded, and auto-scrolls
// only when the user is already pinned to the bottom.
function subscribeLogStream(bodyId, statusId, url) {
  var body = document.getElementById(bodyId);
  if (!body || typeof EventSource === 'undefined') return;
  var status = statusId ? document.getElementById(statusId) : null;

  var MAX_ROWS = 500;
  var es = new EventSource(url);

  es.onopen = function () {
    if (status) { status.textContent = 'live'; status.classList.remove('is-disconnected'); }
  };
  es.onerror = function () {
    if (status) { status.textContent = 'disconnected'; status.classList.add('is-disconnected'); }
  };
  es.onmessage = function (evt) {
    var entry;
    try { entry = JSON.parse(evt.data); } catch (e) { return; }

    var empty = body.querySelector('.log-empty');
    if (empty) empty.remove();

    var atBottom = (body.scrollHeight - body.scrollTop - body.clientHeight) < 4;

    var row = document.createElement('div');
    row.className = 'log-row';
    var t = document.createElement('span');
    t.className = 'log-time';
    t.textContent = entry.time || '';
    var l = document.createElement('span');
    l.className = 'log-line';
    l.textContent = entry.line || '';
    row.appendChild(t);
    row.appendChild(l);
    body.appendChild(row);

    while (body.childElementCount > MAX_ROWS) {
      body.removeChild(body.firstElementChild);
    }
    if (atBottom) body.scrollTop = body.scrollHeight;
  };
}

subscribeLogStream('llama-log', null, '/logs/llama');
subscribeLogStream('embed-log', null, '/logs/embed');
subscribeLogStream('harness-log', 'harness-log-status', '/logs/harness');

// Modal dialog wiring. Buttons with data-open-dialog="<id>" call
// showModal() on the matching <dialog>; buttons with data-close-dialog
// close the nearest enclosing dialog. Falls back to a confirm() popup
// if the browser does not support <dialog>, so the delete flow still
// asks for confirmation either way.
document.addEventListener('click', function (evt) {
  var openId = evt.target.getAttribute && evt.target.getAttribute('data-open-dialog');
  if (openId) {
    var dlg = document.getElementById(openId);
    if (dlg && typeof dlg.showModal === 'function') {
      evt.preventDefault();
      dlg.showModal();
      return;
    }
    // Browser without <dialog>: submit the form directly after a
    // native confirm. The button lives in a card next to its dialog
    // form, so resolve the form by id of the dialog. The button can
    // declare a custom prompt via data-confirm-text; otherwise fall
    // back to a generic message.
    if (dlg) {
      evt.preventDefault();
      var form = dlg.querySelector('form');
      var msg = (evt.target.getAttribute && evt.target.getAttribute('data-confirm-text')) || 'Are you sure?';
      if (form && window.confirm(msg)) {
        form.submit();
      }
    }
    return;
  }
  if (evt.target.hasAttribute && evt.target.hasAttribute('data-close-dialog')) {
    var nearest = evt.target.closest('dialog');
    if (nearest && typeof nearest.close === 'function') {
      evt.preventDefault();
      nearest.close();
    }
  }
});
