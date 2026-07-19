package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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

func TestBuiltinDescriptorsDefinePolicyMetadata(t *testing.T) {
	descriptors := BuiltinDescriptors()
	want := []Descriptor{
		{ID: "file_read", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "file_list", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "file_write", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: writes require approval"},
		{ID: "shell_exec", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: shell commands require approval"},
		{ID: "web_search", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: web search uses the network"},
	}
	if !reflect.DeepEqual(descriptors, want) {
		t.Fatalf("BuiltinDescriptors() = %#v, want %#v", descriptors, want)
	}
	for _, desc := range descriptors {
		got, ok := BuiltinDescriptor(desc.ID)
		if !ok {
			t.Fatalf("BuiltinDescriptor(%q) not found", desc.ID)
		}
		if got != desc {
			t.Fatalf("BuiltinDescriptor(%q) = %#v, want %#v", desc.ID, got, desc)
		}
		if BuiltinDefaultEnabled(desc.ID) != desc.DefaultEnabled {
			t.Fatalf("BuiltinDefaultEnabled(%q) = %v, want %v", desc.ID, BuiltinDefaultEnabled(desc.ID), desc.DefaultEnabled)
		}
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

func TestFileWrite_CreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	tool := &fileWriteTool{}
	path := filepath.Join(dir, "nested", "notes", "todo.txt")
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{
		"path":    path,
		"content": "hello",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("written content = %q, want hello", string(got))
	}
}

func TestFileWrite_ParentOverFileFails(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile parent: %v", err)
	}
	tool := &fileWriteTool{}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{
		"path":    filepath.Join(parent, "child.txt"),
		"content": "hello",
	})
	if res.Error == "" || !strings.Contains(res.Error, "create parent directories") {
		t.Fatalf("expected parent creation error, got %q", res.Error)
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

func TestShellExec_OutputIsCappedWhileCommandCompletes(t *testing.T) {
	dir := t.TempDir()
	tool := &shellExecTool{}
	command := quoteShellArg(os.Args[0]) + " -test.run=TestShellExecHelperLargeOutput -- --harness-shell-helper-large-output"
	if runtime.GOOS == "windows" {
		command = "call " + command
	}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"command": command})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "... (output truncated)") {
		t.Fatalf("expected truncation marker, got content length %d", len(res.Content))
	}
	maxLen := shellOutputLimit + len("\n... (output truncated)")
	if len(res.Content) > maxLen {
		t.Fatalf("content length = %d, want <= %d", len(res.Content), maxLen)
	}
}

func TestShellExecHelperLargeOutput(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "--harness-shell-helper-large-output" {
		return
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("x", shellOutputLimit*4))
	os.Exit(0)
}

func quoteShellArg(arg string) string {
	if runtime.GOOS == "windows" {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
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

func TestFileWrite_RejectsMissingContentWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(path, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := (&fileWriteTool{}).Execute(context.Background(), Context{SandboxRoots: []string{dir}}, map[string]any{"path": path})
	if !strings.Contains(res.Error, "content") {
		t.Fatalf("missing content error = %q", res.Error)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("file was modified to %q", got)
	}
}

func TestFileWrite_RejectsSymlinkedParentOutsideSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires a Windows privilege; junction coverage is platform-specific")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "written.txt")
	res := (&fileWriteTool{}).Execute(context.Background(), Context{SandboxRoots: []string{root}}, map[string]any{"path": path, "content": "blocked"})
	if !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("symlink write error = %q", res.Error)
	}
	if _, err := os.Stat(filepath.Join(outside, "written.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write escaped sandbox: %v", err)
	}
}
