package rootfs

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestAppendSync_PreservesExistingContents(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if err := root.AppendSync("log.txt", []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("append clobbered existing contents: got %q, want %q", string(got), "first\nsecond\n")
	}
}

func TestAppendSync_AppendsExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	rec := "record-one\n"
	if err := root.AppendSync("sessions.jsonl", []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := root.AppendSync("sessions.jsonl", []byte("record-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := "record-one\nrecord-two\n"
	if string(got) != want {
		t.Errorf("each append must land exactly once: got %q, want %q", string(got), want)
	}
}

func TestAppendSync_WriteErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	injected := errors.New("injected write failure")
	err = root.appendSync("log.txt", []byte("new"), 0o644, appendHooks{
		Write: func(f *os.File, data []byte) error { return injected },
	})
	if !errors.Is(err, injected) {
		t.Fatalf("write failure must propagate, got %v", err)
	}
	// The failed append must not truncate, remove, or shorten the file.
	got, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing\n" {
		t.Errorf("failed append altered the file: got %q, want %q", string(got), "existing\n")
	}
}

func TestAppendSync_SyncErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	injected := errors.New("injected sync failure")
	err = root.appendSync("log.txt", []byte("data"), 0o644, appendHooks{
		Sync: func(f *os.File) error { return injected },
	})
	if !errors.Is(err, injected) {
		t.Fatalf("sync failure must propagate, got %v", err)
	}
}

// AppendSync must never attempt cleanup by name after a failed append: a name
// whose ownership may have changed since the open is not this call's to remove
// or shorten. The pre-existing content must survive an injected failure.
func TestAppendSync_FailedAppendDoesNotCleanUpByName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), []byte("durable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	injected := errors.New("injected write failure")
	if err := root.appendSync("log.txt", []byte("new"), 0o644, appendHooks{
		Write: func(f *os.File, data []byte) error { return injected },
	}); !errors.Is(err, injected) {
		t.Fatalf("write failure must propagate, got %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatalf("failed append must not remove the log: %v", err)
	}
	if string(got) != "durable\n" {
		t.Errorf("failed append altered the log: got %q, want %q", string(got), "durable\n")
	}
}

func TestAppendSync_LinkedOutOfRootLeavesOutsideUnchanged(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	const outsideBody = "outsider\n"
	if err := os.WriteFile(filepath.Join(outside, "sessions.jsonl"), []byte(outsideBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "sessions.jsonl"), filepath.Join(repoRoot, "sessions.jsonl")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("file symlinks require Developer Mode on Windows")
		}
		t.Fatal(err)
	}

	root, err := Open(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	err = root.AppendSync("sessions.jsonl", []byte("record\n"), 0o644)
	if err == nil {
		t.Fatal("append followed a link out of the root")
	}
	got, err := os.ReadFile(filepath.Join(outside, "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != outsideBody {
		t.Errorf("linked-outside file was modified: got %q, want %q", string(got), outsideBody)
	}
}

// The append primitive must not expose a truncation-capable spelling of an
// open: the only append open in the package carries O_WRONLY|O_CREATE|O_APPEND
// and nothing else. A behavior-level test cannot prove this (an unused O_TRUNC
// variant would pass), so the compiled source is inspected.
func TestAppendSync_NoTruncationCapableAPIExposed(t *testing.T) {
	fset := token.NewFileSet()
	var packageFiles []*ast.File
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		packageFiles = append(packageFiles, f)
	}

	// No os.O_TRUNC may appear in the package's code. A truncation-capable
	// open is the one spelling that could shorten an append-only log.
	var appendFlags []string
	for _, f := range packageFiles {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "os" && sel.Sel.Name == "O_TRUNC" {
				t.Error("rootfs references os.O_TRUNC")
			}
			return true
		})
		// Every append open in the package must be exactly
		// O_WRONLY|O_CREATE|O_APPEND and nothing else. Find every OpenFile call
		// whose flags carry O_APPEND and require the exact set on each.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "OpenFile" || len(call.Args) < 2 {
				return true
			}
			flags := collectOSFlags(call.Args[1])
			if slices.Contains(flags, "O_APPEND") {
				appendFlags = flags
			}
			return true
		})
	}
	if appendFlags == nil {
		t.Fatal("no append open found in rootfs")
	}
	slices.Sort(appendFlags)
	if want := []string{"O_APPEND", "O_CREATE", "O_WRONLY"}; !slices.Equal(appendFlags, want) {
		t.Errorf("append open flags = %v, want exactly %v", appendFlags, want)
	}
}

// collectOSFlags returns the os.<FLAG> constant names referenced inside expr.
func collectOSFlags(expr ast.Expr) []string {
	var out []string
	ast.Inspect(expr, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "os" {
			out = append(out, sel.Sel.Name)
		}
		return true
	})
	return out
}

// The append primitive reads nothing: a failed open (missing parent directory)
// must surface as fs.ErrNotExist so callers can distinguish "no file yet" from
// a real I/O failure without the primitive doing its own parent scaffolding.
func TestAppendSync_MissingParentReturnsErrNotExist(t *testing.T) {
	root, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	err = root.AppendSync("a/b/log.txt", []byte("data"), 0o644)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("append into a missing parent: errors.Is(err, fs.ErrNotExist) = %v, err = %v", errors.Is(err, fs.ErrNotExist), err)
	}
}
