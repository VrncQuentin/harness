package ui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleStatus_OK(t *testing.T) {
	s := NewServer(3000)

	// Use httptest recorder directly instead of starting a real server.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Harness") {
		t.Error("expected 'Harness' in response body")
	}
}

func TestHandleStatus_WithErrors(t *testing.T) {
	s := NewServer(3000)
	s.AddStartupError(errors.New("config.toml not found"))
	s.AddStartupError(errors.New("llama-server binary not found"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "config.toml not found") {
		t.Error("expected startup error in response body")
	}
}

func TestSetLlamaStatus(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Name: "llama", Running: true, Healthy: true})

	s.state.mu.RLock()
	healthy := s.state.LlamaStatus.Healthy
	s.state.mu.RUnlock()

	if !healthy {
		t.Error("expected llama status healthy")
	}
}

func TestSetQueueDepth(t *testing.T) {
	s := NewServer(3000)
	s.SetQueueDepth(3, 8)

	s.state.mu.RLock()
	depth := s.state.QueueDepth
	max := s.state.QueueMax
	s.state.mu.RUnlock()

	if depth != 3 || max != 8 {
		t.Errorf("expected depth 3/8, got %d/%d", depth, max)
	}
}

func TestStart_ServerStarts(t *testing.T) {
	s := NewServer(13001) // use a high port to avoid conflicts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	// Give it a moment to bind.
	var resp *http.Response
	var err error
	for i := 0; i < 10; i++ {
		resp, err = http.Get("http://localhost:13001/")
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("could not connect to UI server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}
