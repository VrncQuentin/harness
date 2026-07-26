package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	runErr := cmd.Run()
	timedOut := errors.Is(timeoutCtx.Err(), context.DeadlineExceeded)
	return goLintResult(runErr, timedOut, stdout.Bytes(), stderr.Bytes())
}

// goLintResult maps a completed golangci-lint run onto a tool result.
//
// golangci-lint exits 1 when it has issues to report, which is a run that
// worked. Every other non-zero status means the lint did not complete — a
// broken config, a package that would not build, or the timeout killing it.
// Discarding the status turned all of those into "No lint issues found.",
// which reads as a clean bill of health for a lint that never ran.
func goLintResult(runErr error, timedOut bool, stdout, stderr []byte) Result {
	errText := strings.TrimSpace(string(stderr))

	if runErr != nil && !isLintIssuesExit(runErr) {
		detail := errText
		if detail == "" {
			detail = strings.TrimSpace(string(stdout))
		}
		if timedOut {
			return Result{Error: fmt.Sprintf("go_lint: timed out: %v\n%s", runErr, detail)}
		}
		return Result{Error: fmt.Sprintf("go_lint: did not complete: %v\n%s", runErr, detail)}
	}

	if len(stdout) == 0 {
		if len(stderr) > 0 {
			return Result{Error: fmt.Sprintf("go_lint: no output; stderr: %s", string(stderr))}
		}
		return Result{Content: "No lint issues found."}
	}

	var report golangciReport
	if err := json.Unmarshal(stdout, &report); err != nil {
		return Result{Error: fmt.Sprintf("go_lint: failed to parse output: %v\nstderr: %s", err, string(stderr))}
	}

	if len(report.Issues) == 0 {
		// Exit 1 with an empty issue list means the report does not say what
		// the linter objected to. Reporting a clean run would be inventing an
		// answer the tool did not give.
		if runErr != nil {
			return Result{Error: fmt.Sprintf("go_lint: reported failure with no issues in its report: %v\nstderr: %s", runErr, errText)}
		}
		return Result{Content: "No lint issues found."}
	}

	return Result{Content: formatLintReport(report.Issues)}
}

// lintIssuesExitCode is golangci-lint's status for "the run completed and found
// issues". Every other non-zero code reports a run that did not complete.
const lintIssuesExitCode = 1

// isLintIssuesExit reports whether err is golangci-lint exiting because it had
// issues to report, as opposed to failing to run.
func isLintIssuesExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == lintIssuesExitCode
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
