// Package httpclient provides shared HTTP clients for the harness.
package httpclient

import (
	"net"
	"net/http"
	"time"
)

// dialTimeout caps TCP dial + DNS lookup for harness-managed clients. The
// stdlib default leans on the OS, which on Windows is ~21 s for a stalled SYN
// and can stack with IPv6 AAAA lookup timeouts to wedge a request well past
// the per-request ceiling. Three seconds is comfortably above local-network
// SYN+ACK; anything slower is a real connectivity problem we want to surface
// quickly rather than mask.
const dialTimeout = 3 * time.Second

// dialer is the shared net.Dialer used by both client constructors.
var dialer = &net.Dialer{
	Timeout:   dialTimeout,
	KeepAlive: 30 * time.Second,
}

// New returns an *http.Client for short-lived requests (health checks, API calls).
func New() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialer.DialContext
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: t,
	}
}

// NewStreaming returns an *http.Client for streaming responses (SSE).
// No overall timeout - callers must pass a context with appropriate deadlines.
func NewStreaming() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			DialContext:           dialer.DialContext,
		},
	}
}
