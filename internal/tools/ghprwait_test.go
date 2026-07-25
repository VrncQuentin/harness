package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGHPRWaitTool_MissingRequired(t *testing.T) {
	tool := &ghPRWaitTool{}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing owner", map[string]any{"repo": "r", "pr_number": float64(1)}, "owner"},
		{"missing repo", map[string]any{"owner": "o", "pr_number": float64(1)}, "repo"},
		{"missing pr_number", map[string]any{"owner": "o", "repo": "r"}, "pr_number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tool.Execute(context.Background(), CallInfo{GHTokenFn: func() string { return "tok" }}, tc.args)
			if res.Error == "" {
				t.Fatal("expected error")
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("error %q should mention %q", res.Error, tc.want)
			}
		})
	}
}

func TestGHPRWaitTool_MissingToken(t *testing.T) {
	tool := &ghPRWaitTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{
		"owner": "o", "repo": "r", "pr_number": float64(1),
	})
	if res.Error == "" || !strings.Contains(res.Error, "GITHUB_TOKEN") {
		t.Errorf("expected GITHUB_TOKEN error, got %q", res.Error)
	}
}

// fakeGHServer builds an httptest.Server that returns a fixed PR head SHA and
// a fixed list of check-run states for testing.
func fakeGHServer(t *testing.T, headSHA string, checkRuns []map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"head": map[string]any{"sha": headSHA},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/check-runs") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": len(checkRuns),
				"check_runs":  checkRuns,
			})
			return
		}
		http.NotFound(w, r)
	}))
}

func TestGHPRWaitTool_Green(t *testing.T) {
	srv := fakeGHServer(t, "sha123", []map[string]string{
		{"name": "test", "status": "completed", "conclusion": "success"},
		{"name": "lint", "status": "completed", "conclusion": "skipped"},
	})
	defer srv.Close()

	tool := &ghPRWaitTool{}
	c := CallInfo{
		GHTokenFn:  func() string { return "tok" },
		HTTPClient: srv.Client(),
	}
	// Patch ghAPIBase for the test.
	origBase := ghAPIBase
	ghAPIBase = srv.URL
	defer func() { ghAPIBase = origBase }()

	res := tool.Execute(context.Background(), c, map[string]any{
		"owner": "o", "repo": "r", "pr_number": float64(1),
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, `"green"`) {
		t.Errorf("expected green result, got: %s", res.Content)
	}
}

func TestGHPRWaitTool_Red(t *testing.T) {
	srv := fakeGHServer(t, "sha456", []map[string]string{
		{"name": "test", "status": "completed", "conclusion": "failure"},
		{"name": "build", "status": "completed", "conclusion": "success"},
	})
	defer srv.Close()

	tool := &ghPRWaitTool{}
	c := CallInfo{
		GHTokenFn:  func() string { return "tok" },
		HTTPClient: srv.Client(),
	}
	origBase := ghAPIBase
	ghAPIBase = srv.URL
	defer func() { ghAPIBase = origBase }()

	res := tool.Execute(context.Background(), c, map[string]any{
		"owner": "o", "repo": "r", "pr_number": float64(2),
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, `"red"`) {
		t.Errorf("expected red result, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "test") {
		t.Errorf("failed check name should appear in result: %s", res.Content)
	}
}

func TestGHPRWaitTool_TimeoutCapped(t *testing.T) {
	tool := &ghPRWaitTool{}
	// Just verify the schema has x-expected-blocking
	schema := tool.Schema()
	props, _ := schema["properties"].(map[string]any)
	ts, _ := props["timeout_seconds"].(map[string]any)
	if ts["x-expected-blocking"] != true {
		t.Error("timeout_seconds should have x-expected-blocking: true in schema")
	}
}
