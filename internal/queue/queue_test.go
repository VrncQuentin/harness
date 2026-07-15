package queue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vrnc/harness/internal/inference"
)

// fakeClient returns a fixed set of tokens.
type fakeClient struct {
	tokens []string
}

func (f *fakeClient) Complete(_ context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	ch := make(chan inference.Token, len(f.tokens)+1)
	for _, t := range f.tokens {
		ch <- inference.Token{Content: t}
	}
	ch <- inference.Token{Done: true}
	close(ch)
	return ch, nil
}

func (f *fakeClient) Health(_ context.Context) error { return nil }

func TestEnqueue_And_Dispatch(t *testing.T) {
	client := &fakeClient{tokens: []string{"hello", " world"}}
	q := New(8, "", client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}

	resp := make(chan inference.Token, 16)
	err := q.Enqueue(Request{
		ID:       "test-1",
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
		Response: resp,
		Ctx:      context.Background(),
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	var got string
	for tok := range resp {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
		got += tok.Content
	}
	if got != "hello world" {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestEnqueue_DispatchPreservesTools(t *testing.T) {
	client := &captureClient{seen: make(chan inference.CompletionRequest, 1)}
	q := New(8, "", client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop()

	resp := make(chan inference.Token, 4)
	if err := q.Enqueue(Request{
		ID:       "tool-test",
		Messages: []inference.Message{{Role: "user", Content: "read"}},
		Tools: []inference.Tool{{
			Type: "function",
			Function: inference.ToolDefinition{
				Name:        "file_read",
				Description: "Read a file",
				Parameters:  map[string]any{"type": "object"},
			},
		}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	for range resp {
	}

	select {
	case got := <-client.seen:
		if len(got.Tools) != 1 || got.Tools[0].Function.Name != "file_read" {
			t.Fatalf("tools not preserved: %+v", got.Tools)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for dispatched request")
	}
}

func TestEnqueue_Full(t *testing.T) {
	// Use nil client - worker will stall so queue fills up.
	q := New(2, "", nil)
	// Do NOT start the worker - keeps requests in channel.

	resp1 := make(chan inference.Token, 4)
	resp2 := make(chan inference.Token, 4)
	resp3 := make(chan inference.Token, 4)

	_ = q.Enqueue(Request{ID: "1", Ctx: context.Background(), Response: resp1})
	_ = q.Enqueue(Request{ID: "2", Ctx: context.Background(), Response: resp2})

	err := q.Enqueue(Request{ID: "3", Ctx: context.Background(), Response: resp3})
	if err != ErrQueueFull {
		t.Errorf("expected ErrQueueFull, got %v", err)
	}
}

func TestEnqueue_WALAppendFailureReleasesDepthReservation(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "queue.wal")
	q := New(1, walPath, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop()
	if err := q.walFile.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}

	err := q.Enqueue(Request{ID: "write-fails", Ctx: context.Background(), Response: make(chan inference.Token, 1)})
	if err == nil {
		t.Fatal("expected WAL append error")
	}
	if q.Depth() != 0 {
		t.Fatalf("depth after failed WAL append = %d, want 0", q.Depth())
	}
}

func TestDepth(t *testing.T) {
	q := New(8, "", nil)
	if q.Depth() != 0 {
		t.Errorf("expected depth 0, got %d", q.Depth())
	}
}

func TestSetClient_Swaps(t *testing.T) {
	first := &fakeClient{tokens: []string{"old"}}
	q := New(8, "", first)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}

	q.SetClient(&fakeClient{tokens: []string{"new"}})

	resp := make(chan inference.Token, 8)
	if err := q.Enqueue(Request{
		ID:       "swap-test",
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var got string
	for tok := range resp {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
		got += tok.Content
	}
	if got != "new" {
		t.Errorf("expected swapped client output %q, got %q", "new", got)
	}
}

func TestStop_DrainsAcceptedRequests(t *testing.T) {
	client := &fakeClient{tokens: []string{"done"}}
	q := New(4, "", client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}

	resp := make(chan inference.Token, 4)
	if err := q.Enqueue(Request{
		ID:       "drain-test",
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	done := make(chan struct{})
	go func() {
		q.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not drain accepted request")
	}

	var got string
	for tok := range resp {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
		got += tok.Content
	}
	if got != "done" {
		t.Fatalf("drained response = %q, want done", got)
	}
	if err := q.Enqueue(Request{ID: "after-stop", Response: make(chan inference.Token, 1), Ctx: context.Background()}); !errors.Is(err, ErrStopped) {
		t.Fatalf("enqueue after Stop = %v, want ErrStopped", err)
	}
}

func TestRestart_AllowsRequestsAfterStop(t *testing.T) {
	client := &fakeClient{tokens: []string{"restarted"}}
	q := New(4, "", client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}

	q.Stop()
	if err := q.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	defer q.Stop()

	resp := make(chan inference.Token, 4)
	if err := q.Enqueue(Request{
		ID:       "after-restart",
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue after Restart: %v", err)
	}

	var got string
	for tok := range resp {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
		got += tok.Content
	}
	if got != "restarted" {
		t.Fatalf("response after Restart = %q, want restarted", got)
	}
}
func TestDispatch_CancelledFullResponseDoesNotWedgeWorker(t *testing.T) {
	stream := make(chan inference.Token, 2)
	client := &streamClient{tokens: stream, started: make(chan struct{})}
	q := New(2, "", client)

	ctx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	fullResp := make(chan inference.Token, 1)
	if err := q.Enqueue(Request{
		ID:       "cancel-test",
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
		Response: fullResp,
		Ctx:      reqCtx,
	}); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}

	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("client was not called")
	}
	stream <- inference.Token{Content: "one"}
	deadline := time.After(time.Second)
	for len(fullResp) == 0 {
		select {
		case <-deadline:
			t.Fatal("first token did not fill response buffer")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	stream <- inference.Token{Content: "two"}
	time.Sleep(50 * time.Millisecond)
	cancelReq()

	q.SetClient(&fakeClient{tokens: []string{"next"}})
	resp := make(chan inference.Token, 4)
	if err := q.Enqueue(Request{
		ID:       "next-test",
		Messages: []inference.Message{{Role: "user", Content: "hi again"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}

	select {
	case tok, ok := <-resp:
		if !ok {
			t.Fatal("second response closed before token")
		}
		if tok.Err != nil {
			t.Fatalf("second token error: %v", tok.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue worker wedged after cancelled full response")
	}
}

type streamClient struct {
	tokens  chan inference.Token
	started chan struct{}
	once    sync.Once
}

func (s *streamClient) Complete(_ context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	s.once.Do(func() { close(s.started) })
	return s.tokens, nil
}

func (s *streamClient) Health(_ context.Context) error { return nil }

func TestEnqueue_ClientError(t *testing.T) {
	errClient := &errInferenceClient{}
	q := New(8, "", errClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}

	resp := make(chan inference.Token, 4)
	_ = q.Enqueue(Request{
		ID:       "err-test",
		Messages: nil,
		Response: resp,
		Ctx:      context.Background(),
	})

	tok := <-resp
	if tok.Err == nil {
		t.Error("expected error token from failed client")
	}
}

type errInferenceClient struct{}

func (e *errInferenceClient) Complete(_ context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	return nil, errors.New("test error")
}

func (e *errInferenceClient) Health(_ context.Context) error { return nil }

func TestWAL_ReplaysUnfinishedRequest(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "runtime", "queue.wal")
	q1 := New(4, walPath, nil)
	if err := q1.openWAL(); err != nil {
		t.Fatalf("openWAL: %v", err)
	}
	resp := make(chan inference.Token, 1)
	if err := q1.Enqueue(Request{
		ID:          "recover-me",
		Model:       "qwen",
		Messages:    []inference.Message{{Role: "user", Content: "persist me"}},
		Temperature: 0.6,
		TopP:        0.8,
		MaxTokens:   42,
		Tools: []inference.Tool{{
			Type:     "function",
			Function: inference.ToolDefinition{Name: "file_list", Parameters: map[string]any{"type": "object"}},
		}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	if err := q1.walFile.Close(); err != nil {
		t.Fatalf("close first WAL: %v", err)
	}

	client := &captureClient{seen: make(chan inference.CompletionRequest, 1)}
	q2 := New(4, walPath, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q2.Start(ctx); err != nil {
		t.Fatalf("Start recovery queue: %v", err)
	}
	defer q2.Stop()

	select {
	case got := <-client.seen:
		if got.Model != "qwen" {
			t.Errorf("replayed model = %q, want qwen", got.Model)
		}
		if len(got.Messages) != 1 || got.Messages[0].Content != "persist me" {
			t.Fatalf("replayed messages = %+v, want persisted payload", got.Messages)
		}
		if got.Temperature != 0.6 {
			t.Errorf("replayed temperature = %v, want 0.6", got.Temperature)
		}
		if got.TopP != 0.8 {
			t.Errorf("replayed top_p = %v, want 0.8", got.TopP)
		}
		if got.MaxTokens != 42 {
			t.Errorf("replayed max_tokens = %d, want 42", got.MaxTokens)
		}
		if len(got.Tools) != 1 || got.Tools[0].Function.Name != "file_list" {
			t.Fatalf("replayed tools = %+v, want file_list", got.Tools)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for WAL replay")
	}
}

func TestWAL_DoneRequestIsNotReplayed(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "queue.wal")
	q1 := New(4, walPath, nil)
	if err := q1.openWAL(); err != nil {
		t.Fatalf("openWAL: %v", err)
	}
	resp := make(chan inference.Token, 1)
	if err := q1.Enqueue(Request{
		ID:       "done",
		Messages: []inference.Message{{Role: "user", Content: "already handled"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	q1.walMarkDone("done")
	if err := q1.walFile.Close(); err != nil {
		t.Fatalf("close first WAL: %v", err)
	}

	client := &captureClient{seen: make(chan inference.CompletionRequest, 1)}
	q2 := New(4, walPath, client)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := q2.Start(ctx); err != nil {
		t.Fatalf("Start recovery queue: %v", err)
	}
	defer q2.Stop()

	select {
	case got := <-client.seen:
		t.Fatalf("done request replayed unexpectedly: %+v", got)
	case <-ctx.Done():
	}
}

func TestWAL_ClearedOnCleanStop(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "queue.wal")
	q := New(4, walPath, &fakeClient{tokens: []string{"ok"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	resp := make(chan inference.Token, 4)
	if err := q.Enqueue(Request{
		ID:       "clean",
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for range resp {
	}
	q.Stop()

	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("WAL size after clean Stop = %d, want 0", info.Size())
	}
}

func TestWAL_StopKeepsUnfinishedReplay(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "queue.wal")
	q1 := New(4, walPath, nil)
	if err := q1.openWAL(); err != nil {
		t.Fatalf("openWAL: %v", err)
	}
	resp := make(chan inference.Token, 1)
	if err := q1.Enqueue(Request{
		ID:       "retry-me",
		Messages: []inference.Message{{Role: "user", Content: "survive stop"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	if err := q1.walFile.Close(); err != nil {
		t.Fatalf("close first WAL: %v", err)
	}

	failing := &failingReplayClient{called: make(chan struct{})}
	q2 := New(4, walPath, failing)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := q2.Start(ctx2); err != nil {
		t.Fatalf("Start failing recovery queue: %v", err)
	}
	select {
	case <-failing.called:
	case <-ctx2.Done():
		t.Fatal("timed out waiting for first replay attempt")
	}
	q2.Stop()

	client := &captureClient{seen: make(chan inference.CompletionRequest, 1)}
	q3 := New(4, walPath, client)
	ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()
	if err := q3.Start(ctx3); err != nil {
		t.Fatalf("Start second recovery queue: %v", err)
	}
	defer q3.Stop()

	select {
	case got := <-client.seen:
		if len(got.Messages) != 1 || got.Messages[0].Content != "survive stop" {
			t.Fatalf("replayed messages after stopped failed replay = %+v", got.Messages)
		}
	case <-ctx3.Done():
		t.Fatal("timed out waiting for replay after stopped failed replay")
	}
}

type captureClient struct {
	seen chan inference.CompletionRequest
}

func (c *captureClient) Complete(_ context.Context, req inference.CompletionRequest) (<-chan inference.Token, error) {
	c.seen <- req
	ch := make(chan inference.Token, 1)
	ch <- inference.Token{Done: true}
	close(ch)
	return ch, nil
}

func (c *captureClient) Health(_ context.Context) error { return nil }

type failingReplayClient struct {
	called chan struct{}
	once   sync.Once
}

func (c *failingReplayClient) Complete(_ context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	c.once.Do(func() { close(c.called) })
	return nil, errors.New("replay failed")
}

func (c *failingReplayClient) Health(_ context.Context) error { return nil }

type fakeQueueMetrics struct {
	ttft       []time.Duration
	throughput []float64
}

func (f *fakeQueueMetrics) TimeToFirstTokenMS(d time.Duration) error {
	f.ttft = append(f.ttft, d)
	return nil
}

func (f *fakeQueueMetrics) TokenThroughput(v float64) error {
	f.throughput = append(f.throughput, v)
	return nil
}

func TestDispatch_RecordsLatencyAndThroughputMetrics(t *testing.T) {
	q := New(4, "", &delayedMetricClient{})
	metrics := &fakeQueueMetrics{}
	q.SetMetrics(metrics)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop()

	resp := make(chan inference.Token, 8)
	if err := q.Enqueue(Request{
		ID:       "metrics-test",
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
		Response: resp,
		Ctx:      context.Background(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for range resp {
	}

	if len(metrics.ttft) != 1 {
		t.Fatalf("TTFT samples = %d, want 1", len(metrics.ttft))
	}
	if len(metrics.throughput) != 1 {
		t.Fatalf("throughput samples = %d, want 1", len(metrics.throughput))
	}
	if metrics.throughput[0] <= 0 {
		t.Fatalf("throughput = %v, want > 0", metrics.throughput[0])
	}
}

type delayedMetricClient struct{}

func (d *delayedMetricClient) Complete(ctx context.Context, _ inference.CompletionRequest) (<-chan inference.Token, error) {
	ch := make(chan inference.Token, 3)
	go func() {
		defer close(ch)
		for _, tok := range []inference.Token{{Content: "one"}, {Content: "two"}, {Done: true}} {
			select {
			case ch <- tok:
			case <-ctx.Done():
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ch, nil
}

func (d *delayedMetricClient) Health(context.Context) error { return nil }

func TestDispatch_DoesNotRecordTTFTForEmptyOrErrorOnlyStreams(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token inference.Token
	}{
		{name: "empty done", token: inference.Token{Done: true}},
		{name: "stream error", token: inference.Token{Err: errors.New("stream failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := New(4, "", &singleTokenClient{token: tc.token})
			metrics := &fakeQueueMetrics{}
			q.SetMetrics(metrics)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := q.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer q.Stop()

			resp := make(chan inference.Token, 4)
			if err := q.Enqueue(Request{ID: "empty-metrics", Response: resp, Ctx: context.Background()}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			for range resp {
			}
			if len(metrics.ttft) != 0 {
				t.Fatalf("TTFT samples = %d, want 0", len(metrics.ttft))
			}
		})
	}
}

type singleTokenClient struct {
	token inference.Token
}

func (c *singleTokenClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	ch := make(chan inference.Token, 1)
	ch <- c.token
	close(ch)
	return ch, nil
}

func (c *singleTokenClient) Health(context.Context) error { return nil }
