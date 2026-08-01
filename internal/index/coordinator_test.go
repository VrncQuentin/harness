package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/coord"
	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/pathid"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

// TestIndex_UpsertRootedUnder_RightGate publishes under a caller-held
// coordinator without reacquiring it. The gate is held by the same goroutine
// the upsert runs on, so reacquiring it would self-deadlock — completing is
// the proof that a publish-and-commit transaction can hold the gate across
// both operations without reacquiring it.
func TestIndex_UpsertRootedUnder_RightGate(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	id := repoID(t, dir)
	idx, err := CreateRooted(r, dir, 2, id)
	if err != nil {
		t.Fatal(err)
	}

	g := coord.For(id)
	g.Lock()
	// Bounded watchdog: if UpsertRootedUnder regresses to reacquiring the
	// held gate it would self-deadlock; report a failure instead of hanging
	// the package until Go's global test timeout.
	errCh := make(chan error, 1)
	go func() { errCh <- idx.UpsertRootedUnder(g, r, "a", "a", [][]float32{{1, 0}}) }()
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		g.Unlock()
		t.Fatal("upsert under a held gate deadlocked on the coordinator")
	}
	g.Unlock()
	if err != nil {
		t.Fatalf("upsert under the shared coordinator: %v", err)
	}
	if !idx.Contains("a") {
		t.Fatal("entry published under the held coordinator is missing")
	}
}

// TestIndex_UpsertRootedUnder_WrongGateFailsClosed verifies an index cannot
// be published under a coordinator it does not own: a caller holding a gate
// for a different repository must be refused rather than serialized against
// the wrong set of writers.
func TestIndex_UpsertRootedUnder_WrongGateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	id := repoID(t, dir)
	idx, err := CreateRooted(r, dir, 2, id)
	if err != nil {
		t.Fatal(err)
	}

	foreignID, err := pathid.Resolve(filepath.Join(t.TempDir(), "other-repo"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := coord.For(foreignID)
	foreign.Lock()
	err = idx.UpsertRootedUnder(foreign, r, "a", "a", [][]float32{{1, 0}})
	foreign.Unlock()
	if err == nil {
		t.Fatal("upsert under a foreign coordinator must fail closed")
	}
	if idx.Contains("a") {
		t.Fatal("entry leaked despite the refused upsert")
	}
}

// TestRepoAndIndex_ShareRepositoryTransaction verifies index publication and
// git mutation enter the same repository transaction. It runs the exact
// production shape — index publication inside the git WithMutation — and the
// deterministic shared-coordinator discriminator is the gate identity: the
// gate git holds must be the index's own coordinator, or UpsertRootedUnder
// fails closed. The call runs under a bounded watchdog so a reacquisition
// regression fails instead of hanging the package.
func TestRepoAndIndex_ShareRepositoryTransaction(t *testing.T) {
	dir := t.TempDir()
	repo, err := gitw.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	// Seed the repo so go-git worktree operations behave normally.
	if err := os.WriteFile(dir+"/a.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}

	root, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	idx, err := CreateRooted(root, dir, 2, repoID(t, dir))
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- repo.WithMutation(func(m *gitw.Mutation) error {
			if m.Gate() != idx.gate {
				return errors.New("index and git do not share a coordinator")
			}
			return idx.UpsertRootedUnder(m.Gate(), root, "a", "a", [][]float32{{1, 0}})
		})
	}()
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("index publication inside the git transaction deadlocked on the coordinator")
	}
	if err != nil {
		t.Fatal(err)
	}

	// The publication must have landed.
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	opened, err := OpenRooted(r2, dir, repoID(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if !opened.Contains("a") {
		t.Fatal("index publication lost its entry after the git transaction released")
	}
}
