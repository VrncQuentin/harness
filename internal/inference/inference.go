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

// Token is a single streamed token from a completion. Content carries text
// deltas; ToolCallDelta carries partial tool-call JSON during tool-use turns.
type Token struct {
	Content       string         `json:"content,omitempty"`
	Done          bool           `json:"done"`
	Err           error          `json:"-"`
	ToolCallDelta *ToolCallDelta `json:"tool_call_delta,omitempty"`
}

// ToolCallDelta is a streaming tool-call fragment. The caller accumulates
// these to assemble a complete ToolCall.
type ToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall represents a complete tool invocation the model requested.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the function name and JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a tool definition sent to the model in a completion request.
type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

// ToolDefinition describes a single callable function for the model.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Message is a single chat message. ToolCalls is populated on assistant
// messages with tool-use turns; ToolCallID identifies a tool result message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// CompletionRequest is the request body for /v1/chat/completions.
type CompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	TopP        float64   `json:"top_p,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"`
}

// sseChunk is the JSON structure of a single SSE data line.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type completionResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
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

// Complete sends a chat completion request and returns a channel of tokens.
// Streaming requests are forwarded as SSE; non-streaming requests are converted
// into one or more tokens followed by Done. Cancelling ctx stops the request.
func (c *implClient) Complete(ctx context.Context, req CompletionRequest) (<-chan Token, error) {
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
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("inference: POST /v1/chat/completions: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("inference: unexpected status %d: %s", resp.StatusCode, string(b))
	}

	// Token buffer absorbs short network bursts so the reader does not
	// block on a slow consumer mid-stream. 64 covers a handful of round
	// trips of llama-server token batches.
	ch := make(chan Token, 64)
	if !req.Stream {
		go func() {
			defer close(ch)
			defer func() { _ = resp.Body.Close() }()
			readCompletion(ctx, resp.Body, ch)
		}()
		return ch, nil
	}

	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		readSSE(ctx, resp.Body, ch)
	}()

	return ch, nil
}

func readCompletion(ctx context.Context, r io.Reader, ch chan<- Token) {
	var body completionResponse
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		emitToken(ctx, ch, Token{Err: fmt.Errorf("inference: decode completion: %w", err)})
		return
	}
	for _, choice := range body.Choices {
		if choice.Message.Content != "" {
			if !emitToken(ctx, ch, Token{Content: choice.Message.Content}) {
				return
			}
		}
		for i, tc := range choice.Message.ToolCalls {
			if !emitToken(ctx, ch, Token{ToolCallDelta: &ToolCallDelta{
				Index:     i,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}}) {
				return
			}
		}
	}
	emitToken(ctx, ch, Token{Done: true})
}

// readSSE reads the SSE stream from r and sends tokens on ch.
func readSSE(ctx context.Context, r io.Reader, ch chan<- Token) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if ctx.Err() != nil {
			emitToken(ctx, ch, Token{Err: ctx.Err()})
			return
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			emitToken(ctx, ch, Token{Done: true})
			return
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Skip malformed lines.
			continue
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if !emitToken(ctx, ch, Token{Content: choice.Delta.Content}) {
					return
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				if !emitToken(ctx, ch, Token{ToolCallDelta: &ToolCallDelta{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				}}) {
					return
				}
			}
			if choice.FinishReason != nil {
				emitToken(ctx, ch, Token{Done: true})
				return
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		emitToken(ctx, ch, Token{Err: fmt.Errorf("inference: SSE read error: %w", err)})
		return
	}
	if ctx.Err() == nil {
		emitToken(ctx, ch, Token{Err: fmt.Errorf("inference: SSE ended before completion: %w", io.ErrUnexpectedEOF)})
	}
}

func emitToken(ctx context.Context, ch chan<- Token, tok Token) bool {
	select {
	case ch <- tok:
		return true
	case <-ctx.Done():
		return false
	}
}
