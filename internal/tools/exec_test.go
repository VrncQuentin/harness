package tools

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestExec_EmptySandboxRoots(t *testing.T) {
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{}},
		map[string]any{"cmd": []any{"echo", "hi"}})
	if res.Error == "" {
		t.Fatal("expected error for empty sandbox roots, got none")
	}
	if !strings.Contains(res.Error, "no sandbox root") {
		t.Errorf("expected sandbox error, got %q", res.Error)
	}
}

func TestExec_BlankSandboxRoot(t *testing.T) {
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{""}},
		map[string]any{"cmd": []any{"echo", "hi"}})
	if res.Error == "" {
		t.Fatal("expected error for blank sandbox root, got none")
	}
	if !strings.Contains(res.Error, "no sandbox root") {
		t.Errorf("expected sandbox error, got %q", res.Error)
	}
}

func TestExec_EmptyCmd(t *testing.T) {
	dir := t.TempDir()
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
		map[string]any{"cmd": []any{}})
	if res.Error == "" {
		t.Fatal("expected error for empty cmd, got none")
	}
	if !strings.Contains(res.Error, "non-empty") {
		t.Errorf("expected non-empty error, got %q", res.Error)
	}
}

func TestExec_MissingCmd(t *testing.T) {
	dir := t.TempDir()
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
		map[string]any{})
	if res.Error == "" {
		t.Fatal("expected error for missing cmd, got none")
	}
}

func TestExec_DenyFilter_DestructiveExecutable(t *testing.T) {
	dir := t.TempDir()
	tool := &execTool{}
	for _, exe := range []string{"dd", "mkfs", "fdisk", "shutdown", "reboot", "diskpart", "format"} {
		res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
			map[string]any{"cmd": []any{exe, "arg"}})
		if !strings.Contains(res.Error, "denied") {
			t.Errorf("exec(%q): expected deny, got %q", exe, res.Error)
		}
	}
}

func TestExec_DenyFilter_RecursiveDelete(t *testing.T) {
	dir := t.TempDir()
	tool := &execTool{}
	cases := [][]any{
		{"rm", "-rf", "/"},
		{"rm", "-fr", "/"},
		{"rm", "--recursive", "/"},
		{"rm", "-r", "/"},
	}
	for _, argv := range cases {
		res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
			map[string]any{"cmd": argv})
		if !strings.Contains(res.Error, "denied") {
			t.Errorf("exec(%v): expected deny, got %q", argv, res.Error)
		}
	}
}

func TestExec_DenyFilter_AllowsSafeRm(t *testing.T) {
	dir := t.TempDir()
	tmp, _ := os.CreateTemp(dir, "exec_deny_test_*.txt")
	tmp.Close()
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
		map[string]any{"cmd": []any{"rm", tmp.Name()}})
	// rm of a specific file without -r flags should not be denied.
	if strings.Contains(res.Error, "denied") {
		t.Errorf("safe rm was denied: %q", res.Error)
	}
}

func TestExec_RunsCommand(t *testing.T) {
	dir := t.TempDir()
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
		map[string]any{"cmd": []any{"go", "version"}})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "go") {
		t.Errorf("expected 'go' in output, got %q", res.Content)
	}
}

func TestExec_OutputIsCappedWhileCommandCompletes(t *testing.T) {
	dir := t.TempDir()
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
		map[string]any{"cmd": []any{os.Args[0], "-test.run=TestExecHelperLargeOutput", "--", "--harness-exec-helper-large-output"}})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "... (output truncated)") {
		t.Fatalf("expected truncation marker, got content length %d", len(res.Content))
	}
	maxLen := execOutputLimit + len("\n... (output truncated)")
	if len(res.Content) > maxLen {
		t.Fatalf("content length = %d, want <= %d", len(res.Content), maxLen)
	}
}

func TestExecHelperLargeOutput(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "--harness-exec-helper-large-output" {
		return
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("x", execOutputLimit*4))
	os.Exit(0)
}
