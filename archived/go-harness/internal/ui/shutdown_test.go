package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandleShutdown_RejectsGET(t *testing.T) {
	s := NewServer(3000)
	s.SetQuit(func() {})

	req := httptest.NewRequest(http.MethodGet, "/shutdown", nil)
	rec := httptest.NewRecorder()
	s.handleShutdown(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleShutdown_NoCallbackReturns503(t *testing.T) {
	// SetQuit was never called - the user must see a clear failure
	// rather than a no-op success that strands the UI.
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	rec := httptest.NewRecorder()
	s.handleShutdown(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no quit callback, got %d", rec.Code)
	}
}

func TestHandleShutdown_RendersShutdownPage(t *testing.T) {
	s := NewServer(3000)
	s.SetQuit(func() {})

	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	rec := httptest.NewRecorder()
	s.handleShutdown(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "shutting down") {
		t.Errorf("expected shutdown page text, got: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html content type, got %q", ct)
	}
}

func TestHandleShutdown_FiresQuitCallback(t *testing.T) {
	s := NewServer(3000)
	var calls int32
	s.SetQuit(func() { atomic.AddInt32(&calls, 1) })

	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	rec := httptest.NewRecorder()
	s.handleShutdown(rec, req)

	// quit fires asynchronously after shutdownDelay - wait long enough
	// for the goroutine to run before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected quit callback to fire once, got %d", calls)
	}
}

func TestHandleShutdown_DoubleClickFiresQuitOnce(t *testing.T) {
	// A user double-clicking the confirm button must not run the
	// shutdown sequence twice; sync.Once collapses the second call.
	s := NewServer(3000)
	var calls int32
	s.SetQuit(func() { atomic.AddInt32(&calls, 1) })

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
		rec := httptest.NewRecorder()
		s.handleShutdown(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d", i, rec.Code)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Wait an extra beat to let any erroneous extra goroutines fire.
	time.Sleep(2 * shutdownDelay)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly one quit invocation, got %d", got)
	}
}

func TestSetQuit_Roundtrip(t *testing.T) {
	s := NewServer(3000)
	if s.getQuit() != nil {
		t.Error("default quit must be nil")
	}
	called := false
	s.SetQuit(func() { called = true })
	if fn := s.getQuit(); fn == nil {
		t.Fatal("getQuit returned nil after SetQuit")
	} else {
		fn()
	}
	if !called {
		t.Error("installed quit callback was not invoked")
	}
}

func TestHandleStatus_RendersShutdownButton(t *testing.T) {
	// The button lives in the shared layout, so it must appear on every
	// page. Status is the simplest one to render without extra wiring.
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `data-open-dialog="harness-shutdown"`) {
		t.Error("layout should render the shutdown button on every page")
	}
	if !strings.Contains(body, `id="harness-shutdown"`) {
		t.Error("layout should render the shutdown confirmation dialog")
	}
	if !strings.Contains(body, `action="/shutdown"`) {
		t.Error("shutdown dialog form should POST to /shutdown")
	}
}
