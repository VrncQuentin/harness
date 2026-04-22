// Package httpclient provides shared HTTP clients for the harness.
package httpclient

import (
	"net/http"
	"time"
)

// New returns an *http.Client for short-lived requests (health checks, API calls).
func New() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
	}
}

// NewStreaming returns an *http.Client for streaming responses (SSE).
// No overall timeout - callers must pass a context with appropriate deadlines.
func NewStreaming() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}
