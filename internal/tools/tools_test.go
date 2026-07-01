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
	if len(all) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(all))
	}
	for _, id := range []string{"file_read", "file_list", "file_write", "shell_exec"} {
		if r.Get(id) == nil {
			t.Errorf("%s not found", id)
		}
	}
}

func TestDestructiveToolsRegisteredButDisabledByDefault(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)

	// Destructive tools exist in the registry.
	for _, id := range []string{"file_write", "shell_exec"} {
		if r.Get(id) == nil {
			t.Errorf("%s should be registered (M7 approval layer is active)", id)
		}
	}
	// Schemas includes destructive tools.
	schemas := r.Schemas()
	if len(schemas) != 4 {
		t.Fatalf("expected 4 schemas, got %d", len(schemas))
	}
	// Tool IDs are in insertion order: file_read, file_list, file_write, shell_exec.
	expectedIDs := []string{"file_read", "file_list", "file_write", "shell_exec"}
	for i, id := range expectedIDs {
		if schemas[i]["function"].(map[string]any)["name"] != id {
			t.Errorf("schema %d: expected %s, got %v", i, id, schemas[i]["function"].(map[string]any)["name"])
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
