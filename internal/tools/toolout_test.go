package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The spill directory lives outside every sandbox root, so resolution cannot go
// through validatePath. That makes the id the only barrier between a crafted
// handle and an arbitrary file, and it has to hold on its own.
func TestResolveToolout_RejectsAnythingButHex(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		locator string
	}{
		{name: "parent traversal", locator: TooloutScheme + ".."},
		{name: "traversal with separator", locator: TooloutScheme + "../../etc/passwd"},
		{name: "windows separator", locator: TooloutScheme + `..\..\secrets`},
		{name: "absolute unix path", locator: TooloutScheme + "/etc/passwd"},
		{name: "windows absolute path", locator: TooloutScheme + `C:\Windows\System32\config\SAM`},
		{name: "nested path", locator: TooloutScheme + "sub/dir"},
		{name: "dot", locator: TooloutScheme + "."},
		{name: "uppercase hex", locator: TooloutScheme + "ABCDEF"},
		{name: "non-hex letters", locator: TooloutScheme + "zzzz"},
		{name: "hex with dash", locator: TooloutScheme + "ab-cd"},
		{name: "hex with null byte", locator: TooloutScheme + "abcd\x00ef"},
		{name: "empty id", locator: TooloutScheme},
		{name: "over-long id", locator: TooloutScheme + strings.Repeat("a", tooloutIDMaxLen+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveToolout(dir, tt.locator)
			if err == nil {
				t.Fatalf("resolved %q to %q, want a rejection", tt.locator, got)
			}
			if got != "" {
				t.Errorf("returned path %q alongside an error", got)
			}
		})
	}
}

func TestResolveToolout_AcceptsGeneratedIDs(t *testing.T) {
	dir := t.TempDir()
	// The shape B3 emits: 16 lowercase hex characters.
	const id = "1a2b3c4d5e6f7890"

	got, err := resolveToolout(dir, TooloutScheme+id)
	if err != nil {
		t.Fatalf("resolveToolout: %v", err)
	}
	if want := filepath.Join(dir, id); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveToolout_UnconfiguredDirectoryRefuses(t *testing.T) {
	_, err := resolveToolout("", TooloutScheme+"abcd")
	if !errors.Is(err, ErrTooloutUnavailable) {
		t.Errorf("err = %v, want ErrTooloutUnavailable", err)
	}
}

// writeSpill puts content in the spill directory under the id B3 would use.
func writeSpill(t *testing.T, dir, id, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// B3 emitted a handle no tool resolved, so the preserved output could not be
// reached at all. read now serves it.
func TestRead_ResolvesTooloutHandle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "00ff11ee22dd33cc"
	writeSpill(t, dir, id, "line one\nline two\nline three\n")

	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "line two") {
		t.Errorf("spilled output not returned:\n%s", res.Content)
	}
}

func TestRead_TooloutHonoursLineRange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "abcdef0123456789"
	writeSpill(t, dir, id, "alpha\nbravo\ncharlie\ndelta\n")

	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}
	res := (&readTool{}).Execute(context.Background(), ci, map[string]any{
		"locator":    TooloutScheme + id,
		"start_line": 2,
		"end_line":   3,
	})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if got := res.Content; got != "bravo\ncharlie\n" {
		t.Errorf("Content = %q, want the requested span", got)
	}
}

// A spill can be megabytes. Returning one whole would put back into context
// exactly the volume the tee exists to keep out of it.
func TestRead_TooloutPagesLargeSpills(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolout")
	const id = "0123456789abcdef"
	writeSpill(t, dir, id, strings.Repeat("a line of spilled output\n", 4000))

	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: dir}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + id})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if len(res.Content) > tooloutPageLimit*2 {
		t.Errorf("returned %d bytes for a large spill; the page limit is %d", len(res.Content), tooloutPageLimit)
	}
	if !strings.Contains(res.Content, "start_line") {
		t.Errorf("paged output does not say how to request the rest:\n…%s",
			res.Content[max(0, len(res.Content)-200):])
	}
}

func TestRead_TooloutMissingFile(t *testing.T) {
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, TooloutDir: t.TempDir()}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": TooloutScheme + "deadbeefdeadbeef"})

	if res.Error == "" {
		t.Fatal("expected an error for a handle with no file")
	}
	if !strings.Contains(res.Error, "no longer exists") {
		t.Errorf("error = %q, want it to explain the spill is cached", res.Error)
	}
}

// A path-shaped locator must still behave as before.
func TestRead_OrdinaryLocatorUnaffected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ci := CallInfo{SandboxRoots: []string{root}}
	res := (&readTool{}).Execute(context.Background(), ci,
		map[string]any{"locator": FormatLocator(path, 2, 3)})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Content != "two\nthree\n" {
		t.Errorf("Content = %q", res.Content)
	}
}
