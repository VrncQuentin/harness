package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/VrncQuentin/harness/internal/retrieval"
	"github.com/VrncQuentin/harness/internal/ui"
)

func TestTeeWritesAllSinks(t *testing.T) {
	var left, right bytes.Buffer
	w := tee(&left, &right)

	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len("hello") {
		t.Fatalf("Write returned %d bytes, want %d", n, len("hello"))
	}
	if left.String() != "hello" || right.String() != "hello" {
		t.Fatalf("tee wrote left=%q right=%q, want both hello", left.String(), right.String())
	}
}

// installTraceSink is the production sink installation: a valid harness home
// pins the retrieval trace directory and installs the sink as the package
// default, so production startup always has a working trace path.
func TestInstallTraceSinkInstallsOnValidHome(t *testing.T) {
	srv := ui.NewServer(0)
	home := t.TempDir()

	prev := retrieval.DefaultTraceSink
	defer retrieval.SetDefaultTraceSink(prev)

	sink := installTraceSink(srv, home)
	if sink == nil {
		t.Fatal("installTraceSink returned nil for a valid home")
	}
	defer func() { _ = sink.Close() }()
	if retrieval.DefaultTraceSink != sink {
		t.Fatal("the constructed sink was not installed as the package default")
	}
	if _, err := os.Stat(filepath.Join(home, "logs", "retrieval")); err != nil {
		t.Fatalf("trace directory was not created through rooted operations: %v", err)
	}
}

// A sink construction failure must be surfaced rather than silently ignored:
// no sink is installed and the failure is reported as a startup error.
func TestInstallTraceSinkSurfacesConstructionFailure(t *testing.T) {
	srv := ui.NewServer(0)
	// A regular file in place of the harness home: the logs/retrieval path
	// cannot be created beneath it, so NewNDJSONSink must fail.
	blocker := filepath.Join(t.TempDir(), "homefile")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := retrieval.DefaultTraceSink
	defer retrieval.SetDefaultTraceSink(prev)

	sink := installTraceSink(srv, blocker)
	if sink != nil {
		t.Fatalf("installTraceSink returned a sink for an uncreatable trace dir")
	}
	if retrieval.DefaultTraceSink != prev {
		t.Fatal("a failed construction must not install a sink")
	}
}

// A completed shutdown closes the trace sink and uninstalls it, so no further
// emission writes through a closed handle.
func TestCloseTraceSinkOnCompletedClosesAndUninstalls(t *testing.T) {
	srv := ui.NewServer(0)
	sink := installTraceSink(srv, t.TempDir())
	if sink == nil {
		t.Fatal("installTraceSink failed")
	}

	closeTraceSinkOnCompleted(sink, true)
	if retrieval.DefaultTraceSink != nil {
		t.Fatal("a completed shutdown must uninstall the trace sink")
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("closing an already-closed sink must stay idempotent: %v", err)
	}
}

// A shutdown whose termination is unconfirmed retains the sink: a later retry
// may still emit trace rows through the retained generation, so closing early
// would drop them.
func TestCloseTraceSinkOnCompletedRetainsOnUnconfirmed(t *testing.T) {
	srv := ui.NewServer(0)
	sink := installTraceSink(srv, t.TempDir())
	if sink == nil {
		t.Fatal("installTraceSink failed")
	}
	defer func() { _ = sink.Close() }()

	closeTraceSinkOnCompleted(sink, false)
	if retrieval.DefaultTraceSink != sink {
		t.Fatal("an unconfirmed shutdown must retain the trace sink")
	}
}

// closeTraceSinkOnCompleted must be safe with a nil sink (no sink installed).
func TestCloseTraceSinkOnCompletedNilSinkIsNoOp(t *testing.T) {
	prev := retrieval.DefaultTraceSink
	defer retrieval.SetDefaultTraceSink(prev)
	retrieval.SetDefaultTraceSink(nil)

	closeTraceSinkOnCompleted(nil, true)
	if retrieval.DefaultTraceSink != nil {
		t.Fatal("nil sink must be a no-op")
	}
}
