package embedder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbedMapsVectorsByResponseIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}

		var req embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "nomic-embed-text" {
			t.Fatalf("model = %q, want nomic-embed-text", req.Model)
		}
		if strings.Join(req.Input, ",") != "first,second" {
			t.Fatalf("input = %#v", req.Input)
		}

		_ = json.NewEncoder(w).Encode(embeddingResponse{Data: []embeddingData{
			{Index: 1, Embedding: []float32{0.2, 0.3}},
			{Index: 0, Embedding: []float32{0.1, 0.4}},
		}})
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/", srv.Client())
	vectors, err := client.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 2 {
		t.Fatalf("len(vectors) = %d, want 2", len(vectors))
	}
	if vectors[0][0] != 0.1 || vectors[1][0] != 0.2 {
		t.Fatalf("vectors not mapped by index: %#v", vectors)
	}
}

func TestEmbedEmptyInputSkipsHTTP(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	vectors, err := client.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if vectors != nil {
		t.Fatalf("vectors = %#v, want nil", vectors)
	}
	if called {
		t.Fatal("server was called for empty input")
	}
}

func TestEmbedUnexpectedStatusIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	_, err := client.Embed(context.Background(), []string{"chunk"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unexpected status 503") || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestHealthAcceptsAny2xxStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestHealthRejectsNon2xxStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, srv.Client())
	if err := client.Health(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
