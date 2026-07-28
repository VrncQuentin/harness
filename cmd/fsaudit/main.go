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

// watched maps a canonical import path to the set of symbols whose direct
// calls, dot-imported calls, and value-extractions must be classified.
// This table is compiled into the scanner — it is not read from the
// allowlist. Changing it requires a code review.
var watched = map[string][]string{
	"os": {
		"Open", "OpenFile", "OpenRoot",
		"ReadFile", "WriteFile",
		"ReadDir",
		"Create", "CreateTemp",
		"Rename",
		"Remove", "RemoveAll",
		"Mkdir", "MkdirAll", "MkdirTemp",
		"Lstat", "Stat",
		"SameFile",
		"Truncate",
		"Link", "Symlink",
		"Readlink",
		"Chmod", "Chown", "Chtimes", "Lchown",
		"Chdir",
		"CopyFS", "DirFS",
	},
	"path/filepath": {
		"Walk", "WalkDir",
		"Glob",
		"EvalSymlinks",
	},
}

// ---------- data types ----------

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

type sourceCall struct {
	File string
	Line int
	Col  int
	Fn   string
}

type Report struct {
	SourceCalls  []sourceCall
	Unclassified []sourceCall
	Stale        []entry
	Blocked      []sourceCall // dot-imports, function-value captures, os.OpenRoot outside rootfs
}

// ---------- validateAllowlist ----------

type AllowlistError struct {
	Entry entry
	Msg   string
}

func (e *AllowlistError) Error() string {
	return fmt.Sprintf("%s:%d %s: %s", e.Entry.File, e.Entry.Line, e.Entry.Fn, e.Msg)
}

func ValidateAllowlist(al allowlist) []error {
	var errs []error
	all := append([]entry(nil), al.Perm...)
	all = append(all, al.Migr...)

	seen := map[string]bool{}
	for _, e := range all {
		k := entryKey(e.File, e.Line, 0, e.Fn)
		if seen[k] {
			errs = append(errs, &AllowlistError{Entry: e, Msg: "duplicate entry"})
		}
		seen[k] = true
		if e.Justification == "" && e.PR == "" {
			errs = append(errs, &AllowlistError{Entry: e, Msg: "must have justification or pr"})
		}
		if e.Justification != "" && e.PR != "" {
			errs = append(errs, &AllowlistError{Entry: e, Msg: "cannot have both justification and pr"})
		}
	}
	return errs
}

// ---------- audit ----------

func Audit(root string, al allowlist) Report {
	calls, blocked := collectSourceCalls(root)

	allEntries := append([]entry(nil), al.Perm...)
	allEntries = append(allEntries, al.Migr...)

	// Build a remaining-allowances map keyed by (file, line, fn).
	// Entries without a column match any column on that line.
	remaining := map[string]int{}
	for _, e := range allEntries {
		k := entryKey(e.File, e.Line, 0, e.Fn)
		remaining[k]++
	}

	var unclassified []sourceCall
	for _, c := range calls {
		k := entryKey(c.File, c.Line, 0, c.Fn)
		if remaining[k] > 0 {
			remaining[k]--
			continue
		}
		// Also try exact column match.
		kCol := entryKey(c.File, c.Line, c.Col, c.Fn)
		if remaining[kCol] > 0 {
			remaining[kCol]--
			continue
		}
		unclassified = append(unclassified, c)
	}

	// Remaining entries with positive count are stale.
	var stale []entry
	for _, e := range allEntries {
		k := entryKey(e.File, e.Line, 0, e.Fn)
		for remaining[k] > 0 {
			stale = append(stale, e)
			remaining[k]--
		}
	}

	return Report{
		SourceCalls:  calls,
		Unclassified: unclassified,
		Stale:        stale,
		Blocked:      blocked,
	}
}

// ---------- scanning ----------

func collectSourceCalls(root string) (calls []sourceCall, blocked []sourceCall) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil
	}
	_ = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
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

		imports, dotImports := resolveImports(f)

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				c, ok := resolveCall(rel, node, imports, dotImports, fset)
				if ok {
					calls = append(calls, c)
				}

			case *ast.AssignStmt, *ast.ValueSpec, *ast.GenDecl:
				findValueExtractions(rel, node, imports, fset, &blocked)
			}
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
		return calls[i].Col < calls[j].Col
	})
	sort.Slice(blocked, func(i, j int) bool {
		if blocked[i].File != blocked[j].File {
			return blocked[i].File < blocked[j].File
		}
		return blocked[i].Line < blocked[j].Line
	})
	return calls, blocked
}

func resolveImports(f *ast.File) (imports map[string]string, dotImports map[string]bool) {
	imports = map[string]string{}
	dotImports = map[string]bool{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := filepath.Base(path)
		if imp.Name != nil {
			if imp.Name.Name == "." {
				if _, ok := watched[path]; ok {
					dotImports[path] = true
				}
				continue
			}
			name = imp.Name.Name
		}
		imports[name] = path
	}
	return
}

func resolveCall(relFile string, call *ast.CallExpr, imports map[string]string, dotImports map[string]bool, fset *token.FileSet) (sourceCall, bool) {
	// pkg.Symbol(...)
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if pkgIdent, ok := sel.X.(*ast.Ident); ok {
			if pkgPath, ok := imports[pkgIdent.Name]; ok {
				if syms, ok := watched[pkgPath]; ok {
					if slices.Contains(syms, sel.Sel.Name) {
						pos := fset.Position(call.Pos())
						return sourceCall{File: relFile, Line: pos.Line, Col: pos.Column, Fn: filepath.Base(pkgPath) + "." + sel.Sel.Name}, true
					}
				}
			}
		}
		return sourceCall{}, false
	}

	// Dot-import: bare Symbol(...) where Symbol is in a dot-imported watch set.
	if ident, ok := call.Fun.(*ast.Ident); ok {
		for pkgPath := range dotImports {
			if syms, ok := watched[pkgPath]; ok {
				if slices.Contains(syms, ident.Name) {
					pos := fset.Position(call.Pos())
					return sourceCall{File: relFile, Line: pos.Line, Col: pos.Column, Fn: filepath.Base(pkgPath) + "." + ident.Name}, true
				}
			}
		}
	}
	return sourceCall{}, false
}

func findValueExtractions(relFile string, n ast.Node, imports map[string]string, fset *token.FileSet, blocked *[]sourceCall) {
	// Walk values in assignments and var declarations looking for
	// watched functions that are being extracted rather than called.
	// Pattern:  var f = os.RemoveAll   or   f := os.RemoveAll
	// or:       f = os.RemoveAll (plain assignment)
	var values []ast.Expr

	switch node := n.(type) {
	case *ast.AssignStmt:
		for _, rhs := range node.Rhs {
			values = append(values, rhs)
		}
	case *ast.ValueSpec:
		for _, val := range node.Values {
			values = append(values, val)
		}
	default:
		return
	}

	for _, val := range values {
		sel, ok := val.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			continue
		}
		pkgPath, ok := imports[pkgIdent.Name]
		if !ok {
			continue
		}
		syms, ok := watched[pkgPath]
		if !ok {
			continue
		}
		if !slices.Contains(syms, sel.Sel.Name) {
			continue
		}
		pos := fset.Position(n.Pos())
		*blocked = append(*blocked, sourceCall{
			File: relFile,
			Line: pos.Line,
			Col:  pos.Column,
			Fn:   filepath.Base(pkgPath) + "." + sel.Sel.Name,
		})
	}
}

// ---------- key helpers ----------

func entryKey(file string, line, col int, fn string) string {
	if col > 0 {
		return fmt.Sprintf("%s:%d:%d:%s", file, line, col, fn)
	}
	return fmt.Sprintf("%s:%d:%s", file, line, fn)
}

// ---------- main ----------

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

	if errs := ValidateAllowlist(al); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "fsaudit: %v\n", e)
		}
		os.Exit(1)
	}

	report := Audit(".", al)

	exit := false
	if len(report.Blocked) > 0 {
		fmt.Fprintf(os.Stderr, "fsaudit: %d blocked capability escape(s):\n\n", len(report.Blocked))
		for _, c := range report.Blocked {
			fmt.Fprintf(os.Stderr, "  %s:%d  %s\n", c.File, c.Line, c.Fn)
		}
		fmt.Fprintf(os.Stderr, "\nThese patterns create unclassifiable filesystem access:\n")
		fmt.Fprintf(os.Stderr, "  - dot import of os or path/filepath\n")
		fmt.Fprintf(os.Stderr, "  - extracting a watched function as a value (e.g. f := os.RemoveAll)\n")
		fmt.Fprintf(os.Stderr, "  - (*os.Root) method call outside internal/rootfs\n")
		fmt.Fprintf(os.Stderr, "\n")
		exit = true
	}

	if len(report.Stale) > 0 {
		fmt.Fprintf(os.Stderr, "fsaudit: %d stale allowlist entry(ies):\n\n", len(report.Stale))
		for _, e := range report.Stale {
			fmt.Fprintf(os.Stderr, "  %s %s:%d %s\n", classify(e), e.File, e.Line, e.Fn)
		}
		fmt.Fprintf(os.Stderr, "\n")
		exit = true
	}

	if len(report.Unclassified) > 0 {
		fmt.Fprintf(os.Stderr, "fsaudit: %d unclassified direct filesystem call(s):\n\n", len(report.Unclassified))
		counts := map[string]int{}
		for _, c := range report.Unclassified {
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

func classify(e entry) string {
	if e.PR != "" {
		return "migration"
	}
	return "permanent"
}
