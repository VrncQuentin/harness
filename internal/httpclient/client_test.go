package httpclient

import (
	"net/http"
	"testing"
	"time"
)

func TestNewUsesShortRequestTimeout(t *testing.T) {
	client := New()
	if client.Timeout != 10*time.Second {
		t.Fatalf("Timeout = %v, want 10s", client.Timeout)
	}
	if _, ok := client.Transport.(*http.Transport); !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
}

func TestNewStreamingUsesHeaderTimeoutWithoutOverallTimeout(t *testing.T) {
	client := NewStreaming()
	if client.Timeout != 0 {
		t.Fatalf("Timeout = %v, want no overall timeout", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 30*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 30s", transport.ResponseHeaderTimeout)
	}
}
