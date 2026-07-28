package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// watched maps a canonical package path to the set of symbols whose direct
// calls must be classified in the allowlist. This table is compiled into the
// scanner — it does not come from the allowlist — so removing an entry here
// requires a code change with review.
var watched = map[string][]string{
	"os": {
		"Open", "OpenFile", "OpenRoot",
		"ReadFile", "WriteFile",
		"ReadDir",
		"Create", "CreateTemp",
		"Rename",
		"Remove", "RemoveAll",
		"Mkdir", "MkdirAll",
		"Lstat", "Stat",
		"SameFile",
		"Truncate",
		"Link", "Symlink",
		"Readlink",
		"Chmod", "Chown", "Chtimes",
	},
	"path/filepath": {
		"Walk", "WalkDir",
		"Glob",
		"EvalSymlinks",
	},
}

type entry struct {
	File          string `json:"file"`
	Line          int    `json:"line"`
	Fn            string `json:"fn"`
	Justification string `json:"justification,omitempty"`
	PR            string `json:"pr,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

type allowlist struct {
	Perm []entry `json:"permanent"`
	Migr []entry `json:"migration"`
}

type call struct {
	File string
	Line int
	Fn   string
}

func main() {
	alPath := "cmd/fsaudit/allowlist.json"
	if len(os.Args) > 1 {
		alPath = os.Args[1]
	}
	data, err := os.ReadFile(alPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsaudit: cannot read allowlist: %v\n", err)
		os.Exit(1)
	}
	var al allowlist
	if err := json.Unmarshal(data, &al); err != nil {
		fmt.Fprintf(os.Stderr, "fsaudit: cannot parse allowlist: %v\n", err)
		os.Exit(1)
	}

	allEntries := append([]entry(nil), al.Perm...)
	allEntries = append(allEntries, al.Migr...)

	seenEntry := map[string]bool{}
	for _, e := range allEntries {
		k := entryKey(e)
		if seenEntry[k] {
			fmt.Fprintf(os.Stderr, "fsaudit: duplicate allowlist entry: %s %s:%d %s\n", classify(e), e.File, e.Line, e.Fn)
			os.Exit(1)
		}
		seenEntry[k] = true
		if e.Justification == "" && e.PR == "" {
			fmt.Fprintf(os.Stderr, "fsaudit: allowlist entry has neither justification nor pr: %s:%d %s\n", e.File, e.Line, e.Fn)
			os.Exit(1)
		}
		if e.Justification != "" && e.PR != "" {
			fmt.Fprintf(os.Stderr, "fsaudit: allowlist entry has both justification and pr: %s:%d %s\n", e.File, e.Line, e.Fn)
			os.Exit(1)
		}
	}

	sourceCalls, err := collectCalls(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsaudit: scan error: %v\n", err)
		os.Exit(1)
	}

	allowed := map[string]bool{}
	for _, e := range allEntries {
		allowed[entryKey(e)] = true
	}

	matched := map[string]bool{}
	var unclassified []call
	for _, c := range sourceCalls {
		k := entryKey(entry{File: c.File, Line: c.Line, Fn: c.Fn})
		if allowed[k] {
			matched[k] = true
		} else {
			unclassified = append(unclassified, c)
		}
	}

	var stale []entry
	for _, e := range allEntries {
		if !matched[entryKey(e)] {
			stale = append(stale, e)
		}
	}

	exit := false
	if len(stale) > 0 {
		fmt.Fprintf(os.Stderr, "fsaudit: %d stale allowlist entry(ies) — no matching source call found:\n\n", len(stale))
		for _, e := range stale {
			fmt.Fprintf(os.Stderr, "  %s %s:%d %s\n", classify(e), e.File, e.Line, e.Fn)
		}
		fmt.Fprintf(os.Stderr, "\n")
		exit = true
	}
	if len(unclassified) > 0 {
		fmt.Fprintf(os.Stderr, "fsaudit: %d unclassified direct filesystem call(s):\n\n", len(unclassified))
		counts := map[string]int{}
		for _, c := range unclassified {
			counts[c.Fn]++
			fmt.Fprintf(os.Stderr, "  %s:%d  %s\n", c.File, c.Line, c.Fn)
		}
		fmt.Fprintf(os.Stderr, "\n")
		for fn, n := range counts {
			fmt.Fprintf(os.Stderr, "  %d × %s\n", n, fn)
		}
		fmt.Fprintf(os.Stderr, "\nAdd to cmd/fsaudit/allowlist.json:\n")
		fmt.Fprintf(os.Stderr, "  - permanent entry if this is an intentional boundary exception\n")
		fmt.Fprintf(os.Stderr, "  - migration entry if this will be routed through rootfs in a future PR\n")
		exit = true
	}
	if exit {
		os.Exit(1)
	}
	fmt.Println("fsaudit: all direct filesystem calls are accounted for in the allowlist")
}

func entryKey(e entry) string { return fmt.Sprintf("%s:%d:%s", e.File, e.Line, e.Fn) }

func classify(e entry) string {
	if e.PR != "" {
		return "migration"
	}
	return "permanent"
}

func collectCalls(root string) ([]call, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var calls []call
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "cmd/fsaudit/") {
			return nil
		}
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "cmd/") {
			return nil
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		imports := resolveImports(f)

		ast.Inspect(f, func(n ast.Node) bool {
			callExpr, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			canon, match := resolveCall(callExpr, imports)
			if !match {
				return true
			}
			pos := fset.Position(callExpr.Pos())
			if pos.Line == 0 {
				return true
			}
			calls = append(calls, call{
				File: rel,
				Line: pos.Line,
				Fn:   canon,
			})
			return true
		})
		return nil
	})
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].File != calls[j].File {
			return calls[i].File < calls[j].File
		}
		if calls[i].Line != calls[j].Line {
			return calls[i].Line < calls[j].Line
		}
		return calls[i].Fn < calls[j].Fn
	})
	return calls, err
}

func resolveImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

func resolveCall(call *ast.CallExpr, imports map[string]string) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	pkgPath, ok := imports[pkgIdent.Name]
	if !ok {
		return "", false
	}
	symbols, ok := watched[pkgPath]
	if !ok {
		return "", false
	}
	if !slices.Contains(symbols, sel.Sel.Name) {
		return "", false
	}
	return filepath.Base(pkgPath) + "." + sel.Sel.Name, true
}
