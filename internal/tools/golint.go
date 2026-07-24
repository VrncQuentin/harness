package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type goLintTool struct{}

var _ Tool = (*goLintTool)(nil)

func (t *goLintTool) ID() string { return "go_lint" }
func (t *goLintTool) Description() string {
	return "Run golangci-lint v2 on the given packages and return issues grouped by linter and file. Returns an error if golangci-lint is not found."
}
func (t *goLintTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"packages": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": `Package patterns to lint, e.g. ["./..."] or ["./internal/foo/..."]. Defaults to ["./..."].`,
			},
		},
		"required": []string{},
	}
}

// golangciIssue is one entry from golangci-lint's JSON output.
type golangciIssue struct {
	FromLinter string `json:"FromLinter"`
	Text       string `json:"Text"`
	Pos        struct {
		Filename string `json:"Filename"`
		Line     int    `json:"Line"`
		Column   int    `json:"Column"`
	} `json:"Pos"`
}

type golangciReport struct {
	Issues []golangciIssue `json:"Issues"`
}

func (t *goLintTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	if len(c.SandboxRoots) == 0 || c.SandboxRoots[0] == "" {
		return Result{Error: "go_lint: no sandbox root configured"}
	}
	workDir := c.SandboxRoots[0]
	if _, err := validatePath(workDir, c.SandboxRoots); err != nil {
		return Result{Error: fmt.Sprintf("go_lint: invalid working directory: %v", err)}
	}

	lintBin, err := exec.LookPath("golangci-lint")
	if err != nil {
		return Result{Error: "go_lint: golangci-lint not found — install it with: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"}
	}

	pkgs := []string{"./..."}
	if raw, ok := args["packages"]; ok {
		if parsed, err := parseStringSlice(raw); err == nil && len(parsed) > 0 {
			pkgs = parsed
		}
	}

	lintArgs := append([]string{"run", "--output.json.path=stdout"}, pkgs...)
	timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, lintBin, lintArgs...) //nolint:gosec
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	_ = cmd.Run() // non-zero exit is expected when there are lint issues

	if stdout.Len() == 0 {
		if stderr.Len() > 0 {
			return Result{Error: fmt.Sprintf("go_lint: no output; stderr: %s", stderr.String())}
		}
		return Result{Content: "No lint issues found."}
	}

	var report golangciReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return Result{Error: fmt.Sprintf("go_lint: failed to parse output: %v\nstderr: %s", err, stderr.String())}
	}

	if len(report.Issues) == 0 {
		return Result{Content: "No lint issues found."}
	}

	return Result{Content: formatLintReport(report.Issues)}
}

// formatLintReport groups issues by linter → file → sorted by line.
func formatLintReport(issues []golangciIssue) string {
	type fileIssue struct {
		line int
		col  int
		text string
	}
	type linterGroup struct {
		files map[string][]fileIssue
	}
	groups := make(map[string]*linterGroup)

	for _, iss := range issues {
		lg := groups[iss.FromLinter]
		if lg == nil {
			lg = &linterGroup{files: make(map[string][]fileIssue)}
			groups[iss.FromLinter] = lg
		}
		lg.files[iss.Pos.Filename] = append(lg.files[iss.Pos.Filename], fileIssue{
			line: iss.Pos.Line,
			col:  iss.Pos.Column,
			text: iss.Text,
		})
	}

	linters := make([]string, 0, len(groups))
	for l := range groups {
		linters = append(linters, l)
	}
	sort.Strings(linters)

	var b strings.Builder
	for _, linter := range linters {
		fmt.Fprintf(&b, "[%s]\n", linter)
		lg := groups[linter]
		files := make([]string, 0, len(lg.files))
		for f := range lg.files {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, file := range files {
			fileIssues := lg.files[file]
			sort.Slice(fileIssues, func(i, j int) bool {
				return fileIssues[i].line < fileIssues[j].line
			})
			for _, iss := range fileIssues {
				fmt.Fprintf(&b, "  %s:%d:%d: %s\n", file, iss.line, iss.col, iss.text)
			}
		}
	}
	return b.String()
}
