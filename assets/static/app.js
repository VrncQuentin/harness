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
    setUptime(d.uptime_text);
  });

  es.addEventListener('llama-log', logEventHandler('llama-log'));
  es.addEventListener('embed-log', logEventHandler('embed-log'));
  es.addEventListener('harness-log', logEventHandler('harness-log'));

  // The harness log card has a connection indicator. With one shared stream
  // it now reflects the /events connection itself.
  es.onopen = function () { setHarnessConnState('live', false); };
  es.onerror = function () { setHarnessConnState('disconnected', true); };
})();

// Task page wiring. This is intentionally still the existing browser-owned
// streaming flow; Phase 1 moves it server-side in a later, behavior-changing PR.
(function () {
  var form = document.getElementById('task-form');
  if (!form) return;

  var input = document.getElementById('task-input');
  var transcript = document.getElementById('task-transcript');
  var sendBtn = document.getElementById('task-send');
  var stopBtn = document.getElementById('task-stop');
  var clearBtn = document.getElementById('task-clear');
  var controller = null;
  var messages = [];
  var sessionId = '';

  function pushMsg(type, data) {
    var el = document.createElement('div');
    el.className = 'chat-msg';
    switch (type) {
      case 'user':
        el.classList.add('is-user');
        el.innerHTML = '<span class="chat-role">You</span><div class="chat-body">' + esc(data) + '</div>';
        break;
      case 'text':
        el.classList.add('is-assistant');
        el.innerHTML = '<span class="chat-role">Assistant</span><div class="chat-body">' + esc(data) + '</div>';
        break;
      case 'tool_call':
        el.classList.add('is-tool');
        el.innerHTML = '<span class="chat-role">Tool call</span><div class="chat-body"><code>' + esc(data.tool_id) + '</code><pre class="tool-args">' + esc(data.tool_args || '') + '</pre></div>';
        break;
      case 'tool_result':
        el.classList.add('is-tool-result');
        var body = data.tool_error ? '<div class="tool-error">ERROR: ' + esc(data.tool_error) + '</div>' : '';
        if (data.tool_result) body += '<pre class="tool-content">' + esc(data.tool_result) + '</pre>';
        el.innerHTML = '<span class="chat-role">Result</span><div class="chat-body"><code>' + esc(data.tool_id) + '</code>' + body + '</div>';
        break;
      case 'done':
        el.classList.add('is-system');
        el.innerHTML = '<span class="chat-role">Done</span><div class="chat-body">Task completed in ' + data.turn + ' turn(s).</div>';
        break;
      case 'limit':
        el.classList.add('is-system', 'is-warn');
        el.innerHTML = '<span class="chat-role">Limit</span><div class="chat-body">' + esc(data.content) + '</div>';
        break;
      case 'doom_loop':
        el.classList.add('is-system', 'is-err');
        el.innerHTML = '<span class="chat-role">Loop detected</span><div class="chat-body">' + esc(data.content) + '</div>';
        break;
      case 'error':
        el.classList.add('is-system', 'is-err');
        el.innerHTML = '<span class="chat-role">Error</span><div class="chat-body">' + esc(data.content) + '</div>';
        break;
      case 'cancelled':
        el.classList.add('is-system', 'is-warn');
        el.innerHTML = '<span class="chat-role">Cancelled</span><div class="chat-body">Task was stopped.</div>';
        break;
      default:
        return;
    }
    transcript.appendChild(el);
    transcript.scrollTop = transcript.scrollHeight;
  }

  function esc(s) {
    if (!s) return '';
    var el = document.createElement('span');
    el.textContent = s;
    return el.innerHTML;
  }

  function setRunning(v) {
    sendBtn.style.display = v ? 'none' : '';
    stopBtn.style.display = v ? '' : 'none';
    input.disabled = v;
  }

  form.addEventListener('submit', function (evt) {
    evt.preventDefault();
    var text = input.value.trim();
    if (!text) return;
    input.value = '';
    messages.push({ role: 'user', content: text });
    pushMsg('user', text);

    setRunning(true);
    controller = new AbortController();
    fetch('/task/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agent: '', session_id: sessionId, messages: messages }),
      signal: controller.signal,
    }).then(function (resp) {
      if (!resp.ok) {
        setRunning(false);
        pushMsg('error', { content: 'Request failed: ' + resp.status });
        return null;
      }
      return drainTaskStream(resp.body);
    }).catch(function (err) {
      if (err.name !== 'AbortError') pushMsg('error', { content: err.message });
    }).finally(function () {
      setRunning(false);
      controller = null;
    });
  });

  function drainTaskStream(body) {
    var reader = body.getReader();
    var decoder = new TextDecoder();
    var buf = '';

    function pump() {
      return reader.read().then(function (chunk) {
        if (chunk.done) return;
        buf += decoder.decode(chunk.value, { stream: true });
        var parts = buf.split('\n\n');
        buf = parts.pop();
        parts.forEach(handleTaskFrame);
        return pump();
      });
    }

    return pump();
  }

  function handleTaskFrame(part) {
    if (!part.trim()) return;
    var frame;
    try { frame = JSON.parse(part); } catch (err) { return; }
    if (frame.event === 'session') {
      sessionId = frame.data.session_id;
      return;
    }
    if (frame.data && frame.data.error) {
      pushMsg('error', { content: frame.data.error });
      return;
    }
    if (frame.data) pushMsg(frame.event, frame.data);
    if (frame.data && frame.data.terminate) {
      setRunning(false);
      controller = null;
    }
  }

  stopBtn.addEventListener('click', function () {
    if (controller) controller.abort();
  });

  clearBtn.addEventListener('click', function () {
    transcript.innerHTML = '';
    messages = [];
    sessionId = '';
  });
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

function setUptime(text) {
  var el = document.getElementById('uptime');
  if (!el) return;
  if (text) el.textContent = text;
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

// Chat page wiring. Activates when the chat shell is on the page; the
// transcript lives in this module's `messages` array. M3 adds explicit
// session handling: each /chat/stream POST carries the current
// session_id, the server returns one as the first SSE event, and the
// Save/New/Resume controls let the user persist and revisit episodes.
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
  var saveBtn = document.getElementById('chat-save');
  var newBtn = document.getElementById('chat-new');
  var savedEl = document.getElementById('chat-saved');
  var sessionIDEl = document.getElementById('chat-session-id');
  var resumeEl = document.getElementById('chat-resume');
  var resumeBodyEl = document.getElementById('chat-resume-body');

  var agent = root.getAttribute('data-agent') || '';
  var messages = [];
  var inFlight = null; // AbortController while a request is open.
  var currentSessionID = '';
  var dirty = false;

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
    clearSaved();
    setStatus('');
  });

  if (saveBtn) {
    saveBtn.addEventListener('click', function () {
      if (!currentSessionID) {
        showError('Send at least one message before saving.');
        return;
      }
      saveSession();
    });
  }

  if (newBtn) {
    newBtn.addEventListener('click', function () {
      if (inFlight) inFlight.abort();
      // Best-effort save of the current session before resetting so
      // users do not lose work by clicking "New" too quickly.
      if (currentSessionID && dirty) {
        saveSession({ silent: true }).finally(resetSession);
      } else {
        resetSession();
      }
    });
  }

  if (resumeBodyEl) {
    resumeBodyEl.querySelectorAll('[data-session-id]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        resumeSession(btn.getAttribute('data-session-id') || '', btn.getAttribute('data-session-agent') || '');
      });
    });
  }

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
    dirty = true;
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
  function setSaved(msg) {
    if (!savedEl) return;
    savedEl.textContent = msg;
    savedEl.removeAttribute('hidden');
  }
  function clearSaved() {
    if (!savedEl) return;
    savedEl.textContent = '';
    savedEl.setAttribute('hidden', '');
  }
  function setSessionID(id) {
    currentSessionID = id || '';
    if (sessionIDEl) {
      sessionIDEl.textContent = currentSessionID || '(unsaved)';
    }
  }

  function setBusy(busy) {
    sendBtn.disabled = busy;
    inputEl.disabled = busy;
    if (busy) stopBtn.removeAttribute('hidden');
    else stopBtn.setAttribute('hidden', '');
  }

  function resetSession() {
    setSessionID('');
    messages = [];
    transcriptEl.innerHTML = '<p class="chat-empty">Send a message to begin.</p>';
    clearError();
    clearSaved();
    setStatus('');
    dirty = false;
  }

  function sendChat() {
    clearError();
    clearSaved();
    setBusy(true);
    setStatus('thinking...');

    var assistant = pushMessage('assistant', '');
    assistant.el.classList.add('is-streaming');
    // The server sees the assistant placeholder we just appended so we
    // strip it from the request body - only completed turns go up.
    var payload = {
      agent: agent,
      session_id: currentSessionID,
      messages: messages.slice(0, -1),
    };

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
        var idx = buffer.indexOf('\n\n');
        while (idx !== -1) {
          var frame = buffer.slice(0, idx);
          buffer = buffer.slice(idx + 2);
          handleFrame(frame, assistant);
          if (streamErr) return;
          idx = buffer.indexOf('\n\n');
        }
        return pump();
      });
    }

    function handleFrame(frame, assistant) {
      // SSE frames may carry an `event:` tag and one or more `data:`
      // lines. We treat the session frame specially so the browser can
      // pin subsequent calls without parsing every JSON payload.
      var eventName = '';
      var dataLines = [];
      frame.split('\n').forEach(function (line) {
        if (line.indexOf('event:') === 0) {
          eventName = line.slice(6).replace(/^ /, '').trim();
        } else if (line.indexOf('data:') === 0) {
          dataLines.push(line.slice(5).replace(/^ /, ''));
        }
      });
      if (dataLines.length === 0) return;
      var data = dataLines.join('\n');
      var obj;
      try { obj = JSON.parse(data); } catch (e) { return; }
      if (eventName === 'session') {
        if (obj && obj.id) {
          setSessionID(obj.id);
        }
        return;
      }
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
    dirty = true;
  }

  function saveSession(opts) {
    opts = opts || {};
    setStatus('saving...');
    if (saveBtn) saveBtn.disabled = true;
    return fetch('/chat/save', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: currentSessionID }),
    }).then(function (resp) {
      if (!resp.ok) {
        return resp.text().then(function (body) {
          var msg = body;
          try { msg = JSON.parse(body).error || body; } catch (e) { /* keep raw */ }
          throw new Error(msg || ('HTTP ' + resp.status));
        });
      }
      return resp.json();
    }).then(function (res) {
      if (!opts.silent) {
        setSaved('Saved session ' + (res.id || '') + ' (seq ' + (res.save_seq || 1) + ').');
      }
      setStatus('saved');
      dirty = false;
    }).catch(function (err) {
      showError('Save failed: ' + (err.message || String(err)));
      setStatus('save failed');
    }).finally(function () {
      if (saveBtn) saveBtn.disabled = false;
    });
  }

  function resumeSession(id, recAgent) {
    setStatus('resuming...');
    fetch('/chat/session?agent=' + encodeURIComponent(recAgent) + '&id=' + encodeURIComponent(id))
      .then(function (resp) {
        if (resp.status === 404) {
          throw new Error('Conversation history not available for this session.');
        }
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        return resp.json();
      })
      .then(function (data) {
        messages = (data.messages || []).slice();
        transcriptEl.innerHTML = '';
        if (messages.length === 0) {
          transcriptEl.innerHTML = '<p class="chat-empty">Send a message to begin.</p>';
        } else {
          messages.forEach(function (m) {
            // pushMessage appends to messages too; rebuild manually.
            renderMessage(m.role, m.content);
          });
        }
        setSessionID(id);
        clearError();
        clearSaved();
        setStatus('resumed');
        dirty = false;
        if (resumeEl) resumeEl.open = false;
      })
      .catch(function (err) {
        showError('Resume failed: ' + (err.message || String(err)));
        setStatus('resume failed');
      });
  }

  function renderMessage(role, content) {
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
  }
})();
