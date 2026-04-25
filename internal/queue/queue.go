// Package queue implements a bounded request queue with a WAL for crash recovery.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrnc/harness/internal/inference"
)

// Request is a single queued inference request.
type Request struct {
	ID          string
	Model       string
	Messages    []inference.Message
	Temperature float64
	TopP        float64
	MaxTokens   int
	// Response is closed after the last token or on error.
	Response chan<- inference.Token
	Ctx      context.Context
}

// Sentinel errors returned by the queue.
var (
	// ErrQueueFull is returned when the queue is at max capacity.
	ErrQueueFull = errors.New("queue: at capacity, try again later")
	// ErrNoClient is returned to dispatched requests when no inference
	// client is configured.
	ErrNoClient = errors.New("queue: no inference client configured")
	// ErrStopped is returned when callers try to enqueue after Stop begins.
	ErrStopped = errors.New("queue: stopped")
)

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
	stopped  atomic.Bool
	stopOnce sync.Once
	enqMu    sync.RWMutex

	walPath string
	walMu   sync.Mutex
	walFile *os.File

	clientMu sync.RWMutex
	client   inference.Client
	wg       sync.WaitGroup
}

// New creates a new Queue. walPath may be empty to disable WAL.
func New(maxDepth int, walPath string, client inference.Client) *Queue {
	return &Queue{
		maxDepth: maxDepth,
		// Channel capacity is the queue's contract: the channel itself is the
		// bounded buffer and Enqueue's non-blocking send returns ErrQueueFull
		// when full. Sized intentionally per Uber's "Channel Size" rule.
		ch:      make(chan Request, maxDepth),
		walPath: walPath,
		client:  client,
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

// Stop closes the intake channel, waits for the worker to drain accepted
// requests, then closes the WAL.
func (q *Queue) Stop() {
	q.stopOnce.Do(func() {
		q.enqMu.Lock()
		q.stopped.Store(true)
		close(q.ch)
		q.enqMu.Unlock()

		q.wg.Wait()
		if q.walFile != nil {
			q.walMu.Lock()
			_ = q.walFile.Close()
			q.walMu.Unlock()
		}
	})
}

// Enqueue adds a request to the queue. Returns ErrQueueFull if at capacity.
func (q *Queue) Enqueue(req Request) error {
	q.enqMu.RLock()
	defer q.enqMu.RUnlock()
	if q.stopped.Load() {
		return ErrStopped
	}
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

// SetClient atomically swaps the inference client. In-flight requests keep
// using the old client; new requests go to the new one. Safe to call while
// the worker is running.
func (q *Queue) SetClient(c inference.Client) {
	q.clientMu.Lock()
	q.client = c
	q.clientMu.Unlock()
}

// dispatch sends the request to the inference client and streams tokens back.
func (q *Queue) dispatch(req Request) {
	defer close(req.Response)

	q.clientMu.RLock()
	client := q.client
	q.clientMu.RUnlock()

	if client == nil {
		q.send(req.Ctx, req.Response, inference.Token{Err: ErrNoClient})
		return
	}

	tokenCh, err := client.Complete(req.Ctx, inference.CompletionRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stream:      true,
	})
	if err != nil {
		q.send(req.Ctx, req.Response, inference.Token{Err: fmt.Errorf("queue: inference: %w", err)})
		return
	}

	for tok := range tokenCh {
		select {
		case <-req.Ctx.Done():
			q.trySend(req.Response, inference.Token{Err: req.Ctx.Err()})
			return
		case req.Response <- tok:
		}
		if tok.Done || tok.Err != nil {
			return
		}
	}
}

func (q *Queue) send(ctx context.Context, resp chan<- inference.Token, tok inference.Token) {
	select {
	case resp <- tok:
	case <-ctx.Done():
	}
}

func (q *Queue) trySend(resp chan<- inference.Token, tok inference.Token) {
	select {
	case resp <- tok:
	default:
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
