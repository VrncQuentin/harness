package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/vrnc/harness/internal/logbuf"
)

// ssePayload is the JSON shape of an `event: state` frame.
type ssePayload struct {
	LlamaHealthy             bool                      `json:"llama_healthy"`
	LlamaRunning             bool                      `json:"llama_running"`
	LlamaRestarts            int                       `json:"llama_restarts"`
	LlamaFailed              bool                      `json:"llama_failed"`
	EmbedHealthy             bool                      `json:"embed_healthy"`
	EmbedRunning             bool                      `json:"embed_running"`
	EmbedRestarts            int                       `json:"embed_restarts"`
	EmbedFailed              bool                      `json:"embed_failed"`
	QueueDepth               int                       `json:"queue_depth"`
	QueueMax                 int                       `json:"queue_max"`
	StartupErrors            []string                  `json:"startup_errors,omitempty"`
	ProjectSlug              string                    `json:"project_slug,omitempty"`
	ProjectDirectoryWarnings []ProjectDirectoryWarning `json:"project_directory_warnings,omitempty"`
	ModelMismatch            bool                      `json:"model_mismatch,omitempty"`
	LoadedModel              string                    `json:"loaded_model,omitempty"`
	PreferredModel           string                    `json:"preferred_model,omitempty"`
	FirstRun                 bool                      `json:"first_run"`
	UptimeSeconds            int64                     `json:"uptime_seconds"`
	UptimeText               string                    `json:"uptime_text"`
}

// logEventEntry is the JSON shape of an `event: *-log` frame.
type logEventEntry struct {
	Time string `json:"time"`
	Line string `json:"line"`
}

// logSubBuffer is the per-subscription channel buffer for a log ring.
//
// Sized to absorb verbose-mode bursts: llama.cpp emits dozens of lines per
// Write during model load, and SSE delivery (JSON encode + Fprintf + Flush) is
// much slower than a ring append, so the channel has to bridge the rate gap.
// The ring drops on overflow rather than blocking the writer; with this many
// slots a stalled client loses lines instead of stalling the producer.
const logSubBuffer = 10000

// logBatchMax caps how many entries we drain per select firing. One Flush per
// batch is the win; the cap keeps ctx cancellation and the heartbeat ticker
// responsive when a producer is on a sustained burst.
const logBatchMax = 256

// handleSSE is the single multiplexed SSE endpoint. It carries state frames
// (`event: state`), log frames (`event: llama-log` / `embed-log` /
// `harness-log`), and `: ping` heartbeats over one connection.
//
// Multiplexing matters because the UI is plain HTTP/1.1 (no TLS), so browsers
// cap concurrent connections per origin at ~6. Four separate streams (state +
// three log channels) burned four of those slots and made every navigation,
// stylesheet fetch, or second-tab page load race the browser's 30 s
// connection-stall timeout. One stream frees three slots permanently.
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

	// Flush a connected comment so headers go out and EventSource fires
	// onopen before the first real frame. Without this the browser stays in
	// CONNECTING until the first state frame, which is fine but slower than
	// it needs to be on first paint.
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	// Per-client state channel: senders broadcast non-blocking and drop on
	// full, so a slow client can only miss frames - it never blocks others.
	// Buffer 1 is enough; a missed payload is replaced within the 2 s tick.
	stateCh := make(chan string, 1)
	s.sseClients.Store(stateCh, struct{}{})
	defer s.sseClients.Delete(stateCh)
	s.sendState(stateCh)

	// Subscribe to whichever log rings are wired. nil channels block in
	// select forever, so missing rings simply contribute no frames - no
	// guard branches needed in the loop.
	llamaCh, llamaCancel := subscribeRing(s.getLlamaRing())
	defer llamaCancel()
	embedCh, embedCancel := subscribeRing(s.getEmbedRing())
	defer embedCancel()
	harnessCh, harnessCancel := subscribeRing(s.getLogRing())
	defer harnessCancel()

	stateTick := time.NewTicker(2 * time.Second)
	defer stateTick.Stop()
	pingTick := time.NewTicker(15 * time.Second)
	defer pingTick.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-stateCh:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()
		case <-stateTick.C:
			s.sendState(stateCh)
		case <-pingTick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-llamaCh:
			if !ok {
				return
			}
			if !writeLogBatch(w, "llama-log", drainEntries(llamaCh, e)) {
				return
			}
			flusher.Flush()
		case e, ok := <-embedCh:
			if !ok {
				return
			}
			if !writeLogBatch(w, "embed-log", drainEntries(embedCh, e)) {
				return
			}
			flusher.Flush()
		case e, ok := <-harnessCh:
			if !ok {
				return
			}
			if !writeLogBatch(w, "harness-log", drainEntries(harnessCh, e)) {
				return
			}
			flusher.Flush()
		}
	}
}

// subscribeRing starts a buffered subscription to ring. Returns a nil channel
// and a no-op cancel when ring is nil so callers can wire `defer cancel()` and
// rely on a nil channel never firing in their select.
func subscribeRing(ring *logbuf.Ring) (chan logbuf.Entry, func()) {
	if ring == nil {
		return nil, func() {}
	}
	ch := make(chan logbuf.Entry, logSubBuffer)
	cancel := ring.Subscribe(ch)
	return ch, cancel
}

// drainEntries returns first plus any entries already buffered on ch, up to
// logBatchMax total. Stops on a non-blocking miss or a closed channel so we
// can write one batch per Flush.
func drainEntries(ch chan logbuf.Entry, first logbuf.Entry) []logbuf.Entry {
	batch := make([]logbuf.Entry, 1, logBatchMax)
	batch[0] = first
	for len(batch) < logBatchMax {
		select {
		case e, ok := <-ch:
			if !ok {
				return batch
			}
			batch = append(batch, e)
		default:
			return batch
		}
	}
	return batch
}

// writeLogBatch encodes each entry as one `event: <name>` SSE frame. Returns
// false on the first write error so the caller can tear down. A marshal
// failure drops the offending entry rather than the whole batch.
func writeLogBatch(w http.ResponseWriter, eventName string, batch []logbuf.Entry) bool {
	for _, e := range batch {
		payload, err := json.Marshal(logEventEntry{
			Time: e.Time.Format("15:04:05"),
			Line: e.Line,
		})
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, payload); err != nil {
			return false
		}
	}
	return true
}

// broadcastState sends the current state to all SSE clients. Non-blocking; a
// client whose buffer is full drops this frame and picks up the next tick.
func (s *Server) broadcastState() {
	b, _ := json.Marshal(stateToPayload(s.state.snapshot()))
	msg := string(b)

	s.sseClients.Range(func(key, _ any) bool {
		ch, ok := key.(chan string)
		if !ok {
			return true
		}
		select {
		case ch <- msg:
		default:
		}
		return true
	})
}

// sendState marshals the current state and sends it to a specific client channel.
func (s *Server) sendState(ch chan string) {
	b, _ := json.Marshal(stateToPayload(s.state.snapshot()))
	select {
	case ch <- string(b):
	default:
	}
}

func stateToPayload(s stateSnapshot) ssePayload {
	errs := make([]string, 0, len(s.StartupErrors))
	for _, e := range s.StartupErrors {
		errs = append(errs, e.Error())
	}
	uptime := time.Since(s.StartTime)
	return ssePayload{
		LlamaHealthy:             s.LlamaStatus.Healthy,
		LlamaRunning:             s.LlamaStatus.Running,
		LlamaRestarts:            s.LlamaStatus.RestartCount,
		LlamaFailed:              s.LlamaStatus.Failed,
		EmbedHealthy:             s.EmbedderStatus.Healthy,
		EmbedRunning:             s.EmbedderStatus.Running,
		EmbedRestarts:            s.EmbedderStatus.RestartCount,
		EmbedFailed:              s.EmbedderStatus.Failed,
		QueueDepth:               s.QueueDepth,
		QueueMax:                 s.QueueMax,
		StartupErrors:            errs,
		ProjectSlug:              s.ProjectSlug,
		ProjectDirectoryWarnings: s.ProjectDirectoryWarnings,
		ModelMismatch:            s.ModelMismatch,
		LoadedModel:              s.LoadedModel,
		PreferredModel:           s.PreferredModel,
		FirstRun:                 s.FirstRun,
		UptimeSeconds:            int64(uptime.Seconds()),
		UptimeText:               formatUptime(uptime),
	}
}
