package inference

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealth_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestHealth_NotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected error for 503 response")
	}
}

func TestComplete_Streaming(t *testing.T) {
	sseBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":" world"},"finish_reason":null}]}`,
		`data: [DONE]`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sseBody)) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	ch, err := c.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var content string
	for tok := range ch {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
		if tok.Done {
			break
		}
		content += tok.Content
	}
	if content != "Hello world" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestComplete_TruncatedStreamReturnsError(t *testing.T) {
	sseBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sseBody)) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	ch, err := c.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gotErr error
	for tok := range ch {
		if tok.Err != nil {
			gotErr = tok.Err
		}
	}
	if !errors.Is(gotErr, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error = %v, want unexpected EOF", gotErr)
	}
}

func TestComplete_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Write nothing - request will be cancelled before server responds.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	c := NewClient(srv.URL, nil)
	_, err := c.Complete(ctx, CompletionRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestComplete_NonStreamingToolCalls(t *testing.T) {
	body := `{"choices":[{"message":{"content":"Need a file","tool_calls":[{"id":"call_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"README.md\"}"}}]}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	ch, err := c.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: "user", Content: "read"}},
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var content string
	var tool *ToolCallDelta
	var done bool
	for tok := range ch {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
		if tok.Content != "" {
			content += tok.Content
		}
		if tok.ToolCallDelta != nil {
			tool = tok.ToolCallDelta
		}
		if tok.Done {
			done = true
		}
	}
	if content != "Need a file" {
		t.Fatalf("content = %q", content)
	}
	if tool == nil || tool.ID != "call_1" || tool.Name != "file_read" || !strings.Contains(tool.Arguments, "README.md") {
		t.Fatalf("tool delta not parsed: %#v", tool)
	}
	if !done {
		t.Fatal("expected done token")
	}
}
