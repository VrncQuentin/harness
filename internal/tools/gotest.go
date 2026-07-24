package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const goTestOutputLimit = 64 * 1024

type goTestTool struct{}

var _ Tool = (*goTestTool)(nil)

func (t *goTestTool) ID() string { return "go_test" }
func (t *goTestTool) Description() string {
	return "Run go test -json on the given packages and return a failure summary. Full output is capped to 64 KB."
}
func (t *goTestTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"packages": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": `Package patterns to test, e.g. ["./..."] or ["./internal/foo/..."].`,
			},
			"run": map[string]any{
				"type":        "string",
				"description": "Optional -run regexp to filter which tests execute.",
			},
			"timeout": map[string]any{
				"type":        "string",
				"description": `Per-package test timeout, e.g. "120s" (default 120s).`,
			},
			"all": map[string]any{
				"type":        "boolean",
				"description": "When true, return all test output, not just failures. Default false.",
			},
		},
		"required": []string{"packages"},
	}
}

// goTestEvent is a single line of go test -json output.
type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

func (t *goTestTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	if len(c.SandboxRoots) == 0 || c.SandboxRoots[0] == "" {
		return Result{Error: "go_test: no sandbox root configured"}
	}
	workDir := c.SandboxRoots[0]
	if _, err := validatePath(workDir, c.SandboxRoots); err != nil {
		return Result{Error: fmt.Sprintf("go_test: invalid working directory: %v", err)}
	}

	pkgs, err := parseStringSlice(args["packages"])
	if err != nil || len(pkgs) == 0 {
		return Result{Error: "go_test: packages must be a non-empty array of strings"}
	}

	timeout := "120s"
	if ts, ok := args["timeout"].(string); ok && ts != "" {
		timeout = ts
	}
	all, _ := args["all"].(bool)

	goArgs := []string{"test", "-json", "-timeout", timeout}
	if run, ok := args["run"].(string); ok && run != "" {
		goArgs = append(goArgs, "-run", run)
	}
	goArgs = append(goArgs, pkgs...)

	timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "go", goArgs...) //nolint:gosec
	cmd.Dir = workDir
	out := newCappedOutput(goTestOutputLimit)
	cmd.Stdout = out
	cmd.Stderr = out

	runErr := cmd.Run()

	if all {
		content := out.String()
		if runErr != nil {
			return Result{Error: fmt.Sprintf("go_test: %v\n%s", runErr, content)}
		}
		return Result{Content: content}
	}

	summary := formatTestFailures(out.String())
	if runErr != nil && summary == "" {
		return Result{Error: fmt.Sprintf("go_test: %v\n%s", runErr, out.String())}
	}
	if summary == "" {
		return Result{Content: "All tests passed."}
	}
	return Result{Content: summary}
}

// formatTestFailures parses go test -json NDJSON and returns a compact failure
// summary grouped by package and test name. Returns "" when no failures found.
func formatTestFailures(raw string) string {
	type failKey struct{ pkg, test string }
	outputs := make(map[failKey][]string)
	var failed []failKey
	seen := make(map[failKey]bool)

	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		key := failKey{ev.Package, ev.Test}
		switch ev.Action {
		case "output":
			if ev.Test != "" {
				outputs[key] = append(outputs[key], ev.Output)
			}
		case "fail":
			if !seen[key] {
				seen[key] = true
				failed = append(failed, key)
			}
		}
	}

	if len(failed) == 0 {
		return ""
	}

	var b strings.Builder
	for _, k := range failed {
		if k.test == "" {
			fmt.Fprintf(&b, "FAIL %s\n", k.pkg)
		} else {
			fmt.Fprintf(&b, "--- FAIL: %s/%s\n", k.pkg, k.test)
			for _, line := range outputs[k] {
				b.WriteString(line)
			}
		}
	}
	return b.String()
}
