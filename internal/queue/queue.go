// Package queue implements a bounded request queue with a WAL for crash recovery.
package queue

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	replayed bool
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

// walPayload is the durable portion of Request. Response channels and contexts
// are process-local, so replay recreates those around this payload.
type walPayload struct {
	ID          string              `json:"id"`
	Model       string              `json:"model,omitempty"`
	Messages    []inference.Message `json:"messages"`
	Temperature float64             `json:"temperature,omitempty"`
	TopP        float64             `json:"top_p,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Tools       []inference.Tool    `json:"tools,omitempty"`
	ToolChoice  any                 `json:"tool_choice,omitempty"`
}

// walRecord is a single WAL entry, JSON-encoded.
type walRecord struct {
	ID        string      `json:"id"`
	Timestamp time.Time   `json:"ts"`
	Request   *walPayload `json:"request,omitempty"`
	Done      bool        `json:"done,omitempty"`
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
		// bounded buffer, and enqueue paths return ErrQueueFull when it is full.
		// Sized intentionally per Uber's "Channel Size" rule.
		ch:      make(chan Request, maxDepth),
		walPath: walPath,
		client:  client,
	}
}

// Start begins the worker goroutine and replays any unfinished WAL records.
func (q *Queue) Start(ctx context.Context) error {
	var pending []walPayload
	if q.walPath != "" {
		recovered, err := q.recoverWAL()
		if err != nil {
			return err
		}
		pending = recovered
		if err := q.openWAL(); err != nil {
			return err
		}
	}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		q.worker(ctx)
	}()
	if len(pending) > 0 {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			q.replayPending(ctx, pending)
		}()
	}
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
			q.closeWAL()
			q.clearWALIfDrained()
		}
	})
}

// Enqueue adds a request to the queue. Returns ErrQueueFull if at capacity.
func (q *Queue) Enqueue(req Request) error {
	q.enqMu.Lock()
	defer q.enqMu.Unlock()
	if q.stopped.Load() {
		return ErrStopped
	}
	if len(q.ch) >= cap(q.ch) {
		return ErrQueueFull
	}
	if err := q.walAppend(walRecord{ID: req.ID, Timestamp: time.Now(), Request: req.walPayload()}); err != nil {
		return err
	}
	q.ch <- req
	q.depth.Add(1)
	return nil
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
			if q.dispatch(req) {
				q.walMarkDone(req.ID)
				continue
			}
			if req.replayed && ctx.Err() == nil {
				q.retryReplay(ctx, req.walPayload())
			}
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
// It returns true when the request reached a terminal state and may be marked
// done in the WAL. Replayed requests keep their WAL entry on backend errors so
// a transient startup race does not lose recovered work.
func (q *Queue) dispatch(req Request) bool {
	defer close(req.Response)

	q.clientMu.RLock()
	client := q.client
	q.clientMu.RUnlock()

	if client == nil {
		q.send(req.Ctx, req.Response, inference.Token{Err: ErrNoClient})
		return !req.replayed
	}

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
		return !req.replayed
	}

	for tok := range tokenCh {
		select {
		case <-req.Ctx.Done():
			q.trySend(req.Response, inference.Token{Err: req.Ctx.Err()})
			return !req.replayed
		case req.Response <- tok:
		}
		if tok.Done || tok.Err != nil {
			return tok.Done || !req.replayed
		}
	}
	return !req.replayed
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

func (r Request) walPayload() *walPayload {
	return &walPayload{
		ID:          r.ID,
		Model:       r.Model,
		Messages:    append([]inference.Message(nil), r.Messages...),
		Temperature: r.Temperature,
		TopP:        r.TopP,
		MaxTokens:   r.MaxTokens,
		Tools:       append([]inference.Tool(nil), r.Tools...),
		ToolChoice:  r.ToolChoice,
	}
}

func (p walPayload) request(ctx context.Context) Request {
	resp := make(chan inference.Token, 64)
	go func() {
		for range resp {
		}
	}()
	return Request{
		ID:          p.ID,
		Model:       p.Model,
		Messages:    append([]inference.Message(nil), p.Messages...),
		Temperature: p.Temperature,
		TopP:        p.TopP,
		MaxTokens:   p.MaxTokens,
		Tools:       append([]inference.Tool(nil), p.Tools...),
		ToolChoice:  p.ToolChoice,
		Response:    resp,
		Ctx:         ctx,
		replayed:    true,
	}
}

func (q *Queue) replayPending(ctx context.Context, pending []walPayload) {
	for _, payload := range pending {
		if err := q.enqueueReplay(ctx, payload.request(ctx)); err != nil {
			return
		}
	}
}

func (q *Queue) retryReplay(ctx context.Context, payload *walPayload) {
	if payload == nil {
		return
	}
	go func() {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = q.enqueueReplay(ctx, payload.request(ctx))
		}
	}()
}

func (q *Queue) enqueueReplay(ctx context.Context, req Request) error {
	queued := false
	defer func() {
		if !queued {
			close(req.Response)
		}
	}()
	for {
		if q.stopped.Load() {
			return ErrStopped
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		q.enqMu.RLock()
		if q.stopped.Load() {
			q.enqMu.RUnlock()
			return ErrStopped
		}
		select {
		case q.ch <- req:
			queued = true
			q.depth.Add(1)
			q.enqMu.RUnlock()
			return nil
		default:
			q.enqMu.RUnlock()
		}

		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// openWAL opens or creates the WAL file.
func (q *Queue) openWAL() error {
	if dir := filepath.Dir(q.walPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("queue: create WAL dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(q.walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("queue: open WAL %s: %w", q.walPath, err)
	}
	q.walFile = f
	return nil
}

func (q *Queue) recoverWAL() ([]walPayload, error) {
	f, err := os.Open(q.walPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("queue: read WAL %s: %w", q.walPath, err)
	}
	defer func() { _ = f.Close() }()

	pending := make(map[string]walPayload)
	order := make([]string, 0)
	seen := make(map[string]struct{})

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec walRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("queue: parse WAL %s: %w", q.walPath, err)
		}
		if rec.Done {
			delete(pending, rec.ID)
			continue
		}
		if rec.Request == nil {
			// Pre-durable WAL records only carried IDs; they cannot be
			// reconstructed safely, so leave them behind instead of inventing
			// a partial request.
			continue
		}
		payload := *rec.Request
		if payload.ID == "" {
			payload.ID = rec.ID
		}
		pending[payload.ID] = payload
		if _, ok := seen[payload.ID]; !ok {
			seen[payload.ID] = struct{}{}
			order = append(order, payload.ID)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("queue: scan WAL %s: %w", q.walPath, err)
	}

	out := make([]walPayload, 0, len(pending))
	for _, id := range order {
		payload, ok := pending[id]
		if !ok {
			continue
		}
		out = append(out, payload)
	}
	return out, nil
}

// walAppend writes a record to the WAL.
func (q *Queue) walAppend(r walRecord) error {
	if q.walFile == nil {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("queue: marshal WAL record: %w", err)
	}
	q.walMu.Lock()
	defer q.walMu.Unlock()
	if _, err := q.walFile.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("queue: write WAL: %w", err)
	}
	if err := q.walFile.Sync(); err != nil {
		return fmt.Errorf("queue: sync WAL: %w", err)
	}
	return nil
}

// walMarkDone appends a "done" record for the given request ID.
func (q *Queue) walMarkDone(id string) {
	if q.walFile == nil {
		return
	}
	_ = q.walAppend(walRecord{ID: id, Done: true, Timestamp: time.Now()})
}

func (q *Queue) closeWAL() {
	q.walMu.Lock()
	defer q.walMu.Unlock()
	if q.walFile == nil {
		return
	}
	_ = q.walFile.Sync()
	_ = q.walFile.Close()
	q.walFile = nil
}

func (q *Queue) clearWALIfDrained() {
	if q.walPath == "" {
		return
	}
	pending, err := q.recoverWAL()
	if err != nil || len(pending) > 0 {
		return
	}
	f, err := os.OpenFile(q.walPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}
