package ui

import (
	"net/http"
	"time"
)

// shutdownDelay gives the response a window to flush back to the browser
// before the harness tears down its HTTP listener. Localhost is always fast
// enough; the margin only protects a slow loopback under load.
const shutdownDelay = 200 * time.Millisecond

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
	if err := s.shutdownTmpl.Execute(w, nil); err != nil {
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
