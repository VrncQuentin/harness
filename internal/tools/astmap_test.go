package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vrnc/harness/internal/parser"
)

func newASTTestRegistry(t *testing.T) *parser.Registry {
	t.Helper()
	reg, err := parser.NewRegistry(parser.NewGoFrontEnd())
	if err != nil {
		t.Fatalf("parser.NewRegistry: %v", err)
	}
	return reg
}

func writeSandboxFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const astToolsSrc = `package sample

func Alpha() int {
	return 1
}

func Beta() int {
	return 2
}
`

func TestAstMap_OutlinesGoFile(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	tool := &astMapTool{parsers: newASTTestRegistry(t)}

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{"path": path})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	if res.Origin != OriginExtraction {
		t.Errorf("Origin = %q, want %q", res.Origin, OriginExtraction)
	}
	for _, want := range []string{"go file — 2 symbols", "[func] Alpha", "[func] Beta", "h:", ":3-5"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("Content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestAstMap_Errors(t *testing.T) {
	root := t.TempDir()
	goFile := writeSandboxFile(t, root, "ok.go", astToolsSrc)
	txtFile := writeSandboxFile(t, root, "notes.txt", "hello\n")
	broken := writeSandboxFile(t, root, "broken.go", "package broken\nfunc f( {\n")
	outside := filepath.Join(t.TempDir(), "outside.go")

	tool := &astMapTool{parsers: newASTTestRegistry(t)}
	tests := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{name: "missing path", args: map[string]any{}, wantErr: "missing or invalid path"},
		{name: "outside sandbox", args: map[string]any{"path": outside}, wantErr: "outside sandbox"},
		{name: "unsupported language", args: map[string]any{"path": txtFile}, wantErr: "unsupported file type"},
		{name: "syntax error surfaces", args: map[string]any{"path": broken}, wantErr: "ast_map:"},
		{name: "missing file", args: map[string]any{"path": filepath.Join(root, "gone.go")}, wantErr: "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, tt.args)
			if res.Error == "" || !strings.Contains(res.Error, tt.wantErr) {
				t.Fatalf("Execute error = %q, want substring %q", res.Error, tt.wantErr)
			}
		})
	}
	// Sanity: the valid file still works after the error table ran.
	if res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{"path": goFile}); res.Error != "" {
		t.Fatalf("valid file errored: %s", res.Error)
	}
}
