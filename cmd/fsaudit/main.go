package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// watched maps a canonical import path to the symbols whose direct calls
// must be classified in the allowlist.  The policy is compiled in — it is
// not configurable from the allowlist.
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
	Justification string `json:"justification"`
}

// allowlist classifies every direct filesystem call in production code.
// The migration category is gone by design: the configured-tree migration is
// complete, so every remaining call must be a narrow permanent boundary
// exception. The JSON schema carries no migration field, and the decoder
// rejects unknown fields, so a migration entry cannot be repopulated later.
type allowlist struct {
	Perm []entry `json:"permanent"`
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

// ---------- allowlist parsing and validation ----------

// parseAllowlist decodes an allowlist, rejecting any unknown field and any
// trailing JSON value after the allowlist itself. The schema intentionally has
// no migration category, so a stray "migration", "pr", or "notes" key fails
// the parse rather than being silently ignored.
func parseAllowlist(data []byte) (allowlist, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var al allowlist
	if err := dec.Decode(&al); err != nil {
		return al, err
	}
	// A second decode must hit EOF. A valid allowlist followed by another JSON
	// object would otherwise be accepted without examining the extra value.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return al, fmt.Errorf("allowlist: unexpected trailing JSON value")
		}
		return al, err
	}
	return al, nil
}

func ValidateAllowlist(al allowlist) []error {
	var errs []error
	seen := map[string]bool{}
	for _, e := range al.Perm {
		k := entryKey(e.File, e.Line, e.Col, e.Fn)
		if seen[k] {
			errs = append(errs, &AllowlistError{Entry: e, Msg: "duplicate entry"})
		}
		seen[k] = true
		if e.Justification == "" {
			errs = append(errs, &AllowlistError{Entry: e, Msg: "permanent exception requires a justification"})
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

	remaining := map[string]int{}
	for _, e := range allEntries {
		k := entryKey(e.File, e.Line, e.Col, e.Fn)
		remaining[k]++
	}

	var unclassified []sourceCall
	for _, c := range calls {
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

type scanCtx struct {
	rel        string
	fset       *token.FileSet
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

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}

		imports, dotImports := resolveImports(f)
		ctx := scanCtx{
			rel:        rel,
			fset:       fset,
			imports:    imports,
			dotImports: dotImports,
			inRootFS:   strings.HasPrefix(rel, "internal/rootfs/"),
		}

		add := func(c sourceCall) { calls = append(calls, c) }
		block := func(c sourceCall) { blocked = append(blocked, c) }

		// Block dot imports of watched packages.
		for _, imp := range f.Imports {
			if imp.Name != nil && imp.Name.Name == "." {
				p := strings.Trim(imp.Path.Value, `"`)
				if _, ok := watched[p]; ok {
					pos := fset.Position(imp.Pos())
					block(sourceCall{File: rel, Line: pos.Line, Col: pos.Column, Fn: "import ." + `"` + p + `"`})
				}
			}
		}

		// Single walk: classify calls first, record their selector
		// positions to suppress escape-detection on subsequent visits.
		inCallPos := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				c, ok := resolveCall(&ctx, node)
				if ok {
					add(c)
					inCallPos[node.Fun.Pos()] = true
					if c.Fn == "os.OpenRoot" && !ctx.inRootFS {
						// Creating a root is the core primitive of the
						// boundary; it must be centralized in internal/rootfs.
						block(sourceCall{File: c.File, Line: c.Line, Col: c.Col, Fn: "os.OpenRoot outside internal/rootfs"})
					}
				}

			case *ast.SelectorExpr:
				checkRootTypeRefSelector(&ctx, node, block)
				if inCallPos[node.Pos()] {
					return true
				}
				checkNonCallSelector(&ctx, node, block)

			case *ast.Ident:
				if inCallPos[node.Pos()] {
					return true
				}
				checkDotImportNonCall(&ctx, node, block)
			}
			return true
		})

		if ctx.inRootFS {
			checkRootfsRootRefs(f, imports, fset, rel, block)
		}
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

// resolveSelector resolves sel (which must be an os.Func or filepath.Func
// selector) to a canonical name.  Returns false if the selector is not a
// watched call.
func resolveSelector(imports map[string]string, sel *ast.SelectorExpr) (canon string, ok bool) {
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	pkgPath, ok := imports[pkgIdent.Name]
	if !ok {
		return "", false
	}
	syms, wok := watched[pkgPath]
	if !wok || !slices.Contains(syms, sel.Sel.Name) {
		return "", false
	}
	return filepath.Base(pkgPath) + "." + sel.Sel.Name, true
}

func makeSourceCall(ctx *scanCtx, pos token.Pos, fn string) sourceCall {
	p := ctx.fset.Position(pos)
	return sourceCall{File: ctx.rel, Line: p.Line, Col: p.Column, Fn: fn}
}

func resolveCall(ctx *scanCtx, call *ast.CallExpr) (sourceCall, bool) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if canon, ok := resolveSelector(ctx.imports, sel); ok {
			return makeSourceCall(ctx, call.Pos(), canon), true
		}
	}
	if ident, ok := call.Fun.(*ast.Ident); ok {
		for pkgPath := range ctx.dotImports {
			if syms, wok := watched[pkgPath]; wok {
				if slices.Contains(syms, ident.Name) {
					return makeSourceCall(ctx, call.Pos(), filepath.Base(pkgPath)+"."+ident.Name), true
				}
			}
		}
	}
	return sourceCall{}, false
}

func checkNonCallSelector(ctx *scanCtx, sel *ast.SelectorExpr, block func(sourceCall)) {
	if canon, ok := resolveSelector(ctx.imports, sel); ok {
		block(makeSourceCall(ctx, sel.Pos(), canon))
	}
}

func checkDotImportNonCall(ctx *scanCtx, ident *ast.Ident, block func(sourceCall)) {
	for pkgPath := range ctx.dotImports {
		syms, wok := watched[pkgPath]
		if !wok || !slices.Contains(syms, ident.Name) {
			continue
		}
		block(makeSourceCall(ctx, ident.Pos(), filepath.Base(pkgPath)+"."+ident.Name))
	}
}

func checkRootTypeRefSelector(ctx *scanCtx, sel *ast.SelectorExpr, block func(sourceCall)) {
	if ctx.inRootFS {
		return
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	if ctx.imports[pkgIdent.Name] != "os" || sel.Sel.Name != "Root" {
		return
	}
	block(makeSourceCall(ctx, sel.Pos(), "os.Root"))
}

// checkRootfsRootRefs blocks every os.Root reference in a rootfs file
// except the private Root.root *os.Root backing field.
func checkRootfsRootRefs(f *ast.File, imports map[string]string, fset *token.FileSet, rel string, block func(sourceCall)) {
	allowedPos := map[token.Pos]bool{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Root" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				if len(field.Names) == 1 && field.Names[0].Name == "root" {
					if star, isStar := field.Type.(*ast.StarExpr); isStar {
						if sel, isSel := star.X.(*ast.SelectorExpr); isSel {
							if pkgIdent, ok := sel.X.(*ast.Ident); ok {
								if imports[pkgIdent.Name] == "os" && sel.Sel.Name == "Root" {
									allowedPos[sel.Pos()] = true
								}
							}
						}
					}
				}
			}
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if imports[pkgIdent.Name] != "os" || sel.Sel.Name != "Root" {
			return true
		}
		if allowedPos[sel.Pos()] {
			return true
		}
		pos := fset.Position(sel.Pos())
		block(sourceCall{File: rel, Line: pos.Line, Col: pos.Column, Fn: "os.Root in rootfs"})
		return true
	})
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
	al, err := parseAllowlist(data)
	if err != nil {
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
		fmt.Fprintf(os.Stderr, "  - os.Root type reference outside internal/rootfs\n")
		fmt.Fprintf(os.Stderr, "  - os.Root reference inside internal/rootfs (only Root.root is permitted)\n")
		fmt.Fprintf(os.Stderr, "  - os.OpenRoot call outside internal/rootfs\n")
		fmt.Fprintf(os.Stderr, "\n")
		exit = true
	}

	if len(report.Stale) > 0 {
		fmt.Fprintf(os.Stderr, "fsaudit: %d stale allowlist entry(ies):\n\n", len(report.Stale))
		for _, e := range report.Stale {
			fmt.Fprintf(os.Stderr, "  %s:%d %s\n", e.File, e.Line, e.Fn)
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
		fmt.Fprintf(os.Stderr, "\nThe configured-tree migration is complete. Every remaining direct call\n")
		fmt.Fprintf(os.Stderr, "must be a narrow, justified permanent boundary exception. Add it to\n")
		fmt.Fprintf(os.Stderr, "cmd/fsaudit/allowlist.json under \"permanent\" with a justification, or\n")
		fmt.Fprintf(os.Stderr, "route the operation through internal/rootfs instead.\n")
		exit = true
	}

	if exit {
		os.Exit(1)
	}
	fmt.Println("fsaudit: all direct filesystem calls are permanent boundary exceptions")
}
