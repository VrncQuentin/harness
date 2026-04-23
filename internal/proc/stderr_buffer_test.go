package proc

import "testing"

func TestStderrBuffer_RetainsTrailingLines(t *testing.T) {
	b := newStderrBuffer(3)
	input := "line1\nline2\nline3\nline4\nline5\n"
	n, err := b.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(input) {
		t.Errorf("short write: got %d, want %d", n, len(input))
	}
	got := b.Snapshot()
	want := []string{"line3", "line4", "line5"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestStderrBuffer_SplitsAcrossWrites(t *testing.T) {
	b := newStderrBuffer(10)
	b.Write([]byte("par")) //nolint:errcheck
	b.Write([]byte("tial"))
	b.Write([]byte(" line\nnext line\n"))
	got := b.Snapshot()
	want := []string{"partial line", "next line"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestStderrBuffer_IncludesUnterminatedTail(t *testing.T) {
	b := newStderrBuffer(10)
	b.Write([]byte("done\nin progress")) //nolint:errcheck
	got := b.Snapshot()
	want := []string{"done", "in progress"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestStderrBuffer_TrimsCR(t *testing.T) {
	b := newStderrBuffer(10)
	b.Write([]byte("windows\r\nline\r\n")) //nolint:errcheck
	got := b.Snapshot()
	want := []string{"windows", "line"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestStderrBuffer_Reset(t *testing.T) {
	b := newStderrBuffer(5)
	b.Write([]byte("a\nb\npartial")) //nolint:errcheck
	b.Reset()
	if got := b.Snapshot(); len(got) != 0 {
		t.Errorf("after Reset, Snapshot = %v, want empty", got)
	}
	b.Write([]byte("fresh\n")) //nolint:errcheck
	got := b.Snapshot()
	if !equalSlices(got, []string{"fresh"}) {
		t.Errorf("after Reset + Write, Snapshot = %v, want [fresh]", got)
	}
}

func TestStderrBuffer_DefaultMaxLines(t *testing.T) {
	b := newStderrBuffer(0)
	for i := 0; i < 100; i++ {
		b.Write([]byte("x\n")) //nolint:errcheck
	}
	if n := len(b.Snapshot()); n > 64 {
		t.Errorf("Snapshot length %d exceeds sensible default cap", n)
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
