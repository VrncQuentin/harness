package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/queue"
)

// stubAssembler returns canned messages, records the last agent it saw, and
// can be told to fail instead. Zero value assembles to a single user echo.
type stubAssembler struct {
	mu        sync.Mutex
	lastAgent string
	err       error
	build     func(agent string, conv []inference.Message) []inference.Message
}

func (s *stubAssembler) Assemble(_ context.Context, agentName string, conversation []inference.Message) ([]inference.Message, error) {
	s.mu.Lock()
	s.lastAgent = agentName
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.build != nil {
		return s.build(agentName, conversation), nil
	}
	return append([]inference.Message{{Role: "system", Content: "sys"}}, conversation...), nil
}

func (s *stubAssembler) seenAgent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAgent
}

// stubEnqueuer captures the request and streams a pre-canned token script in
// a background goroutine. tokens runs to completion, then the response chan
// is closed. If err is non-nil, Enqueue returns it without spawning the
// goroutine.
type stubEnqueuer struct {
	tokens   []inference.Token
	err      error
	captured chan queue.Request
	// hold, when set, is closed by the test to release the streamer.
	hold chan struct{}
	// onDispatch is invoked from the streamer goroutine with the captured
	// request context so tests can wait for it to be cancelled.
	onDispatch func(context.Context)
}

func newStubEnqueuer(tokens []inference.Token) *stubEnqueuer {
	return &stubEnqueuer{
		tokens:   tokens,
		captured: make(chan queue.Request, 1),
	}
}

func (s *stubEnqueuer) Enqueue(req queue.Request) error {
	if s.err != nil {
		return s.err
	}
	select {
	case s.captured <- req:
	default:
	}
	go func() {
		defer close(req.Response)
		if s.hold != nil {
			// Simulate a queue worker that is mid-flight: release only when
			// the test closes hold or the client cancels ctx. Either way the
			// goroutine eventually exits without leaking.
			select {
			case <-s.hold:
			case <-req.Ctx.Done():
				if s.onDispatch != nil {
					s.onDispatch(req.Ctx)
				}
				return
			}
		}
		if s.onDispatch != nil {
			s.onDispatch(req.Ctx)
		}
		for _, tok := range s.tokens {
			select {
			case <-req.Ctx.Done():
				return
			case req.Response <- tok:
			}
		}
	}()
	return nil
}

type sessionAppendRecord struct {
	ID      string
	Role    string
	Content string
}

type stubSessionRecorder struct {
	mu      sync.Mutex
	nextID  int
	starts  []Session
	appends []sessionAppendRecord
	saves   []string
	ends    []string
	saveErr error
}

func (r *stubSessionRecorder) Start(agent string) Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	s := Session{ID: fmt.Sprintf("sess-%d", r.nextID), Agent: agent}
	r.starts = append(r.starts, s)
	return s
}

func (r *stubSessionRecorder) Append(id, role, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appends = append(r.appends, sessionAppendRecord{ID: id, Role: role, Content: content})
	return nil
}

func (r *stubSessionRecorder) Save(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves = append(r.saves, id)
	return r.saveErr
}

func (r *stubSessionRecorder) End(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, id)
}

func (r *stubSessionRecorder) snapshot() ([]Session, []sessionAppendRecord, []string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Session(nil), r.starts...), append([]sessionAppendRecord(nil), r.appends...), append([]string(nil), r.saves...), append([]string(nil), r.ends...)
}

// captureRequest returns the request submitted to Enqueue, waiting up to 1s.
func (s *stubEnqueuer) captureRequest(t *testing.T) queue.Request {
	t.Helper()
	select {
	case req := <-s.captured:
		return req
	case <-time.After(1 * time.Second):
		t.Fatal("no request enqueued within 1s")
		return queue.Request{}
	}
}

// parsedEvent is a single SSE data: line decoded from the response body.
type parsedEvent struct {
	raw  string
	done bool
}

// parseSSE splits an SSE body into its `data:` events. Blank-line separators
// are stripped. Returns events in order.
func parseSSE(t *testing.T, body io.Reader) []parsedEvent {
	t.Helper()
	var events []parsedEvent
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			events = append(events, parsedEvent{done: true})
			continue
		}
		events = append(events, parsedEvent{raw: data})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE body: %v", err)
	}
	return events
}

// contentsOf extracts the delta.content field from every non-DONE event.
func contentsOf(t *testing.T, events []parsedEvent) []string {
	t.Helper()
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e.done {
			continue
		}
		var c chatChunk
		if err := json.Unmarshal([]byte(e.raw), &c); err != nil {
			t.Fatalf("unmarshal chunk %q: %v", e.raw, err)
		}
		if len(c.Choices) == 0 {
			continue
		}
		out = append(out, c.Choices[0].Delta.Content)
	}
	return out
}

// newTestServer spins up a Server under httptest.NewServer and returns it
// plus a cleanup closer.
func newTestServer(t *testing.T, asm Assembler, q Enqueuer) (*httptest.Server, func()) {
	t.Helper()
	srv := NewServer(0, asm, q, nil)
	ts := httptest.NewServer(srv.handler())
	return ts, ts.Close
}

func TestUnexpectedServeError(t *testing.T) {
	if unexpectedServeError(nil) {
		t.Fatal("nil serve error should not be unexpected")
	}
	if unexpectedServeError(http.ErrServerClosed) {
		t.Fatal("http.ErrServerClosed should not be unexpected")
	}
	if !unexpectedServeError(errors.New("listener failed")) {
		t.Fatal("ordinary serve error should be unexpected")
	}
}

func TestChatCompletions_StreamingHappyPath(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer([]inference.Token{
		{Content: "hello"},
		{Content: " "},
		{Content: "world"},
		{Done: true},
	})
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	body := bytes.NewBufferString(`{"model":"harness","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	events := parseSSE(t, resp.Body)
	// 3 content chunks + 1 final (finish_reason) + 1 [DONE] = 5 events.
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(events), events)
	}
	if !events[len(events)-1].done {
		t.Errorf("last event is not [DONE]: %+v", events[len(events)-1])
	}

	contents := contentsOf(t, events[:3])
	if strings.Join(contents, "") != "hello world" {
		t.Errorf("concatenated delta = %q, want 'hello world'", strings.Join(contents, ""))
	}

	// Final chunk carries finish_reason=stop and no content.
	var finalChunk chatChunk
	if err := json.Unmarshal([]byte(events[3].raw), &finalChunk); err != nil {
		t.Fatalf("unmarshal final: %v", err)
	}
	if finalChunk.Choices[0].FinishReason == nil || *finalChunk.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want 'stop'", finalChunk.Choices[0].FinishReason)
	}
	if finalChunk.Choices[0].Delta.Content != "" {
		t.Errorf("final delta content = %q, want empty", finalChunk.Choices[0].Delta.Content)
	}
	if !strings.HasPrefix(finalChunk.ID, "chatcmpl-") {
		t.Errorf("chunk ID = %q, want chatcmpl- prefix", finalChunk.ID)
	}
}

func TestChatCompletions_SavesAndEndsAPISessionOnCompletion(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer([]inference.Token{
		{Content: "hello"},
		{Content: " world"},
		{Done: true},
	})
	rec := &stubSessionRecorder{}
	srv := NewServer(0, asm, enq, rec)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := bytes.NewBufferString("{\"stream\":true,\"agent\":\"coder\",\"messages\":[{\"role\":\"system\",\"content\":\"sys\"},{\"role\":\"user\",\"content\":\"hi\"}]}")
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	starts, appends, saves, ends := rec.snapshot()
	if len(starts) != 1 || starts[0].ID != "sess-1" || starts[0].Agent != "coder" {
		t.Fatalf("starts = %+v, want sess-1/coder", starts)
	}
	if len(appends) != 3 {
		t.Fatalf("appends = %+v, want system/user/assistant", appends)
	}
	if appends[0].Role != "system" || appends[1].Role != "user" || appends[2].Role != "assistant" {
		t.Fatalf("append roles = %+v", appends)
	}
	if appends[2].Content != "hello world" {
		t.Fatalf("assistant append = %q, want hello world", appends[2].Content)
	}
	if len(saves) != 1 || saves[0] != "sess-1" {
		t.Fatalf("saves = %+v, want [sess-1]", saves)
	}
	if len(ends) != 1 || ends[0] != "sess-1" {
		t.Fatalf("ends = %+v, want [sess-1]", ends)
	}
}

func TestChatCompletions_SavesPartialAPISessionOnTokenError(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer([]inference.Token{
		{Content: "partial"},
		{Err: errors.New("boom")},
	})
	rec := &stubSessionRecorder{}
	srv := NewServer(0, asm, enq, rec)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := bytes.NewBufferString("{\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	_, appends, saves, ends := rec.snapshot()
	if len(appends) != 2 {
		t.Fatalf("appends = %+v, want user and partial assistant", appends)
	}
	if appends[0].Role != "user" || appends[1].Role != "assistant" {
		t.Fatalf("append roles = %+v, want user/assistant", appends)
	}
	if appends[1].Content != "partial" {
		t.Fatalf("assistant append = %q, want partial", appends[1].Content)
	}
	if len(saves) != 1 || saves[0] != "sess-1" {
		t.Fatalf("saves = %+v, want [sess-1] on token error", saves)
	}
	if len(ends) != 1 || ends[0] != "sess-1" {
		t.Fatalf("ends = %+v, want [sess-1]", ends)
	}
}
func TestChatCompletions_QueueFull(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer(nil)
	enq.err = queue.ErrQueueFull
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	body := bytes.NewBufferString(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	var got apiError
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Type != "rate_limit_error" {
		t.Errorf("type = %q, want rate_limit_error", got.Error.Type)
	}
	if got.Error.Code != "queue_full" {
		t.Errorf("code = %q, want queue_full", got.Error.Code)
	}
	if !strings.Contains(got.Error.Message, "capacity") {
		t.Errorf("message = %q, want a capacity mention", got.Error.Message)
	}
}

func TestChatCompletions_AssemblerError(t *testing.T) {
	asm := &stubAssembler{err: errors.New("persona missing")}
	enq := newStubEnqueuer(nil)
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	body := bytes.NewBufferString(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	var got apiError
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Type != "server_error" {
		t.Errorf("type = %q, want server_error", got.Error.Type)
	}
	if got.Error.Message != "persona missing" {
		t.Errorf("message = %q, want 'persona missing'", got.Error.Message)
	}
}

func TestChatCompletions_BadJSON(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer(nil)
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader("not-json"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var got apiError
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", got.Error.Type)
	}
}

func TestChatCompletions_StreamFalseRejected(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer(nil)
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	body := bytes.NewBufferString(`{"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var got apiError
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", got.Error.Type)
	}
	if !strings.Contains(got.Error.Message, "non-streaming") {
		t.Errorf("message = %q, want a non-streaming mention", got.Error.Message)
	}
}

func TestChatCompletions_WrongMethod(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer(nil)
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestChatCompletions_HeaderAgentPlumbing(t *testing.T) {
	// Record the assembled messages the assembler returns (tagged with the
	// agent name) so we can then verify the queue sees them unmodified.
	asm := &stubAssembler{
		build: func(agent string, _ []inference.Message) []inference.Message {
			return []inference.Message{{Role: "system", Content: "persona:" + agent}}
		},
	}
	enq := newStubEnqueuer([]inference.Token{{Done: true}})
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	body := bytes.NewBufferString(`{"stream":true,"agent":"ignored","messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Harness-Agent", "coder")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the server-side goroutine completes cleanly.
	_, _ = io.Copy(io.Discard, resp.Body)

	if asm.seenAgent() != "coder" {
		t.Errorf("assembler saw agent %q, want 'coder' (header should beat body)", asm.seenAgent())
	}

	captured := enq.captureRequest(t)
	if len(captured.Completion.Messages) != 1 || captured.Completion.Messages[0].Content != "persona:coder" {
		t.Errorf("enqueued messages = %+v, want persona:coder", captured.Completion.Messages)
	}
}

func TestChatCompletions_ForwardsRequestParams(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer([]inference.Token{{Done: true}})
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	body := bytes.NewBufferString(`{
		"model":"qwen-local",
		"stream":true,
		"temperature":0.7,
		"top_p":0.9,
		"max_tokens":123,
		"messages":[{"role":"user","content":"hi"}]
	}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	captured := enq.captureRequest(t)
	completion := captured.Completion
	if completion.Model != "qwen-local" {
		t.Errorf("model = %q, want qwen-local", completion.Model)
	}
	if completion.Temperature == nil || *completion.Temperature != 0.7 {
		t.Errorf("temperature = %v, want 0.7", completion.Temperature)
	}
	if completion.TopP == nil || *completion.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9", completion.TopP)
	}
	if completion.MaxTokens == nil || *completion.MaxTokens != 123 {
		t.Errorf("max_tokens = %v, want 123", completion.MaxTokens)
	}
}

func TestChatCompletions_ForwardsExplicitZeroParams(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer([]inference.Token{{Done: true}})
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	body := bytes.NewBufferString(`{
		"model":"qwen-local",
		"stream":true,
		"temperature":0,
		"top_p":0,
		"max_tokens":0,
		"messages":[{"role":"user","content":"hi"}]
	}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	captured := enq.captureRequest(t)
	completion := captured.Completion
	if completion.Temperature == nil || *completion.Temperature != 0 {
		t.Errorf("temperature = %v, want explicit 0", completion.Temperature)
	}
	if completion.TopP == nil || *completion.TopP != 0 {
		t.Errorf("top_p = %v, want explicit 0", completion.TopP)
	}
	if completion.MaxTokens == nil || *completion.MaxTokens != 0 {
		t.Errorf("max_tokens = %v, want explicit 0", completion.MaxTokens)
	}
}

func TestModels_OK(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer(nil)
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}
	if len(got.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(got.Data))
	}
	if got.Data[0].ID != "harness" {
		t.Errorf("id = %q, want harness", got.Data[0].ID)
	}
	if got.Data[0].OwnedBy != "harness" {
		t.Errorf("owned_by = %q, want harness", got.Data[0].OwnedBy)
	}
}

func TestModels_WrongMethod(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer(nil)
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	resp, err := http.Post(ts.URL+"/v1/models", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestChatCompletions_ClientCancellationPropagates(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer(nil)
	enq.hold = make(chan struct{})
	// Ensure the streamer goroutine exits when the client disconnects so the
	// test does not leak resources on failure paths.
	defer close(enq.hold)

	var ctxDone atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	enq.onDispatch = func(ctx context.Context) {
		defer wg.Done()
		select {
		case <-ctx.Done():
			ctxDone.Store(true)
		case <-time.After(5 * time.Second):
		}
	}

	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	body := bytes.NewBufferString(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	// Wait for Enqueue to be called so we know the server is in the middle
	// of streaming before we cancel.
	_ = enq.captureRequest(t)
	cancel()

	// Give the ctx propagation a moment to land.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamer goroutine did not observe ctx cancellation in 2s")
	}
	if !ctxDone.Load() {
		t.Fatal("queue request ctx was not cancelled by client disconnect")
	}
}

func TestChatCompletions_DefaultModelEcho(t *testing.T) {
	asm := &stubAssembler{}
	enq := newStubEnqueuer([]inference.Token{{Content: "x"}, {Done: true}})
	ts, cleanup := newTestServer(t, asm, enq)
	defer cleanup()

	// No "model" in body - server should echo "harness".
	body := bytes.NewBufferString(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	events := parseSSE(t, resp.Body)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	var c chatChunk
	if err := json.Unmarshal([]byte(events[0].raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Model != "harness" {
		t.Errorf("model echo = %q, want 'harness'", c.Model)
	}
}

// Sanity check that Stop is idempotent and safe before Start, and that it
// reports whether termination was confirmed.
func TestServer_StopIdempotent(t *testing.T) {
	s := NewServer(0, &stubAssembler{}, newStubEnqueuer(nil), nil)
	if !s.Stop() { // before Start: nothing running, termination is certain
		t.Error("Stop before Start must report termination")
	}
	if !s.Stop() { // again: no-op
		t.Error("Stop before Start must report termination")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Bind to an arbitrary free port. If 0 is not legal on this platform we
	// just skip the listen portion; the goal is to cover Stop-after-Start.
	err := s.Start(ctx)
	if err != nil {
		t.Skipf("bind :0 failed (benign, environment dependent): %v", err)
	}
	if !s.Stop() {
		t.Error("Stop of an idle server must report termination")
	}
	if !s.Stop() {
		t.Error("second Stop must report termination (idempotent)")
	}
}

// Guard test: the handler wires the right paths.
func TestHandler_RoutesRegistered(t *testing.T) {
	s := NewServer(0, &stubAssembler{}, newStubEnqueuer(nil), nil)
	h := s.handler()

	cases := []struct {
		method, path string
		wantStatus   int
	}{
		{http.MethodGet, "/v1/models", http.StatusOK},
		{http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.path), func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
		})
	}
}

func TestGenLeaseActiveAgentFallback(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var asmAgent string
		stubAsm := &stubAssembler{
			build: func(agent string, conv []inference.Message) []inference.Message {
				asmAgent = agent
				return []inference.Message{{Role: "assistant", Content: "ok"}}
			},
		}
		stubRec := &stubSessionRecorder{}
		var releases int32

		s := NewServer(0, nil, newStubEnqueuer(nil), nil)
		s.WithGenLease(func() (Assembler, SessionRecorder, string, func()) {
			return stubAsm, stubRec, "coder", func() {
				atomic.AddInt32(&releases, 1)
			}
		})

		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		))
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, req)

		if asmAgent != "coder" {
			t.Errorf("assembler received agent %q, want coder", asmAgent)
		}
		starts, _, _, _ := stubRec.snapshot()
		if len(starts) == 0 || starts[0].Agent != "coder" {
			t.Errorf("session recorder Start agent = %v, want coder", starts)
		}
		if atomic.LoadInt32(&releases) != 1 {
			t.Errorf("expected 1 release, got %d", releases)
		}
	})

	t.Run("assembly-error", func(t *testing.T) {
		stubAsm := &stubAssembler{err: errors.New("persona missing")}
		var releases int32

		s := NewServer(0, nil, newStubEnqueuer(nil), nil)
		s.WithGenLease(func() (Assembler, SessionRecorder, string, func()) {
			return stubAsm, nil, "coder", func() {
				atomic.AddInt32(&releases, 1)
			}
		})

		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		))
		req.Header.Set("X-Harness-Agent", "nonexistent")
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, req)

		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rr.Code)
		}
		if atomic.LoadInt32(&releases) != 1 {
			t.Errorf("assembly-error path: expected 1 release, got %d", releases)
		}
	})
}
