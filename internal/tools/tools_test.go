package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileRead_WithinSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &fileReadTool{}
	res := tool.Execute(nil, Context{SandboxRoots: []string{dir}}, map[string]any{"path": filepath.Join(dir, "hello.txt")})
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
	res := tool.Execute(nil, Context{SandboxRoots: []string{dir}}, map[string]any{"path": "/etc/hosts"})
	if !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("expected sandbox error, got %q", res.Error)
	}
}

func TestFileRead_MissingPath(t *testing.T) {
	dir := t.TempDir()
	tool := &fileReadTool{}
	res := tool.Execute(nil, Context{SandboxRoots: []string{dir}}, map[string]any{"path": filepath.Join(dir, "missing.txt")})
	if !strings.Contains(res.Error, "not found") {
		t.Fatalf("expected not-found error, got %q", res.Error)
	}
}

func TestFileList_WithinSandbox(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)

	tool := &fileListTool{}
	res := tool.Execute(nil, Context{SandboxRoots: []string{dir}}, map[string]any{"path": dir})
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
	res := tool.Execute(nil, Context{SandboxRoots: []string{dir}}, map[string]any{"path": "/etc"})
	if !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("expected sandbox error, got %q", res.Error)
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r := NewRegistry()
	r.Register(&fileReadTool{})
	r.Register(&fileReadTool{}) // should panic
}

func TestRegistry_ListAndGet(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	all := r.List()
	if len(all) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(all))
	}
	if r.Get("file_read") == nil {
		t.Fatal("file_read not found")
	}
	if r.Get("file_list") == nil {
		t.Fatal("file_list not found")
	}
}
