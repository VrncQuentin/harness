package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vrnc/harness/internal/logbuf"
)

// streamRing returns an SSE handler that streams new entries from the ring
// returned by get. get is called per-request so callers can install the ring
// lazily via a setter and have streams pick it up on the next connection.
// The initial snapshot is rendered server-side in the status page; this
// stream only carries entries written after the connection was opened.
func (s *Server) streamRing(get func() *logbuf.Ring) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ring := get()
		if ring == nil {
			http.Error(w, "log ring not configured", http.StatusServiceUnavailable)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Subscribe before the first flush so no entry written between "headers
		// out" and "loop entered" is missed.
		//
		// Buffer 10k entries to absorb verbose-mode bursts: llama.cpp emits
		// dozens of lines per Write syscall during model load and SSE delivery
		// (JSON encode + Fprintf + Flush per line) is much slower than the
		// ring's append, so the channel has to bridge the rate mismatch. The
		// ring drops on overflow rather than blocking the writer; with 10k
		// slots a stalled subscriber loses lines instead of stalling
		// llama-server's stdout pipe. Memory cost is ~400 KB per subscriber.
		ch := make(chan logbuf.Entry, 10000)
		cancel := ring.Subscribe(ch)
		defer cancel()

		// Flush an SSE comment immediately so headers go out the door and the
		// browser fires onopen. Without this the connection sits header-less
		// until the first log line, which for a quiet harness can be many
		// minutes and leaves the panel looking stuck.
		if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
			return
		}
		flusher.Flush()

		// Heartbeat so idle connections stay warm and a dropped client is
		// noticed promptly (the Write fails and we exit).
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		// Per-iteration batch cap. Each entry is one Fprintf; flushing once
		// per batch is the whole point of this loop, but a hard cap keeps the
		// inner drain bounded so ctx cancellation and the heartbeat ticker
		// stay responsive when the producer is on a sustained burst.
		const maxBatch = 256

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case e, ok := <-ch:
				if !ok {
					return
				}
				// Drain anything else already buffered so a single Flush
				// covers the batch. One Flush per line was strictly slower
				// than the ring's append, which let the channel fill on
				// verbose-mode bursts and triggered drop-on-full in the
				// fanout. Batching lets the consumer keep pace with the
				// producer at the cost of a tiny per-batch latency.
				batch := []logbuf.Entry{e}
			drain:
				for len(batch) < maxBatch {
					select {
					case e2, ok := <-ch:
						if !ok {
							break drain
						}
						batch = append(batch, e2)
					default:
						break drain
					}
				}
				if writeBatch(w, batch) != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// writeBatch encodes each entry as one SSE event into w. A marshal failure
// drops the offending entry rather than the whole batch; a write failure
// aborts so the caller can return and let the SSE loop tear down. Extracted
// so the streaming loop stays scannable.
func writeBatch(w io.Writer, batch []logbuf.Entry) error {
	for _, e := range batch {
		payload, err := json.Marshal(struct {
			Time string `json:"time"`
			Line string `json:"line"`
		}{
			Time: e.Time.Format("15:04:05"),
			Line: e.Line,
		})
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return err
		}
	}
	return nil
}
