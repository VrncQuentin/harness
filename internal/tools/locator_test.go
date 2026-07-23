package tools

import (
	"strings"
	"testing"
)

func TestFormatLocator(t *testing.T) {
	got := FormatLocator(`C:\repo\a.go`, 3, 9)
	if got != `C:\repo\a.go:3-9` {
		t.Fatalf("FormatLocator = %q", got)
	}
}

func TestSpanHash(t *testing.T) {
	src := []byte("one\ntwo\nthree\n")
	tests := []struct {
		name       string
		start, end int
		wantErr    bool
	}{
		{name: "single line", start: 2, end: 2},
		{name: "full file", start: 1, end: 3},
		{name: "start below one", start: 0, end: 1, wantErr: true},
		{name: "end before start", start: 2, end: 1, wantErr: true},
		{name: "end past eof", start: 1, end: 4, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SpanHash(src, tt.start, tt.end)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SpanHash(%d,%d) = %q, want error", tt.start, tt.end, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SpanHash: %v", err)
			}
			if !strings.HasPrefix(got, "h:") || len(got) != 2+spanHashLen {
				t.Fatalf("SpanHash = %q, want h:<%d hex>", got, spanHashLen)
			}
		})
	}
}

func TestSpanHash_ExactBytes(t *testing.T) {
	// The hash covers the exact bytes of the span including terminators, so
	// the same line content in different files yields the same anchor.
	a, err := SpanHash([]byte("x\nsame line\ny\n"), 2, 2)
	if err != nil {
		t.Fatalf("SpanHash a: %v", err)
	}
	b, err := SpanHash([]byte("completely\ndifferent\nsame line\n"), 3, 3)
	if err != nil {
		t.Fatalf("SpanHash b: %v", err)
	}
	if a != b {
		t.Fatalf("identical span bytes hash differently: %q vs %q", a, b)
	}
	c, err := SpanHash([]byte("x\nsame line changed\ny\n"), 2, 2)
	if err != nil {
		t.Fatalf("SpanHash c: %v", err)
	}
	if a == c {
		t.Fatal("different span bytes produced identical hash")
	}
}

func TestSpanHash_NoTrailingNewline(t *testing.T) {
	if _, err := SpanHash([]byte("only line"), 1, 1); err != nil {
		t.Fatalf("SpanHash on file without trailing newline: %v", err)
	}
}
