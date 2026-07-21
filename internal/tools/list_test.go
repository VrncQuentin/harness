package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
