// Package embedder provides an HTTP client for the embedding sidecar.
// The sidecar is llama-server running in embedding mode and serving an
// OpenAI-compatible /v1/embeddings endpoint.
package embedder

import (
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

// Client calls the embedder sidecar.
type Client interface {
	Embed(ctx context.Context, chunks []string) ([][]float32, error)
}

// implClient implements Client against a base URL.
type implClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new embedder client targeting baseURL
// (e.g. "http://127.0.0.1:8082"). Pass nil for hc to use the default
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

type embeddingRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type embeddingResponse struct {
	Data []embeddingData `json:"data"`
}

type embeddingData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

// Embed returns a vector for each input chunk. Chunks are embedded in one
// batch call; the sidecar's per-request timeout is 30s.
func (c *implClient) Embed(ctx context.Context, chunks []string) ([][]float32, error) {
	if len(chunks) == 0 {
		return nil, nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, err := json.Marshal(embeddingRequest{
		Input: chunks,
		Model: "nomic-embed-text",
	})
	if err != nil {
		return nil, fmt.Errorf("embedder: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedder: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embedder: POST /v1/embeddings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedder: unexpected status %d: %s", resp.StatusCode, string(b))
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embedder: decode response: %w", err)
	}

	vectors := make([][]float32, len(chunks))
	for _, d := range result.Data {
		if d.Index >= 0 && d.Index < len(vectors) {
			vectors[d.Index] = d.Embedding
		}
	}
	for i, v := range vectors {
		if len(v) == 0 {
			return nil, fmt.Errorf("embedder: response missing embedding for input %d", i)
		}
	}
	return vectors, nil
}
