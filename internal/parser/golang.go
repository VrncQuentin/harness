package parser

import (
	"bytes"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// GoFrontEnd parses Go source with the standard library's go/parser.
// Single-file only: cross-package type resolution is out of scope for the
// M10.1 tier.
type GoFrontEnd struct{}

var _ FrontEnd = (*GoFrontEnd)(nil)

// NewGoFrontEnd returns the Go front-end.
func NewGoFrontEnd() *GoFrontEnd { return &GoFrontEnd{} }

// Language implements FrontEnd.
func (f *GoFrontEnd) Language() string { return "go" }

// Extensions implements FrontEnd.
func (f *GoFrontEnd) Extensions() []string { return []string{".go"} }

// Check implements FrontEnd.
func (f *GoFrontEnd) Check(src []byte) error {
	fset := token.NewFileSet()
	if _, err := goparser.ParseFile(fset, "src.go", src, goparser.SkipObjectResolution); err != nil {
		return fmt.Errorf("parser: go: %w", err)
	}
	return nil
}

// Outline implements FrontEnd. It returns one symbol per top-level
// declaration; grouped type/const/var declarations produce one symbol per
// spec, import declarations one symbol per block.
func (f *GoFrontEnd) Outline(src []byte) ([]Symbol, error) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, "src.go", src, goparser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parser: go: %w", err)
	}
	lines := strings.Split(string(src), "\n")
	var symbols []Symbol
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			symbols = append(symbols, funcSymbol(fset, d, lines))
		case *ast.GenDecl:
			symbols = append(symbols, genSymbols(fset, d, lines)...)
		}
	}
	return symbols, nil
}

const goSelfTestSrc = `package selftest

// Answer is the self-test constant.
const Answer = 42

type pair struct{ a, b int }

func (p pair) Sum() int { return p.a + p.b }

func add(a, b int) int {
	return a + b
}
`

// SelfTest implements FrontEnd.
func (f *GoFrontEnd) SelfTest() error {
	symbols, err := f.Outline([]byte(goSelfTestSrc))
	if err != nil {
		return err
	}
	want := map[string]string{"Answer": "const", "pair": "type", "Sum": "method", "add": "func"}
	for _, sym := range symbols {
		if kind, ok := want[sym.Name]; ok && kind == sym.Kind {
			delete(want, sym.Name)
		}
	}
	if len(want) != 0 {
		return fmt.Errorf("parser: go self-test missed symbols: %v", want)
	}
	return nil
}

func funcSymbol(fset *token.FileSet, d *ast.FuncDecl, lines []string) Symbol {
	sym := Symbol{
		Kind: "func",
		Name: d.Name.Name,
		Span: nodeSpan(fset, d),
	}
	if d.Recv != nil && len(d.Recv.List) > 0 {
		sym.Kind = "method"
		sym.Receiver = exprText(fset, d.Recv.List[0].Type)
	}
	if d.Body != nil {
		sym.Body = nodeSpan(fset, d.Body)
	}
	sym.Signature = signatureLine(lines, sym.Span.StartLine)
	return sym
}

func genSymbols(fset *token.FileSet, d *ast.GenDecl, lines []string) []Symbol {
	if d.Tok == token.IMPORT {
		span := nodeSpan(fset, d)
		return []Symbol{{
			Kind:      "import",
			Name:      "import",
			Span:      span,
			Signature: signatureLine(lines, span.StartLine),
		}}
	}
	kind := strings.ToLower(d.Tok.String())
	var symbols []Symbol
	for _, spec := range d.Specs {
		sym := Symbol{Kind: kind}
		switch s := spec.(type) {
		case *ast.TypeSpec:
			sym.Name = s.Name.Name
			sym.Span = specSpan(fset, d, s)
		case *ast.ValueSpec:
			names := make([]string, 0, len(s.Names))
			for _, n := range s.Names {
				names = append(names, n.Name)
			}
			sym.Name = strings.Join(names, ", ")
			sym.Span = specSpan(fset, d, s)
		default:
			continue
		}
		sym.Signature = signatureLine(lines, sym.Span.StartLine)
		symbols = append(symbols, sym)
	}
	return symbols
}

// specSpan covers the whole declaration for single-spec decls (so `var x = 1`
// includes the keyword) and just the spec inside grouped decls.
func specSpan(fset *token.FileSet, d *ast.GenDecl, spec ast.Node) Span {
	if len(d.Specs) == 1 && !d.Lparen.IsValid() {
		return nodeSpan(fset, d)
	}
	return nodeSpan(fset, spec)
}

func nodeSpan(fset *token.FileSet, n ast.Node) Span {
	return Span{
		StartLine: fset.Position(n.Pos()).Line,
		EndLine:   fset.Position(n.End()).Line,
	}
}

// signatureLine returns the declaration's first source line, trimmed of the
// opening brace so it reads as a signature.
func signatureLine(lines []string, startLine int) string {
	if startLine < 1 || startLine > len(lines) {
		return ""
	}
	line := strings.TrimRight(lines[startLine-1], "\r")
	line = strings.TrimRight(line, " \t")
	line = strings.TrimSuffix(line, "{")
	return strings.TrimRight(line, " \t")
}

func exprText(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		return ""
	}
	return buf.String()
}
