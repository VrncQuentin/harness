package parser

import (
	"errors"
	"strings"
	"testing"
)

type fakeFrontEnd struct {
	lang        string
	exts        []string
	selfTestErr error
}

func (f *fakeFrontEnd) Language() string                 { return f.lang }
func (f *fakeFrontEnd) Extensions() []string             { return f.exts }
func (f *fakeFrontEnd) Outline([]byte) ([]Symbol, error) { return nil, nil }
func (f *fakeFrontEnd) Check([]byte) error               { return nil }
func (f *fakeFrontEnd) SelfTest() error                  { return f.selfTestErr }

func TestNewRegistry(t *testing.T) {
	tests := []struct {
		name    string
		fronts  []FrontEnd
		wantErr string // substring; empty means success
	}{
		{
			name:   "working front-end registers",
			fronts: []FrontEnd{&fakeFrontEnd{lang: "fake", exts: []string{".fk"}}},
		},
		{
			name:    "broken parser fails construction",
			fronts:  []FrontEnd{&fakeFrontEnd{lang: "fake", exts: []string{".fk"}, selfTestErr: errors.New("no parser")}},
			wantErr: "failed self-test",
		},
		{
			name:    "nil front-end fails construction",
			fronts:  []FrontEnd{nil},
			wantErr: "nil front-end",
		},
		{
			name: "duplicate extension fails construction",
			fronts: []FrontEnd{
				&fakeFrontEnd{lang: "a", exts: []string{".x"}},
				&fakeFrontEnd{lang: "b", exts: []string{".x"}},
			},
			wantErr: "duplicate front-end",
		},
		{
			name:    "extension without dot fails construction",
			fronts:  []FrontEnd{&fakeFrontEnd{lang: "a", exts: []string{"x"}}},
			wantErr: "invalid extension",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRegistry(tt.fronts...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NewRegistry() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewRegistry() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRegistry_ForPath(t *testing.T) {
	reg, err := NewRegistry(NewGoFrontEnd())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{`C:\repo\pkg\file.GO`, true},
		{"README.md", false},
		{"noext", false},
	}
	for _, tt := range tests {
		if _, ok := reg.ForPath(tt.path); ok != tt.want {
			t.Errorf("ForPath(%q) = %v, want %v", tt.path, ok, tt.want)
		}
	}
	if langs := reg.Languages(); len(langs) != 1 || langs[0] != "go" {
		t.Errorf("Languages() = %v, want [go]", langs)
	}
}
