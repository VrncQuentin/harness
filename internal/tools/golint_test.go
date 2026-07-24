package tools

import (
	"context"
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
