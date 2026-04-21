// Package ui implements the browser-based management UI server.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sync"
	"time"
)

//go:embed templates/status.html
var templateFS embed.FS

// ProcessStatus is the UI-facing status of a managed process.
type ProcessStatus struct {
	Name         string
	Running      bool
	Healthy      bool
	RestartCount int
	LastError    error
}

// State is the complete UI state snapshot.
type State struct {
	mu sync.RWMutex

	LlamaStatus    ProcessStatus
	EmbedderStatus ProcessStatus
	QueueDepth     int
	QueueMax       int
	StartupErrors  []error
	StartTime      time.Time
}

// Server is the UI HTTP server.
type Server struct {
	port  int
	state *State

	// sseClients maps chan string → struct{} for active SSE subscribers.
	sseClients sync.Map

	tmpl *template.Template
}

// NewServer creates a new UI server on the given port.
func NewServer(port int) *Server {
	s := &Server{
		port:  port,
		state: &State{StartTime: time.Now()},
	}
	s.tmpl = template.Must(template.ParseFS(templateFS, "templates/status.html"))
	return s
}

// SetLlamaStatus updates the llama-server status.
func (s *Server) SetLlamaStatus(st ProcessStatus) {
	s.state.mu.Lock()
	s.state.LlamaStatus = st
	s.state.mu.Unlock()
	s.broadcastState()
}

// SetEmbedderStatus updates the embedder status.
func (s *Server) SetEmbedderStatus(st ProcessStatus) {
	s.state.mu.Lock()
	s.state.EmbedderStatus = st
	s.state.mu.Unlock()
	s.broadcastState()
}

// SetQueueDepth updates the queue depth.
func (s *Server) SetQueueDepth(depth, max int) {
	s.state.mu.Lock()
	s.state.QueueDepth = depth
	s.state.QueueMax = max
	s.state.mu.Unlock()
	s.broadcastState()
}

// AddStartupError appends a startup error.
func (s *Server) AddStartupError(err error) {
	s.state.mu.Lock()
	s.state.StartupErrors = append(s.state.StartupErrors, err)
	s.state.mu.Unlock()
	s.broadcastState()
}

// ClearStartupErrors clears all startup errors.
func (s *Server) ClearStartupErrors() {
	s.state.mu.Lock()
	s.state.StartupErrors = nil
	s.state.mu.Unlock()
	s.broadcastState()
}

// Start begins serving on the configured port in a background goroutine.
// It returns an error immediately if the listener cannot bind.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleStatus)
	mux.HandleFunc("/events", s.handleSSE)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	// Verify we can bind before returning.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("ui: bind :%d: %w", s.port, err)
	}

	go func() {
		srv.Serve(ln) //nolint:errcheck
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	}()

	return nil
}

// handleStatus renders the status page.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.state.mu.RLock()
	snap := *s.state
	s.state.mu.RUnlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, snap); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// handleSSE streams JSON state updates to the client via Server-Sent Events.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 8)
	s.sseClients.Store(ch, struct{}{})
	defer s.sseClients.Delete(ch)

	// Send initial state immediately.
	s.sendState(ch)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			s.sendState(ch)
		}
	}
}

// broadcastState sends the current state to all SSE clients.
func (s *Server) broadcastState() {
	s.state.mu.RLock()
	snap := *s.state
	s.state.mu.RUnlock()

	b, _ := json.Marshal(stateToPayload(snap))
	msg := string(b)

	s.sseClients.Range(func(key, _ any) bool {
		ch := key.(chan string)
		select {
		case ch <- msg:
		default:
		}
		return true
	})
}

// sendState marshals the current state and sends it to a specific client channel.
func (s *Server) sendState(ch chan string) {
	s.state.mu.RLock()
	snap := *s.state
	s.state.mu.RUnlock()

	b, _ := json.Marshal(stateToPayload(snap))
	select {
	case ch <- string(b):
	default:
	}
}

type ssePayload struct {
	LlamaHealthy  bool     `json:"llama_healthy"`
	LlamaRunning  bool     `json:"llama_running"`
	LlamaRestarts int      `json:"llama_restarts"`
	EmbedHealthy  bool     `json:"embed_healthy"`
	EmbedRunning  bool     `json:"embed_running"`
	EmbedRestarts int      `json:"embed_restarts"`
	QueueDepth    int      `json:"queue_depth"`
	QueueMax      int      `json:"queue_max"`
	StartupErrors []string `json:"startup_errors,omitempty"`
	UptimeSeconds int64    `json:"uptime_seconds"`
}

func stateToPayload(s State) ssePayload {
	var errs []string
	for _, e := range s.StartupErrors {
		errs = append(errs, e.Error())
	}
	return ssePayload{
		LlamaHealthy:  s.LlamaStatus.Healthy,
		LlamaRunning:  s.LlamaStatus.Running,
		LlamaRestarts: s.LlamaStatus.RestartCount,
		EmbedHealthy:  s.EmbedderStatus.Healthy,
		EmbedRunning:  s.EmbedderStatus.Running,
		EmbedRestarts: s.EmbedderStatus.RestartCount,
		QueueDepth:    s.QueueDepth,
		QueueMax:      s.QueueMax,
		StartupErrors: errs,
		UptimeSeconds: int64(time.Since(s.StartTime).Seconds()),
	}
}
