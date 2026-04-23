package proc

import "testing"

// writeAll writes every chunk to b, failing the test on any error. Wrapping
// keeps the table tests readable without scattering //nolint:errcheck.
func writeAll(t *testing.T, b *outputBuffer, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		if _, err := b.Write([]byte(c)); err != nil {
			t.Fatalf("Write(%q) returned error: %v", c, err)
		}
	}
}

func TestOutputBuffer_RetainsTrailingLines(t *testing.T) {
	b := newOutputBuffer(3)
	writeAll(t, b, "line1\nline2\nline3\nline4\nline5\n")
	got := b.Snapshot()
	want := []string{"line3", "line4", "line5"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestOutputBuffer_SplitsAcrossWrites(t *testing.T) {
	b := newOutputBuffer(10)
	writeAll(t, b, "par", "tial", " line\nnext line\n")
	got := b.Snapshot()
	want := []string{"partial line", "next line"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestOutputBuffer_IncludesUnterminatedTail(t *testing.T) {
	b := newOutputBuffer(10)
	writeAll(t, b, "done\nin progress")
	got := b.Snapshot()
	want := []string{"done", "in progress"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestOutputBuffer_TrimsCR(t *testing.T) {
	b := newOutputBuffer(10)
	writeAll(t, b, "windows\r\nline\r\n")
	got := b.Snapshot()
	want := []string{"windows", "line"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestOutputBuffer_Reset(t *testing.T) {
	b := newOutputBuffer(5)
	writeAll(t, b, "a\nb\npartial")
	b.Reset()
	if got := b.Snapshot(); len(got) != 0 {
		t.Errorf("after Reset, Snapshot = %v, want empty", got)
	}
	writeAll(t, b, "fresh\n")
	got := b.Snapshot()
	if !equalSlices(got, []string{"fresh"}) {
		t.Errorf("after Reset + Write, Snapshot = %v, want [fresh]", got)
	}
}

func TestOutputBuffer_DefaultMaxLines(t *testing.T) {
	b := newOutputBuffer(0)
	for i := 0; i < 100; i++ {
		writeAll(t, b, "x\n")
	}
	if n := len(b.Snapshot()); n > 64 {
		t.Errorf("Snapshot length %d exceeds sensible default cap", n)
	}
}

func TestOutputBuffer_SetMaxLinesShrink(t *testing.T) {
	b := newOutputBuffer(5)
	writeAll(t, b, "a\nb\nc\nd\ne\n")
	b.SetMaxLines(3)
	got := b.Snapshot()
	want := []string{"c", "d", "e"}
	if !equalSlices(got, want) {
		t.Errorf("after SetMaxLines(3), Snapshot = %v, want %v", got, want)
	}
	writeAll(t, b, "f\n")
	got = b.Snapshot()
	want = []string{"d", "e", "f"}
	if !equalSlices(got, want) {
		t.Errorf("after SetMaxLines+Write, Snapshot = %v, want %v", got, want)
	}
}

func TestOutputBuffer_SetMaxLinesGrow(t *testing.T) {
	b := newOutputBuffer(3)
	writeAll(t, b, "a\nb\nc\n")
	b.SetMaxLines(10)
	writeAll(t, b, "d\ne\n")
	got := b.Snapshot()
	want := []string{"a", "b", "c", "d", "e"}
	if !equalSlices(got, want) {
		t.Errorf("after SetMaxLines(10)+Write, Snapshot = %v, want %v", got, want)
	}
}

func TestOutputBuffer_SetMaxLinesZeroUsesDefault(t *testing.T) {
	b := newOutputBuffer(10)
	b.SetMaxLines(0)
	for i := 0; i < defaultOutputMaxLines+5; i++ {
		writeAll(t, b, "x\n")
	}
	if got := len(b.Snapshot()); got != defaultOutputMaxLines {
		t.Errorf("after SetMaxLines(0), Snapshot length = %d, want %d", got, defaultOutputMaxLines)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
