package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestAstFind_SymbolMode(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	tool := &astFindTool{parsers: newASTTestRegistry(t)}

	tests := []struct {
		name        string
		query       string
		wantContent []string
	}{
		{
			name:        "exact match",
			query:       "Alpha",
			wantContent: []string{"1 symbol match(es)", "[func] Alpha", "h:", ":3-5"},
		},
		{
			name:        "substring fallback",
			query:       "alph",
			wantContent: []string{"1 symbol match(es)", "[func] Alpha"},
		},
		{
			name:        "no match reports outline hint",
			query:       "Gamma",
			wantContent: []string{"no symbol matching", "ast_map"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{"path": path, "query": tt.query})
			if res.Error != "" {
				t.Fatalf("Execute error: %s", res.Error)
			}
			if res.Origin != OriginExtraction {
				t.Errorf("Origin = %q, want %q", res.Origin, OriginExtraction)
			}
			for _, want := range tt.wantContent {
				if !strings.Contains(res.Content, want) {
					t.Errorf("Content missing %q:\n%s", want, res.Content)
				}
			}
		})
	}
}

func TestAstFind_ContentMode(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "notes.txt", "alpha\nbeta target line\ngamma\nanother target here\n")
	tool := &astFindTool{parsers: newASTTestRegistry(t)}

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"path": path, "query": "target", "mode": "content",
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	for _, want := range []string{"2 content match(es)", ":2-2", ":4-4", "h:", "beta target line"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("Content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestAstFind_ContentModeHashMatchesSpanHash(t *testing.T) {
	root := t.TempDir()
	src := "alpha\nbeta target line\n"
	path := writeSandboxFile(t, root, "notes.txt", src)
	tool := &astFindTool{parsers: newASTTestRegistry(t)}

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"path": path, "query": "target", "mode": "content",
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	want, err := SpanHash([]byte(src), 2, 2)
	if err != nil {
		t.Fatalf("SpanHash: %v", err)
	}
	if !strings.Contains(res.Content, want) {
		t.Fatalf("Content missing recomputable hash %q:\n%s", want, res.Content)
	}
}

func TestAstFind_Errors(t *testing.T) {
	root := t.TempDir()
	goFile := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	txtFile := writeSandboxFile(t, root, "notes.txt", "text\n")
	tool := &astFindTool{parsers: newASTTestRegistry(t)}

	tests := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{name: "missing query", args: map[string]any{"path": goFile}, wantErr: "missing or invalid query"},
		{name: "symbol mode on unsupported file", args: map[string]any{"path": txtFile, "query": "x"}, wantErr: "use content mode"},
		{name: "multi-line content query", args: map[string]any{"path": txtFile, "query": "a\nb", "mode": "content"}, wantErr: "single-line"},
		{name: "invalid mode", args: map[string]any{"path": txtFile, "query": "x", "mode": "regex"}, wantErr: "invalid mode"},
		{name: "outside sandbox", args: map[string]any{"path": filepath.Join(t.TempDir(), "out.go"), "query": "x"}, wantErr: "outside sandbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, tt.args)
			if res.Error == "" || !strings.Contains(res.Error, tt.wantErr) {
				t.Fatalf("Execute error = %q, want substring %q", res.Error, tt.wantErr)
			}
		})
	}
}
