package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newEditTool(t *testing.T) *editTool {
	t.Helper()
	return &editTool{parsers: newASTTestRegistry(t)}
}

func anchorFor(t *testing.T, path string, start, end int) (locator, hash string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	hash, err = SpanHash(src, start, end)
	if err != nil {
		t.Fatalf("SpanHash: %v", err)
	}
	return FormatLocator(path, start, end), hash
}

func TestEdit_AnchoredReplace(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	tool := newEditTool(t)
	locator, hash := anchorFor(t, path, 3, 5) // func Alpha

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"locator":     locator,
		"anchor_hash": hash,
		"content":     "func Alpha() int {\n\treturn 42\n}\n",
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	for _, want := range []string{"edited", ":3-5", "h:", "content OK", "parse OK"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("Content missing %q:\n%s", want, res.Content)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(after), "return 42") {
		t.Fatalf("edit did not land:\n%s", after)
	}
	if !strings.Contains(string(after), "func Beta") {
		t.Fatalf("edit clobbered the rest of the file:\n%s", after)
	}
}

func TestEdit_AnchorMismatchRejected(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	tool := newEditTool(t)
	locator, _ := anchorFor(t, path, 3, 5)

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"locator":     locator,
		"anchor_hash": "h:0000000000000000",
		"content":     "func Alpha() int { return 0 }\n",
	})
	if res.Error == "" || !strings.Contains(res.Error, "anchor hash mismatch") {
		t.Fatalf("Execute error = %q, want anchor hash mismatch", res.Error)
	}
	after, _ := os.ReadFile(path)
	if string(after) != astToolsSrc {
		t.Fatal("rejected edit still modified the file")
	}
}

func TestEdit_DeleteSpan(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "notes.txt", "one\ntwo\nthree\n")
	tool := newEditTool(t)
	locator, hash := anchorFor(t, path, 2, 2)

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"locator": locator, "anchor_hash": hash, "content": "",
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "span deleted") {
		t.Errorf("Content missing deletion note:\n%s", res.Content)
	}
	after, _ := os.ReadFile(path)
	if string(after) != "one\nthree\n" {
		t.Fatalf("file after deletion = %q", after)
	}
}

func TestEdit_ReplacementGainsNewlineWhenLinesFollow(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "notes.txt", "one\ntwo\nthree\n")
	tool := newEditTool(t)
	locator, hash := anchorFor(t, path, 2, 2)

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"locator": locator, "anchor_hash": hash, "content": "TWO", // no trailing newline
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	after, _ := os.ReadFile(path)
	if string(after) != "one\nTWO\nthree\n" {
		t.Fatalf("file = %q, want fused newline protection", after)
	}
}

func TestEdit_ParseWarningOnBrokenResult(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	tool := newEditTool(t)
	locator, hash := anchorFor(t, path, 3, 5)

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"locator": locator, "anchor_hash": hash, "content": "func Alpha( {\n",
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "no longer parses") {
		t.Fatalf("Content missing parse warning:\n%s", res.Content)
	}
}

func TestEdit_CreateNewFile(t *testing.T) {
	root := t.TempDir()
	tool := newEditTool(t)
	path := filepath.Join(root, "sub", "new.txt")

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"path": path, "content": "hello\n",
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "created") {
		t.Errorf("Content missing created:\n%s", res.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("created file = %q, err %v", data, err)
	}
}

func TestEdit_Errors(t *testing.T) {
	root := t.TempDir()
	existing := writeSandboxFile(t, root, "exists.txt", "x\n")
	tool := newEditTool(t)
	outside := filepath.Join(t.TempDir(), "out.txt")

	tests := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{name: "whole-file mode rejects existing file", args: map[string]any{"path": existing, "content": "y\n"}, wantErr: "already exists"},
		{name: "anchored without hash", args: map[string]any{"locator": existing + ":1-1", "content": "y\n"}, wantErr: "anchor_hash is required"},
		{name: "bad locator", args: map[string]any{"locator": "nope", "anchor_hash": "h:x", "content": "y"}, wantErr: "invalid locator"},
		{name: "missing content", args: map[string]any{"path": existing}, wantErr: "missing content"},
		{name: "no target", args: map[string]any{"content": "y"}, wantErr: "need either locator"},
		{name: "outside sandbox", args: map[string]any{"path": outside, "content": "y"}, wantErr: "outside sandbox"},
		{name: "missing file for locator", args: map[string]any{"locator": filepath.Join(root, "gone.txt") + ":1-1", "anchor_hash": "h:1", "content": "y"}, wantErr: "not found"},
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
