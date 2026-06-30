package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/vrnc/harness/internal/agentloop"
)

// TaskRunner executes an agent loop and streams events back.
type TaskRunner interface {
	RunTask(ctx context.Context, agent string, sessionID string, conversation []ChatMessage) (string, <-chan agentloop.Event, error)
	// ApplyApproval delivers a user decision for a pending approval event.
	// sessionID identifies the task, approvalID identifies the specific
	// tool call within that task.
	ApplyApproval(sessionID, approvalID, decision string) error
}

var (
	ErrTaskNoAgent   = errors.New("no agent selected — pick one on the /agents page first")
	ErrTaskQueueFull = errors.New("queue is full — wait for in-flight requests to finish")
	ErrTaskCancelled = errors.New("task cancelled")
	ErrTaskNotReady  = errors.New("task runner not available — the harness may still be starting")
)

// SetTaskRunner installs the runner used by /task/send. Safe to leave
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

const taskSendMaxBytes = 32 * 1024

type taskSendView struct {
	UserContent string
	SessionID   string
}

func (s *Server) handleTaskSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runner := s.getTaskRunner()
	if runner == nil {
		http.Error(w, ErrTaskNotReady.Error(), http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, taskSendMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(r.FormValue("message"))
	if msg == "" {
		http.Error(w, "message must not be empty", http.StatusBadRequest)
		return
	}
	agent := strings.TrimSpace(r.FormValue("agent"))
	sessionID := strings.TrimSpace(r.FormValue("session_id"))
	conversation := []ChatMessage{{Role: "user", Content: msg}}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.taskTmpl.ExecuteTemplate(w, "task-send-fragment", taskSendView{
		UserContent: msg,
		SessionID:   sessionID,
	}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(s.asyncContext())
	go func() {
		defer cancel()
		s.streamTaskEvents(ctx, runner, agent, sessionID, conversation)
	}()
}

func (s *Server) streamTaskEvents(ctx context.Context, runner TaskRunner, agent, sessionID string, conversation []ChatMessage) {
	newID, evch, err := runner.RunTask(ctx, agent, sessionID, conversation)
	if err != nil {
		s.broadcastTaskSSE(renderTaskSSE("task-event", s.renderTaskEvent(agentloop.Event{Type: agentloop.EvtError, Content: err.Error(), Terminate: agentloop.EvtError})))
		return
	}
	if newID != "" && newID != sessionID {
		s.broadcastTaskSSE(fmt.Sprintf("event: task-session\ndata: %s\n\n", sseData(fmt.Sprintf(`<input type="hidden" id="task-session-input" name="session_id" hx-swap-oob="true" value="%s">`, newID))))
	}
	for ev := range evch {
		switch ev.Type {
		case agentloop.EvtText:
			s.broadcastTaskSSE(renderTaskSSE("task-text", s.renderTaskText(ev.Content)))
		default:
			s.broadcastTaskSSE(renderTaskSSE("task-event", s.renderTaskEvent(ev)))
		}
	}
}

func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if _, err := fmt.Fprint(w, ": task-connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ch := make(chan string, 8)
	s.taskSSEClients.Store(ch, struct{}{})
	defer s.taskSSEClients.Delete(ch)
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprint(w, msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) renderTaskEvent(ev agentloop.Event) string {
	var buf bytes.Buffer
	if err := s.taskTmpl.ExecuteTemplate(&buf, "task-event-fragment", ev); err != nil {
		return ""
	}
	return buf.String()
}

func (s *Server) renderTaskText(content string) string {
	var buf bytes.Buffer
	if err := s.taskTmpl.ExecuteTemplate(&buf, "task-text-fragment", content); err != nil {
		return ""
	}
	return buf.String()
}

func renderTaskSSE(eventName, html string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventName, sseData(html))
}

func (s *Server) broadcastTaskSSE(frame string) {
	s.taskSSEClients.Range(func(key, _ any) bool {
		ch, ok := key.(chan string)
		if !ok {
			return true
		}
		select {
		case ch <- frame:
		default:
		}
		return true
	})
}

// taskView is the template context for the /task page.
type taskView struct {
	basePage
	Error string
}

func (s *Server) handleTaskApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runner := s.getTaskRunner()
	if runner == nil {
		http.Error(w, ErrTaskNotReady.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	sessionID := strings.TrimSpace(r.FormValue("session_id"))
	approvalID := strings.TrimSpace(r.FormValue("approval_id"))
	decision := strings.TrimSpace(r.FormValue("decision"))
	if sessionID == "" || approvalID == "" || decision == "" {
		http.Error(w, "session_id, approval_id, and decision are required", http.StatusBadRequest)
		return
	}
	if decision != "allow" && decision != "reject" && decision != "always" {
		http.Error(w, "decision must be allow, reject, or always", http.StatusBadRequest)
		return
	}
	if err := runner.ApplyApproval(sessionID, approvalID, decision); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "")
}
