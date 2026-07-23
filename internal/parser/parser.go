// Package parser hosts the language front-ends behind the ast_* tools and
// the governor's query-aware skeletonizer. A front-end is a real parser:
// Registry construction runs each front-end's self-test and fails when the
// parser cannot handle a known-good source, so a harness with a broken
// front-end never reaches the agent loop. Supported-language declarations
// on ast_* tools are generated from this registry, never hand-written.
package parser

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Span is a 1-based inclusive line range within a source file. A zero Span
// means "absent" (e.g. a declaration without a body).
type Span struct {
	StartLine int
	EndLine   int
}

// IsZero reports whether the span is absent.
func (s Span) IsZero() bool { return s.StartLine == 0 && s.EndLine == 0 }

// Symbol is one entry in a structural outline.
type Symbol struct {
	// Kind classifies the declaration: "func", "method", "type", "const",
	// "var", or "import".
	Kind string
	// Name is the declared identifier. Grouped const/var specs join their
	// names with ", "; the import block uses "import".
	Name string
	// Receiver is the method receiver type text, empty for non-methods.
	Receiver string
	// Signature is the first source line of the declaration, trimmed of the
	// opening brace. Deterministic and derived from the source text.
	Signature string
	// Span covers the whole declaration.
	Span Span
	// Body covers the function body braces; zero for bodiless declarations.
	Body Span
}

// FrontEnd is a language parser registered with the harness. Implementations
// must be deterministic: same source in, same outline out, no model
// involvement.
type FrontEnd interface {
	// Language returns the lowercase language name, e.g. "go".
	Language() string
	// Extensions returns the file extensions (with leading dot) this
	// front-end claims.
	Extensions() []string
	// Outline parses src and returns its top-level symbols in source order.
	Outline(src []byte) ([]Symbol, error)
	// Check parses src and returns the first syntax error, or nil when the
	// source parses cleanly.
	Check(src []byte) error
	// SelfTest proves the parser works by outlining a known-good source.
	// Registry construction fails when it returns an error.
	SelfTest() error
}

// Registry resolves files to front-ends. Construct with NewRegistry.
type Registry struct {
	fronts []FrontEnd
	byExt  map[string]FrontEnd
}

// NewRegistry builds a registry from the given front-ends. Every front-end
// is self-tested; a failing or nil front-end makes construction fail so the
// error surfaces as a startup error instead of a broken tool surface.
func NewRegistry(fronts ...FrontEnd) (*Registry, error) {
	r := &Registry{byExt: make(map[string]FrontEnd)}
	for _, f := range fronts {
		if f == nil {
			return nil, errors.New("parser: nil front-end")
		}
		if err := f.SelfTest(); err != nil {
			return nil, fmt.Errorf("parser: front-end %q failed self-test: %w", f.Language(), err)
		}
		for _, ext := range f.Extensions() {
			ext = strings.ToLower(ext)
			if ext == "" || !strings.HasPrefix(ext, ".") {
				return nil, fmt.Errorf("parser: front-end %q declares invalid extension %q", f.Language(), ext)
			}
			if _, dup := r.byExt[ext]; dup {
				return nil, fmt.Errorf("parser: duplicate front-end for extension %q", ext)
			}
			r.byExt[ext] = f
		}
		r.fronts = append(r.fronts, f)
	}
	return r, nil
}

// ForPath returns the front-end claiming path's extension.
func (r *Registry) ForPath(path string) (FrontEnd, bool) {
	f, ok := r.byExt[strings.ToLower(filepath.Ext(path))]
	return f, ok
}

// Languages returns the registered language names in registration order.
func (r *Registry) Languages() []string {
	out := make([]string, 0, len(r.fronts))
	for _, f := range r.fronts {
		out = append(out, f.Language())
	}
	return out
}

// Extensions returns every registered extension in registration order.
func (r *Registry) Extensions() []string {
	out := make([]string, 0, len(r.byExt))
	for _, f := range r.fronts {
		out = append(out, f.Extensions()...)
	}
	return out
}
