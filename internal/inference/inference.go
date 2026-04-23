// Package inference provides an OpenAI-compatible HTTP client for llama-server.
package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vrnc/harness/pkg/httpclient"
)

// Client is the interface for the inference backend.
type Client interface {
	Complete(ctx context.Context, req CompletionRequest) (<-chan Token, error)
	Health(ctx context.Context) error
}

// Token is a single streamed token from a completion.
type Token struct {
	Content string
	Done    bool
	Err     error
}

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompletionRequest is the request body for /v1/chat/completions.
type CompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	TopP        float64   `json:"top_p,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
}

// sseChunk is the JSON structure of a single SSE data line.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// implClient implements Client against a base URL.
type implClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new inference client targeting baseURL
// (e.g. "http://127.0.0.1:8081"). Pass nil for hc to use the default
// streaming-optimised client from pkg/httpclient.
func NewClient(baseURL string, hc *http.Client) Client {
	if hc == nil {
		hc = httpclient.NewStreaming()
	}
	return &implClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: hc,
	}
}

// Health performs a GET /health and returns nil if the server is up.
func (c *implClient) Health(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.baseURL+"/health", http.NoBody)
	if err != nil {
		return fmt.Errorf("inference: build health request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("inference: health check: %w", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= 300 {
		return fmt.Errorf("inference: health check returned %d", resp.StatusCode)
	}
	return nil
}

// Complete sends a streaming chat completion request and returns a channel of tokens.
// The channel is closed after the last token or on error. Cancelling ctx stops the stream.
func (c *implClient) Complete(ctx context.Context, req CompletionRequest) (<-chan Token, error) {
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("inference: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("inference: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("inference: POST /v1/chat/completions: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("inference: unexpected status %d: %s", resp.StatusCode, string(b))
	}

	// Token buffer absorbs short network bursts so the SSE reader does not
	// block on a slow consumer mid-stream. 64 covers a handful of round
	// trips of llama-server token batches.
	ch := make(chan Token, 64)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		readSSE(ctx, resp.Body, ch)
	}()

	return ch, nil
}

// readSSE reads the SSE stream from r and sends tokens on ch.
func readSSE(ctx context.Context, r io.Reader, ch chan<- Token) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if ctx.Err() != nil {
			ch <- Token{Err: ctx.Err()}
			return
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			ch <- Token{Done: true}
			return
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Skip malformed lines.
			continue
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				select {
				case ch <- Token{Content: choice.Delta.Content}:
				case <-ctx.Done():
					ch <- Token{Err: ctx.Err()}
					return
				}
			}
			if choice.FinishReason != nil {
				ch <- Token{Done: true}
				return
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		ch <- Token{Err: fmt.Errorf("inference: SSE read error: %w", err)}
	}
}
