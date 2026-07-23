package parser

import (
	"strings"
	"testing"
)

const outlineSrc = `package sample

import (
	"fmt"
)

const answer = 42

const (
	first  = 1
	second = 2
)

var name = "x"

type Greeter struct {
	prefix string
}

func (g *Greeter) Greet(who string) string {
	return fmt.Sprintf("%s %s", g.prefix, who)
}

func Add(a, b int) int {
	return a + b
}
`

func TestGoFrontEnd_Outline(t *testing.T) {
	fe := NewGoFrontEnd()
	symbols, err := fe.Outline([]byte(outlineSrc))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	byName := make(map[string]Symbol, len(symbols))
	for _, s := range symbols {
		byName[s.Name] = s
	}

	tests := []struct {
		name     string
		kind     string
		receiver string
		sigPart  string
		hasBody  bool
	}{
		{name: "import", kind: "import", sigPart: "import ("},
		{name: "answer", kind: "const", sigPart: "const answer = 42"},
		{name: "first", kind: "const", sigPart: "first  = 1"},
		{name: "second", kind: "const", sigPart: "second = 2"},
		{name: "name", kind: "var", sigPart: `var name = "x"`},
		{name: "Greeter", kind: "type", sigPart: "type Greeter struct"},
		{name: "Greet", kind: "method", receiver: "*Greeter", sigPart: "func (g *Greeter) Greet(who string) string", hasBody: true},
		{name: "Add", kind: "func", sigPart: "func Add(a, b int) int", hasBody: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sym, ok := byName[tt.name]
			if !ok {
				t.Fatalf("symbol %q not found in outline: %+v", tt.name, symbols)
			}
			if sym.Kind != tt.kind {
				t.Errorf("Kind = %q, want %q", sym.Kind, tt.kind)
			}
			if sym.Receiver != tt.receiver {
				t.Errorf("Receiver = %q, want %q", sym.Receiver, tt.receiver)
			}
			if !strings.Contains(sym.Signature, tt.sigPart) {
				t.Errorf("Signature = %q, want substring %q", sym.Signature, tt.sigPart)
			}
			if strings.HasSuffix(sym.Signature, "{") {
				t.Errorf("Signature %q keeps opening brace", sym.Signature)
			}
			if sym.Span.StartLine < 1 || sym.Span.EndLine < sym.Span.StartLine {
				t.Errorf("invalid span %+v", sym.Span)
			}
			if tt.hasBody == sym.Body.IsZero() {
				t.Errorf("Body = %+v, want hasBody=%v", sym.Body, tt.hasBody)
			}
		})
	}
}

func TestGoFrontEnd_OutlineSpansMatchSource(t *testing.T) {
	fe := NewGoFrontEnd()
	symbols, err := fe.Outline([]byte(outlineSrc))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}
	lines := strings.Split(outlineSrc, "\n")
	for _, sym := range symbols {
		if sym.Name != "Add" {
			continue
		}
		if got := lines[sym.Span.StartLine-1]; !strings.HasPrefix(got, "func Add") {
			t.Errorf("span start line %d = %q, want func Add", sym.Span.StartLine, got)
		}
		if got := lines[sym.Span.EndLine-1]; got != "}" {
			t.Errorf("span end line %d = %q, want closing brace", sym.Span.EndLine, got)
		}
	}
}

func TestGoFrontEnd_Check(t *testing.T) {
	fe := NewGoFrontEnd()
	if err := fe.Check([]byte("package ok\n\nfunc f() {}\n")); err != nil {
		t.Fatalf("Check(valid) = %v, want nil", err)
	}
	if err := fe.Check([]byte("package broken\n\nfunc f( {\n")); err == nil {
		t.Fatal("Check(invalid) = nil, want syntax error")
	}
}

func TestGoFrontEnd_SelfTest(t *testing.T) {
	if err := NewGoFrontEnd().SelfTest(); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
}
