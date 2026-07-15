// Package queue implements a bounded in-process request queue.
package queue

import (
	"context"
	"errors"
	"fmt"
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
	Tools       []inference.Tool
	ToolChoice  any
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

// MetricsRecorder is the narrow metrics surface used by Queue.
type MetricsRecorder interface {
	TimeToFirstTokenMS(time.Duration) error
	TokenThroughput(float64) error
}

// Queue is a bounded in-process request queue.
type Queue struct {
	maxDepth int
	ch       chan Request
	depth    atomic.Int64
	stopped  atomic.Bool
	stopOnce sync.Once
	enqMu    sync.RWMutex

	clientMu sync.RWMutex
	client   inference.Client

	metricsMu sync.RWMutex
	metrics   MetricsRecorder
	wg        sync.WaitGroup
}

// New creates a new Queue. Interactive queued requests are not crash-replayed.
func New(maxDepth int, client inference.Client) *Queue {
	return &Queue{
		maxDepth: maxDepth,
		// Channel capacity is the queue's contract: the channel itself is the
		// bounded buffer, and enqueue paths return ErrQueueFull when it is full.
		// Sized intentionally per Uber's "Channel Size" rule.
		ch:     make(chan Request, maxDepth),
		client: client,
	}
}

// Start begins the worker goroutine.
func (q *Queue) Start(ctx context.Context) error {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		q.worker(ctx)
	}()
	return nil
}

// Stop closes the intake channel and waits for the worker to drain accepted
// requests.
func (q *Queue) Stop() {
	q.stopOnce.Do(func() {
		q.enqMu.Lock()
		q.stopped.Store(true)
		close(q.ch)
		q.enqMu.Unlock()

		q.wg.Wait()
	})
}

// Restart drains the current worker, recreates the intake channel, and starts a
// fresh worker. It is intended for runtime reconfiguration paths such as model
// reloads; ordinary shutdown should call Stop.
func (q *Queue) Restart(ctx context.Context) error {
	q.Stop()

	q.enqMu.Lock()
	q.ch = make(chan Request, q.maxDepth)
	q.depth.Store(0)
	q.stopped.Store(false)
	q.stopOnce = sync.Once{}
	q.enqMu.Unlock()

	return q.Start(ctx)
}

// Enqueue adds a request to the queue. Returns ErrQueueFull if at capacity.
func (q *Queue) Enqueue(req Request) error {
	q.enqMu.RLock()
	defer q.enqMu.RUnlock()
	if q.stopped.Load() {
		return ErrStopped
	}
	if !q.reserveDepthSlot() {
		return ErrQueueFull
	}
	reserved := true
	defer func() {
		if reserved {
			q.depth.Add(-1)
		}
	}()

	q.ch <- req
	reserved = false
	return nil
}

func (q *Queue) reserveDepthSlot() bool {
	for {
		depth := q.depth.Load()
		if depth >= int64(q.maxDepth) {
			return false
		}
		if q.depth.CompareAndSwap(depth, depth+1) {
			return true
		}
	}
}

// Depth returns the current number of items waiting in the queue.
func (q *Queue) Depth() int {
	return int(q.depth.Load())
}

// MaxDepth returns the configured queue capacity.
func (q *Queue) MaxDepth() int {
	return q.maxDepth
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

// SetMetrics installs the recorder used for request latency and throughput samples.
func (q *Queue) SetMetrics(rec MetricsRecorder) {
	q.metricsMu.Lock()
	q.metrics = rec
	q.metricsMu.Unlock()
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

	started := time.Now()
	tokenCh, err := client.Complete(req.Ctx, inference.CompletionRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stream:      true,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
	})
	if err != nil {
		q.send(req.Ctx, req.Response, inference.Token{Err: fmt.Errorf("queue: inference: %w", err)})
		return
	}

	var firstTokenAt time.Time
	var lastTokenAt time.Time
	textTokenEvents := 0
	for tok := range tokenCh {
		if tok.Content != "" || tok.ToolCallDelta != nil {
			now := time.Now()
			if firstTokenAt.IsZero() {
				firstTokenAt = now
				q.recordTTFT(firstTokenAt.Sub(started))
			}
			lastTokenAt = now
		}
		if tok.Content != "" {
			textTokenEvents++
		}

		select {
		case <-req.Ctx.Done():
			q.trySend(req.Response, inference.Token{Err: req.Ctx.Err()})
			return
		case req.Response <- tok:
		}
		if tok.Done || tok.Err != nil {
			q.recordThroughput(firstTokenAt, lastTokenAt, textTokenEvents)
			return
		}
	}
	q.recordThroughput(firstTokenAt, lastTokenAt, textTokenEvents)
}

func (q *Queue) recordTTFT(d time.Duration) {
	q.metricsMu.RLock()
	rec := q.metrics
	q.metricsMu.RUnlock()
	if rec != nil {
		_ = rec.TimeToFirstTokenMS(d)
	}
}

func (q *Queue) recordThroughput(firstTokenAt, lastTokenAt time.Time, tokenEvents int) {
	if firstTokenAt.IsZero() || lastTokenAt.IsZero() || tokenEvents < 2 {
		return
	}
	elapsed := lastTokenAt.Sub(firstTokenAt).Seconds()
	if elapsed <= 0 {
		return
	}
	q.metricsMu.RLock()
	rec := q.metrics
	q.metricsMu.RUnlock()
	if rec != nil {
		_ = rec.TokenThroughput(float64(tokenEvents-1) / elapsed)
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
