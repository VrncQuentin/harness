package queue

import (
	"context"
	"fmt"
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
	return nil, fmt.Errorf("test error")
}

func (e *errInferenceClient) Health(_ context.Context) error { return nil }
