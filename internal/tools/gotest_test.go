package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExitError stands in for *exec.ExitError so the outcome tables can express
// a specific exit status without launching a process.
type fakeExitError struct{ code int }

func (e *fakeExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func TestGoTest_NoSandboxRoot(t *testing.T) {
	tool := &goTestTool{}
	res := tool.Execute(context.TODO(), CallInfo{}, map[string]any{
		"packages": []any{"./..."},
	})
	if !strings.Contains(res.Error, "no sandbox root") {
		t.Errorf("expected sandbox error, got %q", res.Error)
	}
}

func TestGoTest_EmptyPackages(t *testing.T) {
	tool := &goTestTool{}
	dir := t.TempDir()
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}}, map[string]any{
		"packages": []any{},
	})
	if !strings.Contains(res.Error, "packages must be a non-empty") {
		t.Errorf("expected packages error, got %q", res.Error)
	}
}

func TestFormatTestFailures_AllPass(t *testing.T) {
	ndjson := `{"Action":"pass","Package":"example.com/foo","Test":"TestA","Elapsed":0.01}` + "\n"
	result := formatTestFailures(ndjson)
	if result != "" {
		t.Errorf("expected empty result for all-pass, got %q", result)
	}
}

func TestFormatTestFailures_OneFail(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"Action":"output","Package":"example.com/foo","Test":"TestB","Output":"--- FAIL: TestB\n"}`,
		`{"Action":"fail","Package":"example.com/foo","Test":"TestB","Elapsed":0.01}`,
	}, "\n") + "\n"
	result := formatTestFailures(ndjson)
	if !strings.Contains(result, "TestB") {
		t.Errorf("expected TestB in failure summary, got %q", result)
	}
}

func TestFormatTestFailures_PackageFail(t *testing.T) {
	ndjson := `{"Action":"fail","Package":"example.com/foo","Elapsed":0.01}` + "\n"
	result := formatTestFailures(ndjson)
	if !strings.Contains(result, "FAIL example.com/foo") {
		t.Errorf("expected package fail line, got %q", result)
	}
}

func TestFormatTestFailures_SkipsNonJSON(t *testing.T) {
	ndjson := "not json\n" + `{"Action":"fail","Package":"example.com/bar","Test":"TestX","Elapsed":0.01}` + "\n"
	result := formatTestFailures(ndjson)
	if !strings.Contains(result, "TestX") {
		t.Errorf("expected TestX in output despite non-json line, got %q", result)
	}
}

// A failing test run must be a failed tool result. Returning the summary as
// Content made metrics count the call a success, showed the model plain
// content instead of an ERROR, and stopped the governor's tee-on-failure from
// firing — all three key on Result.Error.
func TestGoTestResult(t *testing.T) {
	failJSON := `{"Action":"fail","Package":"p","Test":"TestX"}`

	tests := []struct {
		name       string
		runErr     error
		raw        string
		all        bool
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "passing run",
			raw:        `{"Action":"pass","Package":"p","Test":"TestX"}`,
			wantSubstr: "All tests passed.",
		},
		{
			name:       "failing run with a parsed summary",
			runErr:     &fakeExitError{code: 1},
			raw:        failJSON,
			wantErr:    true,
			wantSubstr: "TestX",
		},
		{
			name:       "failing run with nothing parseable",
			runErr:     &fakeExitError{code: 2},
			raw:        "build failed: syntax error",
			wantErr:    true,
			wantSubstr: "build failed",
		},
		{
			name:       "green exit contradicted by reported failures",
			raw:        failJSON,
			wantErr:    true,
			wantSubstr: "exited 0 but reported test failures",
		},
		{
			name:       "all mode passing",
			raw:        "raw output",
			all:        true,
			wantSubstr: "raw output",
		},
		{
			name:       "all mode failing",
			runErr:     &fakeExitError{code: 1},
			raw:        "raw output",
			all:        true,
			wantErr:    true,
			wantSubstr: "raw output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := goTestResult(tt.runErr, tt.raw, tt.all)
			if tt.wantErr {
				if res.Error == "" {
					t.Fatalf("Error empty, want a failure (content %q)", res.Content)
				}
				if res.Content != "" {
					t.Errorf("Content = %q alongside an error; a failed run must not look successful", res.Content)
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

// End to end against the real go tool, so the wiring is covered and not just
// the decision table.
func TestGoTest_FailingSuiteReportsError(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	write("go.mod", "module probe\n\ngo 1.25\n")
	write("probe_test.go", "package probe\n\nimport \"testing\"\n\nfunc TestDeliberateFailure(t *testing.T) {\n\tt.Fatal(\"boom\")\n}\n")

	ci := CallInfo{SandboxRoots: []string{dir}}
	res := (&goTestTool{}).Execute(context.Background(), ci, map[string]any{"packages": []any{"./..."}})

	if res.Error == "" {
		t.Fatalf("a failing suite produced no error (content %q)", res.Content)
	}
	if !strings.Contains(res.Error, "TestDeliberateFailure") {
		t.Errorf("error does not name the failing test:\n%s", res.Error)
	}
	if res.Content != "" {
		t.Errorf("Content = %q, want empty for a failed run", res.Content)
	}
}

func TestGoTest_PassingSuiteReportsSuccess(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	write("go.mod", "module probe\n\ngo 1.25\n")
	write("probe_test.go", "package probe\n\nimport \"testing\"\n\nfunc TestPasses(t *testing.T) {}\n")

	ci := CallInfo{SandboxRoots: []string{dir}}
	res := (&goTestTool{}).Execute(context.Background(), ci, map[string]any{"packages": []any{"./..."}})

	if res.Error != "" {
		t.Fatalf("a passing suite produced an error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "All tests passed.") {
		t.Errorf("Content = %q, want the pass message", res.Content)
	}
}

// A summarized failure replaces the raw NDJSON rather than truncating it, so
// the raw must be preserved however small it is. Keying preservation on "was
// the inline text cut" meant an under-cap run lost its raw output entirely: B3
// spilled the summary under the name "full output", or skipped the spill when
// the summary fell below its threshold.
func TestGoTestResult_SummarizedFailurePreservesRawBelowTheCap(t *testing.T) {
	// Raw well under the inline cap, with a summary far shorter than it.
	var raw strings.Builder
	raw.WriteString(`{"Action":"fail","Package":"p","Test":"TestX"}` + "\n")
	for i := range 200 {
		fmt.Fprintf(&raw, `{"Action":"output","Package":"p","Test":"TestOther","Output":"noise line %d\n"}`+"\n", i)
	}
	rawStr := raw.String()
	if len(rawStr) >= goTestOutputLimit {
		t.Fatalf("raw is %d bytes, must stay under the %d-byte cap for this case", len(rawStr), goTestOutputLimit)
	}

	res := goTestResult(&fakeExitError{code: 1}, rawStr, false)

	if res.Error == "" {
		t.Fatal("expected a failure result")
	}
	if res.FullOutput != rawStr {
		t.Errorf("FullOutput = %d bytes, want the raw %d — the summary replaced it and the raw was dropped",
			len(res.FullOutput), len(rawStr))
	}
	if len(res.Error) >= len(rawStr) {
		t.Errorf("inline error (%d bytes) is not shorter than raw (%d); this case is meant to summarize",
			len(res.Error), len(rawStr))
	}
}

// The scanner must keep reading past a record larger than its default token
// size, or every failure after a long line silently disappears.
func TestFormatTestFailures_LongRecordDoesNotHideLaterFailures(t *testing.T) {
	huge := strings.Repeat("z", 128*1024)
	raw := fmt.Sprintf(`{"Action":"output","Package":"p","Test":"TestBig","Output":%q}`+"\n", huge) +
		`{"Action":"fail","Package":"p","Test":"TestAfterTheLongRecord"}` + "\n"

	summary := formatTestFailures(raw)
	if !strings.Contains(summary, "TestAfterTheLongRecord") {
		t.Errorf("failure after a long record is missing from the summary:\n%s", summary)
	}
}

// When the stream genuinely cannot be read to the end, the summary has to say
// so: a short list would under-report failures and an empty one would read as
// a pass.
func TestFormatTestFailures_ReportsUnreadableStream(t *testing.T) {
	// A record beyond even the raised limit.
	oversize := strings.Repeat("q", spillCeiling+scannerHeadroom+1)
	raw := fmt.Sprintf(`{"Action":"output","Package":"p","Test":"T","Output":%q}`+"\n", oversize)

	summary := formatTestFailures(raw)
	if !strings.Contains(summary, "could not be fully parsed") {
		t.Errorf("unreadable stream produced %q, want an explicit warning", summary)
	}
}
