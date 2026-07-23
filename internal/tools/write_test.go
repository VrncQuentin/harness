package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileWrite_CreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	tool := &fileWriteTool{}
	path := filepath.Join(dir, "nested", "notes", "todo.txt")
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}}, map[string]any{
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
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}}, map[string]any{
		"path":    filepath.Join(parent, "child.txt"),
		"content": "hello",
	})
	if res.Error == "" || !strings.Contains(res.Error, "create parent directories") {
		t.Fatalf("expected parent creation error, got %q", res.Error)
	}
}

func TestFileWrite_RejectsMissingContentWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(path, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := (&fileWriteTool{}).Execute(context.Background(), CallInfo{SandboxRoots: []string{dir}}, map[string]any{"path": path})
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
	res := (&fileWriteTool{}).Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{"path": path, "content": "blocked"})
	if !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("symlink write error = %q", res.Error)
	}
	if _, err := os.Stat(filepath.Join(outside, "written.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write escaped sandbox: %v", err)
	}
}
