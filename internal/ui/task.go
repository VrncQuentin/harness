package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/vrnc/harness/internal/agentloop"
)

// TaskRunner executes an agent loop and streams events back.
type TaskRunner interface {
	RunTask(ctx context.Context, agent string, sessionID string, conversation []ChatMessage) (string, <-chan agentloop.Event, error)
}

var (
	ErrTaskNoAgent     = errors.New("no agent selected — pick one on the /agents page first")
	ErrTaskQueueFull   = errors.New("queue is full — wait for in-flight requests to finish")
	ErrTaskCancelled   = errors.New("task cancelled")
	ErrTaskNotReady    = errors.New("task runner not available — the harness may still be starting")
)

// SetTaskRunner installs the runner used by /task/stream. Safe to leave
// unset; the page then renders a "not ready" state.
func (s *Server) SetTaskRunner(r TaskRunner) {
	s.taskRunnerMu.Lock()
	s.taskRunner = r
	s.taskRunnerMu.Unlock()
}

func (s *Server) getTaskRunner() TaskRunner {
	s.taskRunnerMu.RLock()
	defer s.taskRunnerMu.RUnlock()
	return s.taskRunner
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.renderTask(w, taskView{basePage: s.newBasePage("task")})
}

func (s *Server) renderTask(w http.ResponseWriter, data taskView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.taskTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runner := s.getTaskRunner()
	if runner == nil {
		writeTaskError(w, ErrTaskNotReady.Error())
		return
	}
	var req struct {
		Agent     string        `json:"agent"`
		SessionID string        `json:"session_id"`
		Messages  []ChatMessage `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTaskError(w, "could not parse request")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID, evch, err := runner.RunTask(ctx, req.Agent, req.SessionID, req.Messages)
	if err != nil {
		writeTaskError(w, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	// Emit session id first.
	writeTaskSSE(w, flusher, map[string]any{"event": "session", "data": map[string]string{"session_id": sessionID}})

	for ev := range evch {
		select {
		case <-ctx.Done():
			return
		default:
		}
		writeTaskSSE(w, flusher, map[string]any{
			"event": ev.Type,
			"data":  ev,
		})
		if ev.Terminate != "" {
			return
		}
	}
}

func writeTaskSSE(w http.ResponseWriter, flusher http.Flusher, payload map[string]any) {
	data, _ := json.Marshal(payload)
	w.Write([]byte(string(data) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func writeTaskError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	data, _ := json.Marshal(map[string]any{"data": map[string]string{"error": msg}})
	w.Write([]byte(string(data) + "\n\n"))
}

// taskView is the template context for the /task page.
type taskView struct {
	basePage
	Error string
}
