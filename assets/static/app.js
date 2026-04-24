// Live status stream. Uses a plain EventSource because the only thing the
// client does with each frame is patch a handful of DOM nodes by id - no
// swapping, no htmx needed.
(function () {
  if (typeof EventSource === 'undefined') return;
  var es = new EventSource('/events');
  es.onmessage = function (evt) {
    var d;
    try { d = JSON.parse(evt.data); } catch (e) { return; }
    setBadge('llama-badge', d.llama_healthy);
    setBadge('embed-badge', d.embed_healthy);
    setText('llama-running', d.llama_running ? 'Yes' : 'No');
    setText('embed-running', d.embed_running ? 'Yes' : 'No');
    setText('llama-restarts', d.llama_restarts);
    setText('embed-restarts', d.embed_restarts);
    setProcOutput('llama', d.llama_output);
    setProcOutput('embed', d.embed_output);
    setQueue(d.queue_depth, d.queue_max);
    setUptime(d.uptime_seconds);
  };
})();

function setBadge(id, ok) {
  var el = document.getElementById(id);
  if (!el) return;
  el.textContent = ok ? 'Healthy' : 'Unhealthy';
  el.className = 'badge ' + (ok ? 'badge-ok' : 'badge-err');
}

function setText(id, v) {
  var el = document.getElementById(id);
  if (el) el.textContent = v;
}

// setProcOutput swaps the <pre> contents for a process card in place. Auto-
// scroll only when the user was already pinned to the bottom, so scrolling
// up to read a past line isn't yanked back on the next SSE frame.
function setProcOutput(prefix, lines) {
  var pre = document.getElementById(prefix + '-output');
  var empty = document.getElementById(prefix + '-output-empty');
  var count = document.getElementById(prefix + '-output-count');
  if (!pre || !empty || !count) return;
  lines = lines || [];
  if (lines.length === 0) {
    pre.hidden = true;
    pre.textContent = '';
    empty.hidden = false;
    count.textContent = '';
    return;
  }
  var atBottom = (pre.scrollHeight - pre.scrollTop - pre.clientHeight) < 4;
  var next = lines.join('\n') + '\n';
  if (pre.textContent !== next) {
    pre.textContent = next;
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  }
  pre.hidden = false;
  empty.hidden = true;
  count.textContent = ' (' + lines.length + ' lines)';
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

// Live harness log streaming. Caps DOM rows so a noisy run doesn't grow
// unbounded. Auto-scrolls only when the user is already at the bottom so
// scrolling up to read a past line isn't yanked away.
(function () {
  var body = document.getElementById('logs-body');
  var meta = document.getElementById('logs-status');
  if (!body || typeof EventSource === 'undefined') return;

  var MAX_ROWS = 500;
  var es = new EventSource('/logs/events');

  es.onopen = function () {
    if (meta) { meta.textContent = 'live'; meta.classList.remove('is-disconnected'); }
  };
  es.onerror = function () {
    if (meta) { meta.textContent = 'disconnected'; meta.classList.add('is-disconnected'); }
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
})();
