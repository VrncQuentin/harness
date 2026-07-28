package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type classification string

const (
	permanent classification = "permanent"
	migration classification = "migration"
)

type entry struct {
	File          string `json:"file"`
	Line          int    `json:"line"`
	Fn            string `json:"fn"`
	Justification string `json:"justification,omitempty"`
	PR            string `json:"pr,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

type allowlist struct {
	Watched []string `json:"watched_functions"`
	Perm    []entry  `json:"permanent"`
	Migr    []entry  `json:"migration"`
}

type finding struct {
	File string
	Line int
	Fn   string
	Pkg  string
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

	index := buildIndex(al)

	findings, err := scan("internal", index, al.Watched)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsaudit: scan error: %v\n", err)
		os.Exit(1)
	}
	cmdFindings, err := scan("cmd", index, al.Watched)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fsaudit: scan error: %v\n", err)
		os.Exit(1)
	}
	findings = append(findings, cmdFindings...)

	if len(findings) == 0 {
		fmt.Println("fsaudit: all direct filesystem calls are accounted for in the allowlist")
		return
	}

	fmt.Printf("fsaudit: %d unclassified direct filesystem call(s):\n\n", len(findings))
	counts := map[string]int{}
	for _, f := range findings {
		key := f.Fn
		counts[key]++
		fmt.Printf("  %s:%d  %s  (package %s)\n", f.File, f.Line, f.Fn, f.Pkg)
	}
	fmt.Println()
	for fn, n := range counts {
		fmt.Printf("  %d × %s\n", n, fn)
	}
	fmt.Println("\nEach call must be added to cmd/fsaudit/allowlist.json as either:")
	fmt.Println("  - a 'migration' entry (routed through rootfs in a future PR)")
	fmt.Println("  - a 'permanent' entry (intentional boundary exception with justification)")
	os.Exit(1)
}

func buildIndex(al allowlist) map[string]bool {
	idx := map[string]bool{}
	for _, e := range al.Perm {
		idx[key(e.File, e.Line)] = true
	}
	for _, e := range al.Migr {
		idx[key(e.File, e.Line)] = true
	}
	return idx
}

func key(file string, line int) string {
	return fmt.Sprintf("%s:%d", file, line)
}

func scan(root string, allowed map[string]bool, watched []string) ([]finding, error) {
	watch := map[string]bool{}
	for _, w := range watched {
		watch[w] = true
	}

	var results []finding
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
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
		// Skip the audit tool itself and security primitive packages: their calls are intentional.
		if strings.HasPrefix(filepath.ToSlash(path), "cmd/fsaudit/") ||
			strings.HasPrefix(filepath.ToSlash(path), "internal/rootfs/") ||
			strings.HasPrefix(filepath.ToSlash(path), "internal/pathid/") {
			return nil
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		pkg := "main"
		if f.Name != nil {
			pkg = f.Name.Name
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fnName := callName(call)
			if fnName == "" || !watch[fnName] {
				return true
			}
			pos := fset.Position(call.Pos())
			if pos.Line == 0 {
				return true
			}
			// Normalize path to forward slashes for matching.
			normFile := filepath.ToSlash(path)
			if !allowed[key(normFile, pos.Line)] {
				results = append(results, finding{
					File: normFile,
					Line: pos.Line,
					Fn:   fnName,
					Pkg:  pkg,
				})
			}
			return true
		})
		return nil
	})
	sort.Slice(results, func(i, j int) bool {
		if results[i].File != results[j].File {
			return results[i].File < results[j].File
		}
		return results[i].Line < results[j].Line
	})
	return results, err
}

func callName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}
