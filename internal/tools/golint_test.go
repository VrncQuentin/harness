package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestGoLint_NoSandboxRoot(t *testing.T) {
	tool := &goLintTool{}
	res := tool.Execute(context.TODO(), CallInfo{}, map[string]any{})
	if !strings.Contains(res.Error, "no sandbox root") {
		t.Errorf("expected sandbox error, got %q", res.Error)
	}
}

func TestFormatLintReport_GroupsByLinterAndFile(t *testing.T) {
	issues := []golangciIssue{
		{FromLinter: "errcheck", Text: "error not checked", Pos: struct {
			Filename string `json:"Filename"`
			Line     int    `json:"Line"`
			Column   int    `json:"Column"`
		}{Filename: "foo.go", Line: 10, Column: 5}},
		{FromLinter: "gofmt", Text: "file not formatted", Pos: struct {
			Filename string `json:"Filename"`
			Line     int    `json:"Line"`
			Column   int    `json:"Column"`
		}{Filename: "bar.go", Line: 1, Column: 1}},
		{FromLinter: "errcheck", Text: "another error", Pos: struct {
			Filename string `json:"Filename"`
			Line     int    `json:"Line"`
			Column   int    `json:"Column"`
		}{Filename: "foo.go", Line: 20, Column: 3}},
	}
	report := formatLintReport(issues)
	// errcheck should come before gofmt (alphabetical)
	if !strings.Contains(report, "[errcheck]") || !strings.Contains(report, "[gofmt]") {
		t.Errorf("missing linter sections: %q", report)
	}
	errIdx := strings.Index(report, "[errcheck]")
	fmtIdx := strings.Index(report, "[gofmt]")
	if errIdx > fmtIdx {
		t.Errorf("expected errcheck before gofmt")
	}
	// Both foo.go issues should appear under errcheck, ordered by line
	if !strings.Contains(report, "foo.go:10:5") || !strings.Contains(report, "foo.go:20:3") {
		t.Errorf("missing file:line entries in %q", report)
	}
}

func TestFormatLintReport_Empty(t *testing.T) {
	report := formatLintReport(nil)
	if report != "" {
		t.Errorf("expected empty report for no issues, got %q", report)
	}
}

// TestGoLintHelperExit is a helper process: re-invoked by exitErrWithCode to
// produce a real *exec.ExitError with a chosen status, cross-platform.
func TestGoLintHelperExit(t *testing.T) {
	code := os.Getenv("GO_LINT_HELPER_EXIT_CODE")
	if code == "" {
		return
	}
	n, err := strconv.Atoi(code)
	if err != nil {
		t.Fatalf("bad helper exit code %q: %v", code, err)
	}
	os.Exit(n)
}

func exitErrWithCode(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestGoLintHelperExit") //nolint:gosec
	cmd.Env = append(os.Environ(), fmt.Sprintf("GO_LINT_HELPER_EXIT_CODE=%d", code))
	err := cmd.Run()
	if err == nil {
		t.Fatalf("helper exited 0, want status %d", code)
	}
	return err
}

// golangci-lint exits 1 when it found issues — a run that worked. Every other
// non-zero status means it did not complete, and discarding the status reported
// all of them as "No lint issues found.": a clean bill of health for a lint
// that never ran.
func TestGoLintResult(t *testing.T) {
	issuesJSON := []byte(`{"Issues":[{"FromLinter":"govet","Text":"bad","Pos":{"Filename":"a.go","Line":3,"Column":2}}]}`)
	emptyJSON := []byte(`{"Issues":[]}`)

	tests := []struct {
		name       string
		exitCode   int // 0 means the run succeeded
		timedOut   bool
		stdout     []byte
		stderr     []byte
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "clean run",
			stdout:     emptyJSON,
			wantSubstr: "No lint issues found.",
		},
		{
			name:       "no output at all",
			wantSubstr: "No lint issues found.",
		},
		{
			name:       "issues found",
			exitCode:   1,
			stdout:     issuesJSON,
			wantSubstr: "govet",
		},
		{
			name:       "config error with no output",
			exitCode:   7,
			wantErr:    true,
			wantSubstr: "did not complete",
		},
		{
			name:       "build failure reported on stderr",
			exitCode:   3,
			stderr:     []byte("typechecking error"),
			wantErr:    true,
			wantSubstr: "typechecking error",
		},
		{
			name:       "killed by the timeout",
			exitCode:   2,
			timedOut:   true,
			wantErr:    true,
			wantSubstr: "timed out",
		},
		{
			name:       "issues exit with an empty report",
			exitCode:   1,
			stdout:     emptyJSON,
			stderr:     []byte("something went wrong"),
			wantErr:    true,
			wantSubstr: "no issues in its report",
		},
		{
			name:       "unparseable output",
			stdout:     []byte("not json"),
			wantErr:    true,
			wantSubstr: "failed to parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runErr error
			if tt.exitCode != 0 {
				runErr = exitErrWithCode(t, tt.exitCode)
			}
			res := goLintResult(runErr, tt.timedOut, tt.stdout, tt.stderr)
			if tt.wantErr {
				if res.Error == "" {
					t.Fatalf("Error empty, want a failure (content %q)", res.Content)
				}
				if res.Content != "" {
					t.Errorf("Content = %q alongside an error; a lint that did not run must not look clean", res.Content)
				}
				if !strings.Contains(res.Error, tt.wantSubstr) {
					t.Errorf("Error = %q, want it to contain %q", res.Error, tt.wantSubstr)
				}
				return
			}
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			if !strings.Contains(res.Content, tt.wantSubstr) {
				t.Errorf("Content = %q, want it to contain %q", res.Content, tt.wantSubstr)
			}
		})
	}
}
