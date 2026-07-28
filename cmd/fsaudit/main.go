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

// watched maps a canonical import path to the set of symbols classified by
// the scanner. The policy is compiled in and is not configurable from the
// allowlist.
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

type entry struct {
	File          string `json:"file"`
	Line          int    `json:"line"`
	Col           int    `json:"col,omitempty"`
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
	Blocked      []sourceCall
	Err          error
}

type AllowlistError struct {
	Entry entry
	Msg   string
}

func (e *AllowlistError) Error() string {
	return fmt.Sprintf("%s:%d %s: %s", e.Entry.File, e.Entry.Line, e.Entry.Fn, e.Msg)
}

// ---------- validateAllowlist ----------

func ValidateAllowlist(al allowlist) []error {
	var errs []error
	all := append([]entry(nil), al.Perm...)
	all = append(all, al.Migr...)
	seen := map[string]bool{}
	for _, e := range all {
		k := entryKey(e.File, e.Line, e.Col, e.Fn)
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
	calls, blocked, err := collectSourceCalls(root)
	if err != nil {
		return Report{Err: err}
	}

	allEntries := append([]entry(nil), al.Perm...)
	allEntries = append(allEntries, al.Migr...)

	// Map each allowlist entry's key to remaining allowances.
	remaining := map[string]int{}
	for _, e := range allEntries {
		k := entryKey(e.File, e.Line, e.Col, e.Fn)
		remaining[k]++
	}

	var unclassified []sourceCall
	for _, c := range calls {
		// Try column-aware match first; fall back to column-0.
		kCol := entryKey(c.File, c.Line, c.Col, c.Fn)
		if remaining[kCol] > 0 {
			remaining[kCol]--
			continue
		}
		k0 := entryKey(c.File, c.Line, 0, c.Fn)
		if remaining[k0] > 0 {
			remaining[k0]--
			continue
		}
		unclassified = append(unclassified, c)
	}

	var stale []entry
	for _, e := range allEntries {
		k := entryKey(e.File, e.Line, e.Col, e.Fn)
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

type fileInfo struct {
	rel        string
	imports    map[string]string
	dotImports map[string]bool
	inRootFS   bool
}

func collectSourceCalls(root string) (calls []sourceCall, blocked []sourceCall, err error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
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
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}

		imports, dotImports := resolveImports(f)
		info := fileInfo{
			rel:        rel,
			imports:    imports,
			dotImports: dotImports,
			inRootFS:   strings.HasPrefix(rel, "internal/rootfs/"),
		}

		add := func(c sourceCall) { calls = append(calls, c) }
		block := func(c sourceCall) { blocked = append(blocked, c) }

		// First, find all call expressions to identify selectors that
		// are already classified as direct calls.  Those must not be
		// re-flagged as capability extractions.
		callSelPos := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if ce, ok := n.(*ast.CallExpr); ok {
				if sel, isSel := ce.Fun.(*ast.SelectorExpr); isSel {
					callSelPos[sel.Pos()] = true
				}
				if ident, isIdent := ce.Fun.(*ast.Ident); isIdent {
					callSelPos[ident.Pos()] = true
				}
			}
			return true
		})

		// Second walk: classify calls and flag capability escapes.
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				c, ok := resolveCall(info, node, fset)
				if ok {
					add(c)
					return true
				}
				if sel, isSel := node.Fun.(*ast.SelectorExpr); isSel {
					checkRootMethod(info, sel, node, fset, block)
				}

			case *ast.SelectorExpr:
				if callSelPos[node.Pos()] {
					return true
				}
				checkNonCallSelector(info, node, fset, block)

			case *ast.Ident:
				if callSelPos[node.Pos()] {
					return true
				}
				checkDotImportNonCall(info, node, fset, block)

			case *ast.StarExpr:
				// *os.Root type reference outside rootfs.
				checkRootTypeRef(info, node, fset, block)

			case *ast.TypeSpec:
				// type T = os.Root or type T os.Root outside rootfs.
				if sel, isSel := node.Type.(*ast.SelectorExpr); isSel {
					checkRootTypeRefSelector(info, sel, fset, block)
				}
				if star, isStar := node.Type.(*ast.StarExpr); isStar {
					if sel, isSel := star.X.(*ast.SelectorExpr); isSel {
						checkRootTypeRefSelector(info, sel, fset, block)
					}
				}
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
	return calls, blocked, err
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

// resolveCall checks whether the call expression is a direct,
// classified filesystem call.
func resolveCall(fi fileInfo, call *ast.CallExpr, fset *token.FileSet) (sourceCall, bool) {
	// pkg.Symbol(...)
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if pkgIdent, ok := sel.X.(*ast.Ident); ok {
			if pkgPath, ok := fi.imports[pkgIdent.Name]; ok {
				if syms, wok := watched[pkgPath]; wok {
					if slices.Contains(syms, sel.Sel.Name) {
						pos := fset.Position(call.Pos())
						return sourceCall{File: fi.rel, Line: pos.Line, Col: pos.Column, Fn: filepath.Base(pkgPath) + "." + sel.Sel.Name}, true
					}
				}
			}
		}
	}

	// Dot-import call: bare RemoveAll(...).
	if ident, ok := call.Fun.(*ast.Ident); ok {
		for pkgPath := range fi.dotImports {
			if syms, wok := watched[pkgPath]; wok {
				if slices.Contains(syms, ident.Name) {
					pos := fset.Position(call.Pos())
					return sourceCall{File: fi.rel, Line: pos.Line, Col: pos.Column, Fn: filepath.Base(pkgPath) + "." + ident.Name}, true
				}
			}
		}
	}
	return sourceCall{}, false
}

// checkNonCallSelector flags any pkg.Func selector that is not a direct
// classified call — passing, returning, storing a watched function.
func checkNonCallSelector(fi fileInfo, sel *ast.SelectorExpr, fset *token.FileSet, block func(sourceCall)) {
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	pkgPath, ok := fi.imports[pkgIdent.Name]
	if !ok {
		return
	}
	syms, wok := watched[pkgPath]
	if !wok || !slices.Contains(syms, sel.Sel.Name) {
		return
	}
	pos := fset.Position(sel.Pos())
	block(sourceCall{File: fi.rel, Line: pos.Line, Col: pos.Column, Fn: filepath.Base(pkgPath) + "." + sel.Sel.Name})
}

// checkDotImportNonCall flags a bare watched symbol from a dot-import
// that is used outside a direct call position.
func checkDotImportNonCall(fi fileInfo, ident *ast.Ident, fset *token.FileSet, block func(sourceCall)) {
	for pkgPath := range fi.dotImports {
		syms, wok := watched[pkgPath]
		if !wok || !slices.Contains(syms, ident.Name) {
			continue
		}
		pos := fset.Position(ident.Pos())
		block(sourceCall{File: fi.rel, Line: pos.Line, Col: pos.Column, Fn: filepath.Base(pkgPath) + "." + ident.Name})
	}
}

// checkRootMethod flags *os.Root method calls outside rootfs.
func checkRootMethod(fi fileInfo, sel *ast.SelectorExpr, call *ast.CallExpr, fset *token.FileSet, block func(sourceCall)) {
	if fi.inRootFS {
		return
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	if _, isImport := fi.imports[pkgIdent.Name]; isImport {
		return
	}
	// The receiver is a local variable.  The method name is not an
	// import-qualified call, so it is either a method on some local
	// variable (possibly *os.Root) or a package name that did not
	// appear in the imports map.  Flagging every such method call
	// is too noisy (Name(), Open(), ReadDir() are common), so we
	// flag only when the method is specific to *os.Root.
	rootOnlyMethods := map[string]bool{
		"OpenRoot": true,
	}
	if rootOnlyMethods[sel.Sel.Name] {
		pos := fset.Position(call.Pos())
		block(sourceCall{File: fi.rel, Line: pos.Line, Col: pos.Column, Fn: "(*os.Root)." + sel.Sel.Name})
	}
}

// checkRootTypeRef flags *os.Root type references outside rootfs.
func checkRootTypeRef(fi fileInfo, star *ast.StarExpr, fset *token.FileSet, block func(sourceCall)) {
	if fi.inRootFS {
		return
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return
	}
	checkRootTypeRefSelector(fi, sel, fset, block)
}

func checkRootTypeRefSelector(fi fileInfo, sel *ast.SelectorExpr, fset *token.FileSet, block func(sourceCall)) {
	if fi.inRootFS {
		return
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	pkgPath, ok := fi.imports[pkgIdent.Name]
	if !ok || pkgPath != "os" || sel.Sel.Name != "Root" {
		return
	}
	pos := fset.Position(sel.Pos())
	block(sourceCall{File: fi.rel, Line: pos.Line, Col: pos.Column, Fn: "os.Root"})
}

// ---------- keys ----------

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
	if report.Err != nil {
		fmt.Fprintf(os.Stderr, "fsaudit: scan error: %v\n", report.Err)
		os.Exit(1)
	}

	exit := false
	if len(report.Blocked) > 0 {
		fmt.Fprintf(os.Stderr, "fsaudit: %d blocked capability escape(s):\n\n", len(report.Blocked))
		for _, c := range report.Blocked {
			fmt.Fprintf(os.Stderr, "  %s:%d  %s\n", c.File, c.Line, c.Fn)
		}
		fmt.Fprintf(os.Stderr, "\nThese patterns create unclassifiable filesystem access and must be removed:\n")
		fmt.Fprintf(os.Stderr, "  - dot import of os or path/filepath\n")
		fmt.Fprintf(os.Stderr, "  - extracting a watched function as a value\n")
		fmt.Fprintf(os.Stderr, "  - (*os.Root) or os.Root type reference outside internal/rootfs\n")
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
