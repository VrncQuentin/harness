package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead_WithinSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &readTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}}, map[string]any{"path": filepath.Join(dir, "hello.txt")})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Content != "hello world" {
		t.Fatalf("got %q, want %q", res.Content, "hello world")
	}
}

func TestRead_OutsideSandbox(t *testing.T) {
	dir := t.TempDir()
	tool := &readTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}}, map[string]any{"path": "/etc/hosts"})
	if !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("expected sandbox error, got %q", res.Error)
	}
}

func TestRead_MissingPath(t *testing.T) {
	dir := t.TempDir()
	tool := &readTool{}
	res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}}, map[string]any{"path": filepath.Join(dir, "missing.txt")})
	if !strings.Contains(res.Error, "not found") {
		t.Fatalf("expected not-found error, got %q", res.Error)
	}
}

func TestRead_Addressing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		args        map[string]any
		wantContent string
		wantErr     string
	}{
		{
			name:        "whole file by path",
			args:        map[string]any{"path": path},
			wantContent: "one\ntwo\nthree\nfour\n",
		},
		{
			name:        "range by start and end line",
			args:        map[string]any{"path": path, "start_line": float64(2), "end_line": float64(3)},
			wantContent: "two\nthree\n",
		},
		{
			name:        "range by locator",
			args:        map[string]any{"locator": FormatLocator(path, 2, 2)},
			wantContent: "two\n",
		},
		{
			name:        "locator wins over path",
			args:        map[string]any{"path": filepath.Join(dir, "missing.txt"), "locator": FormatLocator(path, 4, 4)},
			wantContent: "four\n",
		},
		{
			name:    "range past end of file",
			args:    map[string]any{"path": path, "start_line": float64(3), "end_line": float64(99)},
			wantErr: "exceeds file length",
		},
		{
			name:    "inverted range",
			args:    map[string]any{"path": path, "start_line": float64(3), "end_line": float64(2)},
			wantErr: "invalid line range",
		},
		{
			name:    "bad locator",
			args:    map[string]any{"locator": "nope"},
			wantErr: "invalid locator",
		},
		{
			name:    "no target",
			args:    map[string]any{},
			wantErr: "missing path or locator",
		},
	}
	tool := &readTool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := tool.Execute(context.TODO(), CallInfo{SandboxRoots: []string{dir}}, tt.args)
			if tt.wantErr != "" {
				if res.Error == "" || !strings.Contains(res.Error, tt.wantErr) {
					t.Fatalf("Execute error = %q, want substring %q", res.Error, tt.wantErr)
				}
				return
			}
			if res.Error != "" {
				t.Fatalf("Execute error: %s", res.Error)
			}
			if res.Content != tt.wantContent {
				t.Fatalf("Content = %q, want %q", res.Content, tt.wantContent)
			}
		})
	}
}

func TestParseLocator(t *testing.T) {
	tests := []struct {
		in        string
		wantPath  string
		wantStart int
		wantEnd   int
		wantErr   bool
	}{
		{in: `C:\repo\a.go:3-9`, wantPath: `C:\repo\a.go`, wantStart: 3, wantEnd: 9},
		{in: "rel/path.txt:1-1", wantPath: "rel/path.txt", wantStart: 1, wantEnd: 1},
		{in: "no-range", wantErr: true},
		{in: "a.go:3-", wantErr: true},
		{in: "a.go:x-9", wantErr: true},
		{in: "a.go:9-3", wantErr: true},
		{in: "a.go:0-3", wantErr: true},
		{in: ":3-9", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			path, start, end, err := ParseLocator(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLocator(%q) = %q,%d,%d, want error", tt.in, path, start, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLocator(%q): %v", tt.in, err)
			}
			if path != tt.wantPath || start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("ParseLocator(%q) = %q,%d,%d, want %q,%d,%d", tt.in, path, start, end, tt.wantPath, tt.wantStart, tt.wantEnd)
			}
		})
	}
}
