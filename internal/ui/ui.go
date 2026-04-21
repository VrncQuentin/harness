// Package ui implements the browser-based management UI server.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"
)

// ProcessStatus is the UI-facing status of a managed process.
type ProcessStatus struct {
	Name         string
	Running      bool
	Healthy      bool
	RestartCount int
	LastError    string
}

// State is the complete UI state snapshot.
type State struct {
	mu sync.RWMutex

	LlamaStatus   ProcessStatus
	EmbedderStatus ProcessStatus
	QueueDepth    int
	QueueMax      int
	StartupErrors []string
	StartTime     time.Time
}

// Server is the UI HTTP server.
type Server struct {
	port  int
	state *State

	sseClients   map[chan string]struct{}
	sseClientsMu sync.Mutex

	tmpl *template.Template
}

// NewServer creates a new UI server on the given port.
func NewServer(port int) *Server {
	s := &Server{
		port:       port,
		state:      &State{StartTime: time.Now()},
		sseClients: make(map[chan string]struct{}),
	}
	s.tmpl = template.Must(template.New("status").Parse(statusPageHTML))
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

// AddStartupError appends a startup error message.
func (s *Server) AddStartupError(msg string) {
	s.state.mu.Lock()
	s.state.StartupErrors = append(s.state.StartupErrors, msg)
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

// Start begins serving on the configured port. The server runs in a background
// goroutine and returns immediately. It never fails to start — even if config
// is missing, it serves the error state.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleStatus)
	mux.HandleFunc("/events", s.handleSSE)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Nothing we can do — log to stderr at most.
			fmt.Printf("ui: server error: %v\n", err)
		}
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
		http.Error(w, "template error", 500)
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
	s.addSSEClient(ch)
	defer s.removeSSEClient(ch)

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

	s.sseClientsMu.Lock()
	defer s.sseClientsMu.Unlock()
	for ch := range s.sseClients {
		select {
		case ch <- msg:
		default:
		}
	}
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
	LlamaHealthy    bool     `json:"llama_healthy"`
	LlamaRunning    bool     `json:"llama_running"`
	LlamaRestarts   int      `json:"llama_restarts"`
	EmbedHealthy    bool     `json:"embed_healthy"`
	EmbedRunning    bool     `json:"embed_running"`
	EmbedRestarts   int      `json:"embed_restarts"`
	QueueDepth      int      `json:"queue_depth"`
	QueueMax        int      `json:"queue_max"`
	StartupErrors   []string `json:"startup_errors,omitempty"`
	UptimeSeconds   int64    `json:"uptime_seconds"`
}

func stateToPayload(s State) ssePayload {
	return ssePayload{
		LlamaHealthy:  s.LlamaStatus.Healthy,
		LlamaRunning:  s.LlamaStatus.Running,
		LlamaRestarts: s.LlamaStatus.RestartCount,
		EmbedHealthy:  s.EmbedderStatus.Healthy,
		EmbedRunning:  s.EmbedderStatus.Running,
		EmbedRestarts: s.EmbedderStatus.RestartCount,
		QueueDepth:    s.QueueDepth,
		QueueMax:      s.QueueMax,
		StartupErrors: s.StartupErrors,
		UptimeSeconds: int64(time.Since(s.StartTime).Seconds()),
	}
}

func (s *Server) addSSEClient(ch chan string) {
	s.sseClientsMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseClientsMu.Unlock()
}

func (s *Server) removeSSEClient(ch chan string) {
	s.sseClientsMu.Lock()
	delete(s.sseClients, ch)
	s.sseClientsMu.Unlock()
}

const statusPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Harness — Status</title>
<script src="https://unpkg.com/htmx.org@1.9.12/dist/htmx.min.js"></script>
<script src="https://unpkg.com/htmx.org@1.9.12/dist/ext/sse.js"></script>
<style>
  body{font-family:system-ui,sans-serif;max-width:800px;margin:2rem auto;padding:0 1rem;background:#f5f5f5;color:#333}
  h1{font-size:1.4rem;margin-bottom:1.5rem}
  .card{background:#fff;border:1px solid #e0e0e0;border-radius:8px;padding:1rem 1.25rem;margin-bottom:1rem}
  .row{display:flex;justify-content:space-between;align-items:center;padding:.35rem 0}
  .badge{display:inline-block;padding:.2em .7em;border-radius:12px;font-size:.8rem;font-weight:600}
  .healthy{background:#d4edda;color:#155724}
  .unhealthy{background:#f8d7da;color:#721c24}
  .errors{background:#fff3cd;border:1px solid #ffc107;border-radius:8px;padding:1rem 1.25rem;margin-bottom:1rem}
  .errors h2{font-size:1rem;margin:0 0 .5rem;color:#856404}
  .errors ul{margin:0;padding-left:1.25rem}
  .errors li{margin:.2rem 0;font-size:.9rem}
</style>
</head>
<body hx-ext="sse" sse-connect="/events" sse-swap="message">

<h1>Harness Status</h1>

{{if .StartupErrors}}
<div class="errors">
  <h2>Startup Errors</h2>
  <ul>
    {{range .StartupErrors}}<li>{{.}}</li>{{end}}
  </ul>
</div>
{{end}}

<div class="card">
  <div class="row">
    <strong>llama-server</strong>
    <span class="badge {{if .LlamaStatus.Healthy}}healthy{{else}}unhealthy{{end}}" id="llama-badge">
      {{if .LlamaStatus.Healthy}}Healthy{{else}}Unhealthy{{end}}
    </span>
  </div>
  <div class="row">
    <span style="color:#666;font-size:.9rem">Restarts</span>
    <span id="llama-restarts">{{.LlamaStatus.RestartCount}}</span>
  </div>
  {{if .LlamaStatus.LastError}}
  <div class="row">
    <span style="color:#721c24;font-size:.85rem">{{.LlamaStatus.LastError}}</span>
  </div>
  {{end}}
</div>

<div class="card">
  <div class="row">
    <strong>Embedder</strong>
    <span class="badge {{if .EmbedderStatus.Healthy}}healthy{{else}}unhealthy{{end}}" id="embed-badge">
      {{if .EmbedderStatus.Healthy}}Healthy{{else}}Unhealthy{{end}}
    </span>
  </div>
  <div class="row">
    <span style="color:#666;font-size:.9rem">Restarts</span>
    <span id="embed-restarts">{{.EmbedderStatus.RestartCount}}</span>
  </div>
</div>

<div class="card">
  <div class="row">
    <strong>Queue</strong>
    <span id="queue-depth">{{.QueueDepth}} / {{.QueueMax}}</span>
  </div>
</div>

<script>
// Listen for SSE updates and patch the DOM.
document.addEventListener('htmx:sseMessage', function(evt) {
  try {
    var d = JSON.parse(evt.detail.data);
    var lb = document.getElementById('llama-badge');
    if (lb) {
      lb.textContent = d.llama_healthy ? 'Healthy' : 'Unhealthy';
      lb.className = 'badge ' + (d.llama_healthy ? 'healthy' : 'unhealthy');
    }
    var eb = document.getElementById('embed-badge');
    if (eb) {
      eb.textContent = d.embed_healthy ? 'Healthy' : 'Unhealthy';
      eb.className = 'badge ' + (d.embed_healthy ? 'healthy' : 'unhealthy');
    }
    var lr = document.getElementById('llama-restarts');
    if (lr) lr.textContent = d.llama_restarts;
    var er = document.getElementById('embed-restarts');
    if (er) er.textContent = d.embed_restarts;
    var qd = document.getElementById('queue-depth');
    if (qd) qd.textContent = d.queue_depth + ' / ' + d.queue_max;
  } catch(e) {}
});
</script>
</body>
</html>`
