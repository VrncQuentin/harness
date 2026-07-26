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
	return "Run go test -json on the given packages and return a failure summary. Inline output is capped to 64 KB; the full output of a failing run is preserved for retrieval."
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
	out := newCappedOutput(spillCeiling)
	cmd.Stdout = out
	cmd.Stderr = out

	return goTestResult(cmd.Run(), out.String(), all)
}

// goTestResult maps a completed go test run onto a tool result.
//
// The subprocess exit status decides the outcome, not whether a failure summary
// could be parsed. Returning a summary as Content reported a failed test run as
// a successful tool call: metrics counted it as a success, the model saw plain
// content rather than an ERROR, and the governor's tee-on-failure never fired,
// because every one of those keys on Result.Error.
//
// The inline text is bounded, but a failing run also carries the untruncated
// output in FullOutput so the governor's tee writes the whole thing to disk
// rather than the excerpt the model was shown.
func goTestResult(runErr error, raw string, all bool) Result {
	inline := boundInline(raw, goTestOutputLimit)
	// failed builds a failure result that keeps the complete output for the
	// spill whenever the inline form had to be cut.
	failed := func(format string, args ...any) Result {
		res := Result{Error: fmt.Sprintf(format, args...)}
		if inline != raw {
			res.FullOutput = raw
		}
		return res
	}

	if all {
		if runErr != nil {
			return failed("go_test: %v\n%s", runErr, inline)
		}
		return Result{Content: inline}
	}

	summary := formatTestFailures(raw)
	switch {
	case runErr != nil && summary != "":
		// The summary is the useful part, but the spill keeps everything: a
		// summary is a digest of the run, not a substitute for it.
		return failed("go_test: %v\n%s", runErr, boundInline(summary, goTestOutputLimit))
	case runErr != nil:
		// Failed with nothing parseable: a build error, a panic, or the
		// timeout. The raw output is all there is to go on.
		return failed("go_test: %v\n%s", runErr, inline)
	case summary != "":
		// go reported success while its own JSON stream recorded failures. A
		// green exit code that disagrees with the events is not evidence the
		// suite passed, so the failures win.
		return failed("go_test: exited 0 but reported test failures\n%s", boundInline(summary, goTestOutputLimit))
	}
	return Result{Content: "All tests passed."}
}

// formatTestFailures parses go test -json NDJSON and returns a compact failure
// summary grouped by package and test name. Returns "" when no failures found.
func formatTestFailures(raw string) string {
	type failKey struct{ pkg, test string }
	outputs := make(map[failKey][]string)
	var failed []failKey
	seen := make(map[failKey]bool)

	scanner := bufio.NewScanner(strings.NewReader(raw))
	// A single go test -json record can exceed the scanner's default 64 KB
	// token limit — one test printing a large diff is enough. Past that the
	// scanner stops silently, and every failure after the long line vanishes
	// from the summary. The buffer is sized to the same ceiling that bounds
	// what was captured in the first place.
	scanner.Buffer(make([]byte, 0, 64*1024), spillCeiling)
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
