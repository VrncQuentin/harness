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

// logEventHandler appends multiplexed log events to one log box, caps DOM
// size, and auto-scrolls only when the user is already pinned to the bottom.
function logEventHandler(bodyId) {
  return function (evt) {
    var body = document.getElementById(bodyId);
    if (!body) return;

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

    while (body.childElementCount > 500) {
      body.removeChild(body.firstElementChild);
    }
    if (atBottom) body.scrollTop = body.scrollHeight;
  };
}

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

// Chat page wiring. Activates when the chat shell is on the page; the
// transcript lives in this module's `messages` array, posted in full
// each turn since the server is stateless until M3 sessions land.
(function () {
  var root = document.getElementById('chat-root');
  if (!root) return;

  var transcriptEl = document.getElementById('chat-transcript');
  var formEl = document.getElementById('chat-form');
  var inputEl = document.getElementById('chat-input');
  var sendBtn = document.getElementById('chat-send');
  var stopBtn = document.getElementById('chat-stop');
  var statusEl = document.getElementById('chat-status');
  var errorEl = document.getElementById('chat-error');
  var clearBtn = document.getElementById('chat-clear');

  var agent = root.getAttribute('data-agent') || '';
  var messages = [];
  var inFlight = null; // AbortController while a request is open.

  formEl.addEventListener('submit', function (evt) {
    evt.preventDefault();
    var text = (inputEl.value || '').trim();
    if (!text || inFlight) return;
    pushMessage('user', text);
    inputEl.value = '';
    inputEl.style.height = '';
    sendChat();
  });

  // Enter sends, Shift+Enter inserts a newline. Matches the placeholder hint.
  inputEl.addEventListener('keydown', function (evt) {
    if (evt.key === 'Enter' && !evt.shiftKey) {
      evt.preventDefault();
      formEl.requestSubmit();
    }
  });

  stopBtn.addEventListener('click', function () {
    if (inFlight) inFlight.abort();
  });

  clearBtn.addEventListener('click', function () {
    if (inFlight) inFlight.abort();
    messages = [];
    transcriptEl.innerHTML = '<p class="chat-empty">Send a message to begin.</p>';
    clearError();
    setStatus('');
  });

  function pushMessage(role, content) {
    messages.push({ role: role, content: content });
    var empty = transcriptEl.querySelector('.chat-empty');
    if (empty) empty.remove();
    var el = document.createElement('div');
    el.className = 'chat-msg is-' + role;
    var roleEl = document.createElement('span');
    roleEl.className = 'chat-msg-role';
    roleEl.textContent = role === 'user' ? 'You' : (role === 'assistant' ? agent : role);
    var bodyEl = document.createElement('span');
    bodyEl.className = 'chat-msg-body';
    bodyEl.textContent = content;
    el.appendChild(roleEl);
    el.appendChild(bodyEl);
    transcriptEl.appendChild(el);
    transcriptEl.scrollTop = transcriptEl.scrollHeight;
    return { el: el, body: bodyEl };
  }

  function setStatus(text) { statusEl.textContent = text || ''; }
  function showError(msg) {
    errorEl.textContent = msg;
    errorEl.removeAttribute('hidden');
  }
  function clearError() {
    errorEl.textContent = '';
    errorEl.setAttribute('hidden', '');
  }

  function setBusy(busy) {
    sendBtn.disabled = busy;
    inputEl.disabled = busy;
    if (busy) stopBtn.removeAttribute('hidden');
    else stopBtn.setAttribute('hidden', '');
  }

  function sendChat() {
    clearError();
    setBusy(true);
    setStatus('thinking...');

    var assistant = pushMessage('assistant', '');
    assistant.el.classList.add('is-streaming');
    // The server sees the assistant placeholder we just appended so we
    // strip it from the request body - only completed turns go up.
    var payload = { agent: agent, messages: messages.slice(0, -1) };

    inFlight = new AbortController();
    fetch('/chat/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      signal: inFlight.signal,
    }).then(function (resp) {
      if (!resp.ok) {
        return resp.text().then(function (body) {
          var msg = body;
          try { msg = JSON.parse(body).error || body; } catch (e) { /* keep raw */ }
          throw new Error(msg || ('HTTP ' + resp.status));
        });
      }
      return drainStream(resp.body, assistant);
    }).then(function () {
      finalizeAssistant(assistant);
      setStatus('');
    }).catch(function (err) {
      if (err.name === 'AbortError') {
        finalizeAssistant(assistant, '[stopped]');
        setStatus('stopped');
      } else {
        finalizeAssistant(assistant, err.message ? '[error: ' + err.message + ']' : '[error]');
        showError(err.message || String(err));
        setStatus('error');
      }
    }).finally(function () {
      inFlight = null;
      setBusy(false);
      inputEl.focus();
    });
  }

  function drainStream(body, assistant) {
    if (!body || !body.getReader) {
      return Promise.reject(new Error('streaming not supported in this browser'));
    }
    var reader = body.getReader();
    var decoder = new TextDecoder('utf-8');
    var buffer = '';
    var streamErr = null;

    function pump() {
      return reader.read().then(function (chunk) {
        if (chunk.done) return;
        buffer += decoder.decode(chunk.value, { stream: true });
        var idx;
        while ((idx = buffer.indexOf('\n\n')) !== -1) {
          var frame = buffer.slice(0, idx);
          buffer = buffer.slice(idx + 2);
          handleFrame(frame, assistant);
          if (streamErr) return;
        }
        return pump();
      });
    }

    function handleFrame(frame, assistant) {
      // SSE frame may have several `data:` lines; concatenate per spec.
      var dataLines = [];
      frame.split('\n').forEach(function (line) {
        if (line.indexOf('data:') === 0) {
          dataLines.push(line.slice(5).replace(/^ /, ''));
        }
      });
      if (dataLines.length === 0) return;
      var data = dataLines.join('\n');
      var obj;
      try { obj = JSON.parse(data); } catch (e) { return; }
      if (obj.error) {
        streamErr = new Error(obj.error);
        throw streamErr;
      }
      if (obj.done) {
        return;
      }
      if (typeof obj.content === 'string' && obj.content.length > 0) {
        assistant.body.textContent += obj.content;
        transcriptEl.scrollTop = transcriptEl.scrollHeight;
      }
    }

    return pump();
  }

  function finalizeAssistant(assistant, suffix) {
    assistant.el.classList.remove('is-streaming');
    if (suffix) {
      assistant.body.textContent += (assistant.body.textContent ? '\n\n' : '') + suffix;
    }
    // Persist the assembled content back into the message log so the
    // next turn carries it as history.
    if (messages.length > 0 && messages[messages.length - 1].role === 'assistant') {
      messages[messages.length - 1].content = assistant.body.textContent;
    }
  }
})();
