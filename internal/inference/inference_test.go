package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestComplete_ForcesStreamingRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", got)
		}
		var req CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !req.Stream {
			t.Fatal("request stream = false, want true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n")) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	ch, err := c.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for tok := range ch {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
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
