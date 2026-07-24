package tools

import (
	"context"
	"strings"
	"testing"
)

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
