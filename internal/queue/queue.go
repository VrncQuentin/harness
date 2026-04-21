// Package queue implements a bounded request queue with a WAL for crash recovery.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrnc/harness/internal/inference"
)

// Request is a single queued inference request.
type Request struct {
	ID       string
	Messages []inference.Message
	// Response is closed after the last token or on error.
	Response chan<- inference.Token
	Ctx      context.Context
}

// ErrQueueFull is returned when the queue is at max capacity.
var ErrQueueFull = fmt.Errorf("queue: at capacity, try again later")

// walRecord is a single WAL entry, JSON-encoded.
type walRecord struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"ts"`
	Done      bool      `json:"done,omitempty"`
}

// Queue is a bounded request queue with WAL backing.
type Queue struct {
	maxDepth int
	ch       chan Request
	depth    atomic.Int64

	walPath string
	walMu   sync.Mutex
	walFile *os.File

	client inference.Client
	wg     sync.WaitGroup
}

// New creates a new Queue. walPath may be empty to disable WAL.
func New(maxDepth int, walPath string, client inference.Client) *Queue {
	return &Queue{
		maxDepth: maxDepth,
		ch:       make(chan Request, maxDepth),
		walPath:  walPath,
		client:   client,
	}
}

// Start begins the worker goroutine and replays any unfinished WAL records.
// It returns when ctx is cancelled.
func (q *Queue) Start(ctx context.Context) error {
	if q.walPath != "" {
		if err := q.openWAL(); err != nil {
			return err
		}
	}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		q.worker(ctx)
	}()
	return nil
}

// Stop waits for the worker to drain and finish.
func (q *Queue) Stop() {
	q.wg.Wait()
	if q.walFile != nil {
		q.walMu.Lock()
		_ = q.walFile.Close()
		q.walMu.Unlock()
	}
}

// Enqueue adds a request to the queue. Returns ErrQueueFull if at capacity.
func (q *Queue) Enqueue(req Request) error {
	select {
	case q.ch <- req:
		q.depth.Add(1)
		q.walAppend(walRecord{ID: req.ID, Timestamp: time.Now()})
		return nil
	default:
		return ErrQueueFull
	}
}

// Depth returns the current number of items waiting in the queue.
func (q *Queue) Depth() int {
	return int(q.depth.Load())
}

// worker pulls from the channel and dispatches to the inference client.
func (q *Queue) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-q.ch:
			if !ok {
				return
			}
			q.depth.Add(-1)
			q.dispatch(req)
			q.walMarkDone(req.ID)
		}
	}
}

// dispatch sends the request to the inference client and streams tokens back.
func (q *Queue) dispatch(req Request) {
	defer close(req.Response)

	if q.client == nil {
		req.Response <- inference.Token{Err: fmt.Errorf("queue: no inference client configured")}
		return
	}

	tokenCh, err := q.client.Complete(req.Ctx, inference.CompletionRequest{
		Messages: req.Messages,
		Stream:   true,
	})
	if err != nil {
		req.Response <- inference.Token{Err: fmt.Errorf("queue: inference error: %w", err)}
		return
	}

	for tok := range tokenCh {
		select {
		case <-req.Ctx.Done():
			req.Response <- inference.Token{Err: req.Ctx.Err()}
			return
		case req.Response <- tok:
		}
		if tok.Done || tok.Err != nil {
			return
		}
	}
}

// openWAL opens or creates the WAL file.
func (q *Queue) openWAL() error {
	f, err := os.OpenFile(q.walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("queue: open WAL %s: %w", q.walPath, err)
	}
	q.walFile = f
	return nil
}

// walAppend writes a record to the WAL.
func (q *Queue) walAppend(r walRecord) {
	if q.walFile == nil {
		return
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	q.walMu.Lock()
	defer q.walMu.Unlock()
	q.walFile.Write(append(b, '\n')) //nolint:errcheck
}

// walMarkDone appends a "done" record for the given request ID.
func (q *Queue) walMarkDone(id string) {
	if q.walFile == nil {
		return
	}
	q.walAppend(walRecord{ID: id, Done: true, Timestamp: time.Now()})
}
