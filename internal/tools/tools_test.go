package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileRead_WithinSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &fileReadTool{}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"path": filepath.Join(dir, "hello.txt")})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Content != "hello world" {
		t.Fatalf("got %q, want %q", res.Content, "hello world")
	}
}

func TestFileRead_OutsideSandbox(t *testing.T) {
	dir := t.TempDir()
	tool := &fileReadTool{}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"path": "/etc/hosts"})
	if !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("expected sandbox error, got %q", res.Error)
	}
}

func TestValidatePathRejectsSiblingWithSharedPrefix(t *testing.T) {
	root := t.TempDir()
	sibling := root + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("MkdirAll sibling: %v", err)
	}
	outside := filepath.Join(sibling, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}

	if _, err := validatePath(outside, []string{root}); !errors.Is(err, ErrSandboxViolation) {
		t.Fatalf("validatePath sibling error = %v, want ErrSandboxViolation", err)
	}
}

func TestValidatePathWindowsCaseInsensitiveMissingPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows path casing")
	}
	root := t.TempDir()
	missing := filepath.Join(root, "missing.txt")

	got, err := validatePath(missing, []string{strings.ToUpper(root)})
	if err != nil {
		t.Fatalf("validatePath mixed-case root: %v", err)
	}
	if got != missing {
		t.Fatalf("validatePath = %q, want %q", got, missing)
	}
}
func TestFileRead_MissingPath(t *testing.T) {
	dir := t.TempDir()
	tool := &fileReadTool{}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"path": filepath.Join(dir, "missing.txt")})
	if !strings.Contains(res.Error, "not found") {
		t.Fatalf("expected not-found error, got %q", res.Error)
	}
}

func TestFileList_WithinSandbox(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)

	tool := &fileListTool{}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"path": dir})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "a.txt") {
		t.Errorf("missing a.txt in output: %s", res.Content)
	}
	if !strings.Contains(res.Content, "sub/") {
		t.Errorf("missing sub/ in output: %s", res.Content)
	}
}

func TestFileList_OutsideSandbox(t *testing.T) {
	dir := t.TempDir()
	tool := &fileListTool{}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"path": "/etc"})
	if !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("expected sandbox error, got %q", res.Error)
	}
}

func TestRegistry_DuplicateReturnsError(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fileReadTool{}); err != nil {
		t.Fatalf("Register first tool: %v", err)
	}
	if err := r.Register(&fileReadTool{}); !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("Register duplicate error = %v, want ErrDuplicateTool", err)
	}
}

func TestRegistry_ListAndGet(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltins(r); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	all := r.List()
	if len(all) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(all))
	}
	for _, id := range []string{"file_read", "file_list", "file_write", "shell_exec", "web_search"} {
		if r.Get(id) == nil {
			t.Errorf("%s not found", id)
		}
	}
}

func TestDestructiveToolsRegisteredButDisabledByDefault(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltins(r); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	// Destructive tools exist in the registry.
	for _, id := range []string{"file_write", "shell_exec"} {
		if r.Get(id) == nil {
			t.Errorf("%s should be registered (M7 approval layer is active)", id)
		}
	}
}

func TestShellExec_EmptySandboxRoots(t *testing.T) {
	tool := &shellExecTool{}
	// Empty slice.
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{}}, map[string]any{"command": "ls"})
	if res.Error == "" {
		t.Fatal("expected error for empty sandbox roots, got none")
	}
	if !strings.Contains(res.Error, "no sandbox root") {
		t.Errorf("expected sandbox error, got %q", res.Error)
	}
}

func TestShellExec_BlankSandboxRoot(t *testing.T) {
	tool := &shellExecTool{}
	// Slice with one empty string.
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{""}}, map[string]any{"command": "ls"})
	if res.Error == "" {
		t.Fatal("expected error for blank sandbox root, got none")
	}
	if !strings.Contains(res.Error, "no sandbox root") {
		t.Errorf("expected sandbox error, got %q", res.Error)
	}
}

func TestShellExec_DirValidatedLikeFileTools(t *testing.T) {
	dir := t.TempDir()
	tool := &shellExecTool{}
	// Valid sandbox root → command runs.
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"command": "echo hello"})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "hello") {
		t.Errorf("expected 'hello' in output, got %q", res.Content)
	}
}

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
