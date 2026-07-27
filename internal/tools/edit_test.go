package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// The write goes through a temporary file and a rename inside the pinned root,
// so the directory must be left holding the edited file and nothing else. A
// stray .harness-write-* would mean the rename did not consume it, and would
// then show up in git status and in every file_list of the directory.
func TestEdit_LeavesNoTemporaryFileBehind(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	tool := newEditTool(t)
	locator, hash := anchorFor(t, path, 3, 5)
	ci := CallInfo{SandboxRoots: []string{root}}

	if res := tool.Execute(context.Background(), ci, map[string]any{
		"locator": locator, "anchor_hash": hash, "content": "func Alpha() int {\n\treturn 42\n}\n",
	}); res.Error != "" {
		t.Fatalf("anchored edit: %s", res.Error)
	}
	if res := tool.Execute(context.Background(), ci, map[string]any{
		"path": filepath.Join(root, "created.txt"), "content": "new\n",
	}); res.Error != "" {
		t.Fatalf("create: %s", res.Error)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want := []string{"created.txt", "sample.go"}
	if !slices.Equal(got, want) {
		t.Errorf("root contains %v, want exactly %v", got, want)
	}
}

// Verification re-reads through the same pinned root as the write, so an edit
// that reports success has been confirmed against the file it actually wrote.
func TestEdit_VerifiesAgainstTheWrittenFile(t *testing.T) {
	root := t.TempDir()
	path := writeSandboxFile(t, root, "sample.go", astToolsSrc)
	tool := newEditTool(t)
	locator, hash := anchorFor(t, path, 3, 5)

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}}, map[string]any{
		"locator": locator, "anchor_hash": hash, "content": "func Alpha() int {\n\treturn 42\n}\n",
	})
	if res.Error != "" {
		t.Fatalf("Execute error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "content OK") {
		t.Fatalf("Content missing the verification result:\n%s", res.Content)
	}
	// The reported anchor must address the bytes now on disk, or the next
	// anchored edit built on it would be rejected.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	newHash, err := SpanHash(after, 3, 5)
	if err != nil {
		t.Fatalf("SpanHash: %v", err)
	}
	if !strings.Contains(res.Content, newHash) {
		t.Errorf("reported hash does not match the file on disk (%s):\n%s", newHash, res.Content)
	}
}

// Whole-file mode claims to create only. A Stat followed by a rename does not
// deliver that: a file appearing between the two is replaced by the rename, and
// the caller is told it created a new file. The claim on the name and the
// existence check have to be one operation.
func TestEdit_CreateNeverOverwritesAConcurrentCreate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contended.txt")
	tool := newEditTool(t)
	ci := CallInfo{SandboxRoots: []string{root}}

	const writers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created []string
		refused int
	)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("writer-%d\n", i)
			res := tool.Execute(context.Background(), ci, map[string]any{"path": path, "content": body})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case res.Error == "":
				created = append(created, body)
			case strings.Contains(res.Error, "already exists"):
				refused++
			default:
				t.Errorf("unexpected error: %s", res.Error)
			}
		}()
	}
	wg.Wait()

	if len(created) != 1 {
		t.Fatalf("%d callers were told they created the file, want exactly 1: %q", len(created), created)
	}
	if refused != writers-1 {
		t.Errorf("%d callers were refused, want %d", refused, writers-1)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != created[0] {
		t.Errorf("file holds %q but %q was told it created it", got, created[0])
	}
}

// The refusal must leave the existing file exactly as it was — the point of
// refusing is the content on disk, not the message.
func TestEdit_CreateLeavesAnExistingFileUntouched(t *testing.T) {
	root := t.TempDir()
	existing := writeSandboxFile(t, root, "exists.txt", "theirs\n")
	tool := newEditTool(t)

	res := tool.Execute(context.Background(), CallInfo{SandboxRoots: []string{root}},
		map[string]any{"path": existing, "content": "mine\n"})
	if res.Error == "" {
		t.Fatal("whole-file mode overwrote an existing file")
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "theirs\n" {
		t.Errorf("file = %q, want the untouched %q", got, "theirs\n")
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
