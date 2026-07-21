package tools

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseDuckDuckGoResults(t *testing.T) {
	body := []byte(`{
		"Heading":"Harness",
		"AbstractText":"A local inference harness.",
		"AbstractURL":"https://example.com/harness",
		"RelatedTopics":[
			{"Text":"Harness project - source code","FirstURL":"https://example.com/source"},
			{"Topics":[{"Text":"Nested result - details","FirstURL":"https://example.com/nested"}]}
		]
	}`)
	results, err := parseDuckDuckGoResults(body, 2)
	if err != nil {
		t.Fatalf("parseDuckDuckGoResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Title != "Harness" || results[0].URL != "https://example.com/harness" {
		t.Fatalf("unexpected first result: %+v", results[0])
	}
	if results[1].Title != "Harness project" {
		t.Fatalf("unexpected second title: %+v", results[1])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestWebSearch_ExecuteDisclosesNetworkUse(t *testing.T) {
	tool := &webSearchTool{}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("q") != "local harness" {
			t.Fatalf("unexpected query: %s", req.URL.RawQuery)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{
				"Heading":"Harness",
				"AbstractText":"A local inference harness.",
				"AbstractURL":"https://example.com/harness"
			}`)),
			Header: make(http.Header),
		}, nil
	})}
	res := tool.Execute(context.TODO(), Context{HTTPClient: client}, map[string]any{"query": "local harness"})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	for _, want := range []string{"Network request used", "local harness", "Harness", "https://example.com/harness"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("expected content to include %q, got %q", want, res.Content)
		}
	}
}
