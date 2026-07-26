package tools

import (
	"context"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
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
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(res.Content, "(truncated") {
		t.Fatalf("expected truncation marker, got content length %d", len(res.Content))
	}
	maxLen := execOutputLimit + len("\n… (truncated; full output preserved for retrieval)")
	if len(res.Content) > maxLen {
		t.Fatalf("content length = %d, want <= %d", len(res.Content), maxLen)
	}
}

// The inline text stays bounded, but a failing command must also hand the
// governor the untruncated output. Capturing at the inline cap meant the tee
// wrote the same truncated text the model had already seen and labelled it the
// full output.
func TestExec_FailingCommandPreservesFullOutput(t *testing.T) {
	dir := t.TempDir()
	tool := &execTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}},
		map[string]any{"cmd": []any{os.Args[0], "-test.run=TestExecHelperLargeOutputThenFail", "--", "--harness-exec-helper-large-output-fail"}})

	if res.Error == "" {
		t.Fatal("expected a failure from the helper")
	}
	if res.FullOutput == "" {
		t.Fatal("FullOutput empty; the governor would spill the truncated excerpt")
	}
	if len(res.FullOutput) <= len(res.Error) {
		t.Errorf("FullOutput (%d bytes) is not larger than the inline error (%d bytes)",
			len(res.FullOutput), len(res.Error))
	}
	if !utf8.ValidString(res.Error) {
		t.Error("inline error is not valid UTF-8")
	}
}

func TestBoundInline(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "under the limit", in: "short", limit: 100, want: "short"},
		{name: "exactly the limit", in: "abcde", limit: 5, want: "abcde"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boundInline(tt.in, tt.limit); got != tt.want {
				t.Errorf("boundInline = %q, want %q", got, tt.want)
			}
		})
	}

	// Cutting mid-rune would put invalid UTF-8 into the conversation and from
	// there into session records.
	multibyte := strings.Repeat("é", 100) // two bytes per rune
	for limit := 1; limit <= 40; limit++ {
		got := boundInline(multibyte, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("boundInline(limit=%d) produced invalid UTF-8", limit)
		}
	}
}

func TestExecHelperLargeOutput(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "--harness-exec-helper-large-output" {
		return
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("x", execOutputLimit*4))
	os.Exit(0)
}

// TestExecHelperLargeOutputThenFail is a helper process: it emits more than the
// inline cap and then exits non-zero, so the failure path has a full output
// worth preserving.
func TestExecHelperLargeOutputThenFail(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "--harness-exec-helper-large-output-fail" {
		return
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("y", execOutputLimit*4))
	os.Exit(1)
}
