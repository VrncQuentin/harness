package ui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vrnc/harness/internal/logbuf"
)

func newEventStreamID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
			if !s.writeLogBatch(w, "llama-log", drainEntries(llamaCh, e)) {
				return
			}
			flusher.Flush()
		case e, ok := <-embedCh:
			if !ok {
				return
			}
			if !s.writeLogBatch(w, "embed-log", drainEntries(embedCh, e)) {
				return
			}
			flusher.Flush()
		case e, ok := <-harnessCh:
			if !ok {
				return
			}
			if !s.writeLogBatch(w, "harness-log", drainEntries(harnessCh, e)) {
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

// writeLogBatch renders each entry as one HTML `event: <name>` SSE frame.
// htmx consumes these frames directly with sse-swap="<name>".
func (s *Server) writeLogBatch(w http.ResponseWriter, eventName string, batch []logbuf.Entry) bool {
	for _, e := range batch {
		html := s.renderLogRow(logEntryView{
			Time: e.Time.Format("15:04:05"),
			Line: e.Line,
		})
		if html == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, sseData(html)); err != nil {
			return false
		}
	}
	return true
}

func sseData(html string) string {
	return strings.ReplaceAll(html, "\n", "\ndata: ")
}

// broadcastState sends OOB HTML fragments to all SSE clients so htmx can
// swap them directly without browser-side JSON parsing. Each fragment is a
// self-contained element with hx-swap-oob="true". The fragments are joined
// with newlines and run through sseData so each line carries the required
// data: prefix.
func (s *Server) broadcastState() {
	snap := s.state.snapshot()
	msg := s.stateFragments(snap)

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
	snap := s.state.snapshot()
	msg := s.stateFragments(snap)
	select {
	case ch <- msg:
	default:
	}
}

// stateFragments renders the live-updated page elements as OOB HTML fragments
// joined with newlines and formatted as multi-line SSE data.
func (s *Server) stateFragments(snap stateSnapshot) string {
	llamaHTML := injectOOB(s.renderProcStatusPanel(llamaPanelFromSnapshot(snap)), "llama-status-panel")
	embedHTML := injectOOB(s.renderProcStatusPanel(embedPanelFromSnapshot(snap)), "embed-status-panel")
	queueHTML := injectOOB(s.renderQueueCard(snap), "queue-card")
	uptimeHTML := fmt.Sprintf(`<span id="uptime" hx-swap-oob="true">%s</span>`, formatUptime(time.Since(snap.StartTime)))

	joined := strings.Join([]string{llamaHTML, embedHTML, queueHTML, uptimeHTML}, "\n")
	return sseData(joined)
}

// injectOOB adds hx-swap-oob="true" to the root element's id attribute so
// htmx processes the fragment as an out-of-band swap when it appears in
// an SSE data payload.
func injectOOB(html, id string) string {
	return strings.Replace(html, `id="`+id+`"`, `id="`+id+`" hx-swap-oob="true"`, 1)
}

func (s *Server) renderQueueCard(snap stateSnapshot) string {
	var buf bytes.Buffer
	if err := s.statusTmpl.ExecuteTemplate(&buf, "queue_card", queueCardFromSnapshot(snap)); err != nil {
		return ""
	}
	return buf.String()
}

func (s *Server) renderLogRow(row logEntryView) string {
	var buf bytes.Buffer
	if err := s.statusTmpl.ExecuteTemplate(&buf, "log_row", row); err != nil {
		return ""
	}
	return buf.String()
}

func (s *Server) renderProcStatusPanel(data procStatusPanelData) string {
	var buf bytes.Buffer
	if err := s.statusTmpl.ExecuteTemplate(&buf, "proc_status_panel", data); err != nil {
		return ""
	}
	return buf.String()
}
