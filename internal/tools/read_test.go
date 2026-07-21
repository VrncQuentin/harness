package tools

import (
	"context"
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

func TestFileRead_MissingPath(t *testing.T) {
	dir := t.TempDir()
	tool := &fileReadTool{}
	res := tool.Execute(context.TODO(), Context{SandboxRoots: []string{dir}}, map[string]any{"path": filepath.Join(dir, "missing.txt")})
	if !strings.Contains(res.Error, "not found") {
		t.Fatalf("expected not-found error, got %q", res.Error)
	}
}
