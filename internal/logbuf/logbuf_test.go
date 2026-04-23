package logbuf

import (
	"sync"
	"testing"
	"time"
)

// fixedClock returns a Ring whose timestamps advance by 1ns per call so tests
// can assert ordering without sleeping.
func fixedClock(r *Ring) {
	var n int64
	r.now = func() time.Time {
		n++
		return time.Unix(0, n)
	}
}

func writeAll(t *testing.T, r *Ring, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		if _, err := r.Write([]byte(c)); err != nil {
			t.Fatalf("Write(%q): %v", c, err)
		}
	}
}

func lines(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Line
	}
	return out
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

func TestRing_RetainsTrailingLines(t *testing.T) {
	r := New(3)
	writeAll(t, r, "line1\nline2\nline3\nline4\nline5\n")
	got := lines(r.Snapshot())
	want := []string{"line3", "line4", "line5"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestRing_SplitsAcrossWrites(t *testing.T) {
	r := New(10)
	writeAll(t, r, "par", "tial", " line\nnext line\n")
	got := lines(r.Snapshot())
	want := []string{"partial line", "next line"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestRing_IncludesUnterminatedTail(t *testing.T) {
	r := New(10)
	writeAll(t, r, "done\nin progress")
	got := lines(r.Snapshot())
	want := []string{"done", "in progress"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestRing_TrimsCR(t *testing.T) {
	r := New(10)
	writeAll(t, r, "windows\r\nline\r\n")
	got := lines(r.Snapshot())
	want := []string{"windows", "line"}
	if !equalSlices(got, want) {
		t.Errorf("Snapshot = %v, want %v", got, want)
	}
}

func TestRing_DefaultMaxEntries(t *testing.T) {
	r := New(0)
	for i := 0; i < defaultMaxEntries+50; i++ {
		writeAll(t, r, "x\n")
	}
	if got := len(r.Snapshot()); got != defaultMaxEntries {
		t.Errorf("Snapshot length = %d, want %d", got, defaultMaxEntries)
	}
}

func TestRing_TimestampsAssignedAtWrite(t *testing.T) {
	r := New(3)
	fixedClock(r)
	writeAll(t, r, "a\nb\nc\n")
	es := r.Snapshot()
	if len(es) != 3 {
		t.Fatalf("want 3 entries, got %d", len(es))
	}
	for i := 1; i < len(es); i++ {
		if !es[i].Time.After(es[i-1].Time) {
			t.Errorf("entries[%d].Time (%v) not after entries[%d].Time (%v)",
				i, es[i].Time, i-1, es[i-1].Time)
		}
	}
}

func TestRing_SubscribeReceivesNewEntries(t *testing.T) {
	r := New(10)
	ch := make(chan Entry, 4)
	cancel := r.Subscribe(ch)
	defer cancel()

	writeAll(t, r, "first\nsecond\n")

	got := drain(ch, 2, time.Second)
	want := []string{"first", "second"}
	if !equalSlices(got, want) {
		t.Errorf("subscriber got %v, want %v", got, want)
	}
}

func TestRing_SubscribeCancelStopsDelivery(t *testing.T) {
	r := New(10)
	ch := make(chan Entry, 4)
	cancel := r.Subscribe(ch)

	writeAll(t, r, "before\n")
	cancel()
	writeAll(t, r, "after\n")

	got := drain(ch, 1, 100*time.Millisecond)
	if !equalSlices(got, []string{"before"}) {
		t.Errorf("subscriber got %v, want only [before]", got)
	}
}

func TestRing_SlowSubscriberDoesNotBlockWriter(t *testing.T) {
	r := New(10)
	// Buffer of 1, never read - second send should drop, not deadlock.
	ch := make(chan Entry, 1)
	cancel := r.Subscribe(ch)
	defer cancel()

	done := make(chan struct{})
	go func() {
		writeAll(t, r, "a\nb\nc\nd\n")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write blocked waiting for slow subscriber")
	}

	if got := len(r.Snapshot()); got != 4 {
		t.Errorf("Snapshot len = %d, want 4 (writes must not be lost)", got)
	}
}

func TestRing_ConcurrentWritesAreSafe(t *testing.T) {
	r := New(1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = r.Write([]byte("line\n"))
			}
		}()
	}
	wg.Wait()
	if got := len(r.Snapshot()); got != 800 {
		t.Errorf("Snapshot len = %d, want 800", got)
	}
}

// drain collects up to n entries from ch or returns whatever arrived before
// the timeout.
func drain(ch <-chan Entry, n int, timeout time.Duration) []string {
	deadline := time.After(timeout)
	out := make([]string, 0, n)
	for len(out) < n {
		select {
		case e := <-ch:
			out = append(out, e.Line)
		case <-deadline:
			return out
		}
	}
	return out
}
