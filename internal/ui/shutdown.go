package ui

import (
	"net/http"
	"time"
)

// shutdownDelay gives the response a window to flush back to the browser
// before the harness tears down its HTTP listener. Localhost is always fast
// enough; the margin only protects a slow loopback under load.
const shutdownDelay = 200 * time.Millisecond

// shutdownPage is the standalone HTML the user sees after confirming. It
// inlines its own styles so a successful render does not depend on the
// /static fileserver still being up by the time the browser fetches them.
const shutdownPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Harness - shutting down</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", sans-serif; background: #fafafa; color: #111827; padding: 3rem 1.5rem; max-width: 32rem; margin: 0 auto; }
h1 { font-size: 1.25rem; margin: 0 0 0.5rem; letter-spacing: -0.015em; }
p { margin: 0.4rem 0; color: #374151; line-height: 1.5; font-size: 0.9rem; }
p.muted { color: #6b7280; font-size: 0.8rem; margin-top: 1rem; }
</style>
</head>
<body>
<h1>Harness is shutting down</h1>
<p>Stopping the API server, draining the queue, terminating llama-server and the embedder, and flushing the database.</p>
<p class="muted">You can close this tab. Double-click the harness binary to start it again.</p>
</body>
</html>`

// handleShutdown is POST /shutdown - tears the harness down via the wired
// quit callback (typically tray.Quit). The response is written and flushed
// before the callback fires so the browser sees the "shutting down" page
// instead of a connection-reset error.
//
// sync.Once guards against a double-click firing two shutdown flows; the
// second POST gets the same page but does not re-trigger quit.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	fn := s.getQuit()
	if fn == nil {
		http.Error(w, "shutdown not configured", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write([]byte(shutdownPage)); err != nil {
		// The connection dropped mid-write. Still trigger shutdown -
		// the user clicked confirm and will not see the page anyway,
		// but the harness should still exit.
		s.triggerQuit(fn)
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	s.triggerQuit(fn)
}

// triggerQuit fires the quit callback exactly once, asynchronously so the
// caller's response can fully flush before the HTTP server tears down.
func (s *Server) triggerQuit(fn func()) {
	s.quitOnce.Do(func() {
		go func() {
			time.Sleep(shutdownDelay)
			fn()
		}()
	})
}
