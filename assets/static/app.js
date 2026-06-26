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
    // Stopping an active SSE stream is not yet wired; the runner will
    // complete on its own. The UI returns to idle when chat-done or
    // chat-error arrives.
  });

  clearBtn.addEventListener('click', function () {
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
  var newBtn = document.getElementById('chat-new');
  var savedEl = document.getElementById('chat-saved');
  var sessionIDEl = document.getElementById('chat-session-id');
  var resumeEl = document.getElementById('chat-resume');

  var agent = root.getAttribute('data-agent') || '';
  var messages = [];
  var currentSessionID = '';

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
  // sync JS state. Token streaming is handled by htmx SSE via the
  // /chat/events connection on #chat-root.
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
    // Push an assistant placeholder so the payload logic works.
    messages.push({ role: 'assistant', content: '' });
    // Sync the hidden messages field for the next turn.
    var mi = document.getElementById('chat-messages-input');
    if (mi) mi.value = JSON.stringify(messages.slice(0, -1));
    setBusy(true);
    setStatus('thinking...');
  });

  // Token streaming is handled server-side via /chat/events SSE.
  // The assistant placeholder's .chat-msg-body consumes chat-token
  // events via sse-swap. These handlers sync JS state on done/error.
  document.body.addEventListener('htmx:sseMessage', function (evt) {
    var e = evt.detail;
    if (e.type === 'chat-done') {
      var el = document.getElementById('chat-assistant');
      if (el) {
        el.classList.remove('is-streaming');
        el.removeAttribute('id');
        var body = el.querySelector('.chat-msg-body');
        if (messages.length > 0 && messages[messages.length - 1].role === 'assistant') {
          messages[messages.length - 1].content = body ? body.textContent : '';
        }
      }
      setBusy(false);
      setStatus('');
      inputEl.focus();
    }
    if (e.type === 'chat-error') {
      var el2 = document.getElementById('chat-assistant');
      if (el2) {
        el2.classList.remove('is-streaming');
        el2.removeAttribute('id');
        var body2 = el2.querySelector('.chat-msg-body');
        if (body2) body2.textContent += (body2.textContent ? '\n\n' : '') + '[error]';
      }
      showError(e.data || 'stream error');
      setBusy(false);
      setStatus('error');
    }
    if (e.type === 'chat-session') {
      try {
        var obj = JSON.parse(e.data);
        if (obj && obj.id) setSessionID(obj.id);
      } catch (_) { /* ignore */ }
    }
  });
})();
