package tools

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
)

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
