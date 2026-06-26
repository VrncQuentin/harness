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

  var agent = root.getAttribute('data-agent') || '';
  var messages = [];
  var inFlight = null; // AbortController while a request is open.
  var currentSessionID = '';
  var dirty = false;

  // Form submit is handled by htmx (hx-post="/chat/send").
  // Enter sends, Shift+Enter inserts a newline; textarea does not
  // auto-submit on Enter so we keep a minimal key handler.
  inputEl.addEventListener('keydown', function (evt) {
    if (evt.key === 'Enter' && !evt.shiftKey) {
      evt.preventDefault();
      htmx.trigger(formEl, 'submit');
    }
  });

  // After htmx swaps the server-rendered user message + assistant
  // placeholder, the server fires a chatSend browser event so we can
  // sync JS state and start the streaming fetch.
  document.body.addEventListener('chatSend', function () {
    var sessionInput = document.getElementById('chat-session-input');
    if (sessionInput && sessionInput.value) {
      setSessionID(sessionInput.value);
    }
    // Read the user message from the just-rendered DOM fragment.
    var userEls = transcriptEl.querySelectorAll('.chat-msg.is-user');
    var lastUser = userEls[userEls.length - 1];
    if (lastUser) {
      var body = lastUser.querySelector('.chat-msg-body');
      if (body) {
        messages.push({ role: 'user', content: body.textContent });
      }
    }
    // Push an assistant placeholder so the payload logic strips it.
    messages.push({ role: 'assistant', content: '' });
    sendChat();
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
    var si = document.getElementById('chat-session-input');
    if (si) si.value = '';
  });

  // Save is handled by htmx (hx-post="/chat/save" with
  // hx-include="#chat-session-input" on the save button). The server
  // returns an HTML confirmation fragment.

  if (newBtn) {
    newBtn.addEventListener('click', function () {
      if (inFlight) inFlight.abort();
      resetSession();
    });
  }

  // Resume is handled server-side via htmx: the resume buttons carry
  // hx-get, hx-target, and hx-swap so clicking them fetches an HTML
  // transcript fragment from /chat/session and swaps it into place.
  // The fragment also delivers session id + messages data via hidden
  // OOB swaps read below.
  document.body.addEventListener('htmx:afterSettle', function () {
    var state = document.getElementById('chat-state');
    if (!state) return;
    var sid = state.getAttribute('data-session-id');
    if (sid) {
      setSessionID(sid);
    }
    var msgsJSON = state.getAttribute('data-messages');
    if (msgsJSON) {
      try { messages = JSON.parse(msgsJSON); } catch (e) { /* ignore */ }
    }
    dirty = false;
    clearError();
    clearSaved();
    setStatus('resumed');
    if (resumeEl) resumeEl.open = false;
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
    var si = document.getElementById('chat-session-input');
    if (si) si.value = currentSessionID;
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
    // Sync hidden form fields so the next send starts a fresh session.
    var si = document.getElementById('chat-session-input');
    if (si) si.value = '';
  }

  function sendChat() {
    clearError();
    clearSaved();
    setBusy(true);
    setStatus('thinking...');

    // The assistant placeholder is already in the DOM, rendered by
    // the /chat/send handler. Find it by its id and build the same
    // {el, body} shape that drainStream / finalizeAssistant expect.
    var assistantEl = document.getElementById('chat-assistant');
    if (!assistantEl) {
      showError('Could not find assistant placeholder.');
      setBusy(false);
      return;
    }
    var assistant = {
      el: assistantEl,
      body: assistantEl.querySelector('.chat-msg-body'),
    };

    var payload = {
      agent: agent,
      session_id: currentSessionID,
      // Strip the last (assistant placeholder) so the server
      // assembles only completed turns.
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
      // lines. Content frames are plain text (no JSON wrapper); the
      // session frame still uses JSON since it carries structured data.
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
      if (eventName === 'session') {
        var obj;
        try { obj = JSON.parse(data); } catch (e) { return; }
        if (obj && obj.id) setSessionID(obj.id);
        return;
      }
      if (eventName === 'chat-error') {
        streamErr = new Error(data);
        throw streamErr;
      }
      if (eventName === 'chat-done') {
        return;
      }
      // Default: content frame. Append plain text directly.
      if (data.length > 0) {
        assistant.body.textContent += data;
        transcriptEl.scrollTop = transcriptEl.scrollHeight;
      }
    }

    return pump();
  }

  function finalizeAssistant(assistant, suffix) {
    assistant.el.classList.remove('is-streaming');
    // Remove the id so the next /chat/send response can use it for a
    // new pre-rendered assistant placeholder.
    assistant.el.removeAttribute('id');
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
})();
