package index

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/coord"
	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/pathid"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

// TestIndex_UpsertRootedUnder_RightGate publishes under a caller-held
// coordinator without reacquiring it, so a publish-and-commit transaction
// can hold the gate across both operations.
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

	g := coord.Default().GateFor(id.Key())
	g.Lock()
	err = idx.UpsertRootedUnder(g, r, "a", "a", [][]float32{{1, 0}})
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

	foreign := coord.Default().GateFor("some-other-repository")
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
// git mutation enter the same repository transaction. While one is blocked
// inside its critical section, the other cannot reach its own mutation hook;
// once released, it proceeds. Each contender signals immediately before its
// gate acquisition, so the test provably knows it reached the lock before
// asserting it is blocked on it. This is the discriminating test for the
// shared coordinator: if git and index used separate mutexes with the
// same-looking key, neither direction would block.
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
	idx, err := CreateRooted(root, dir, 2, repo.Identity())
	if err != nil {
		t.Fatal(err)
	}

	// Direction 1: git holds the coordinator inside WithMutation; the index
	// publication must not reach its write hook until git releases.
	gitEntered := make(chan struct{})
	gitRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gitRelease) }) }
	defer release()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := repo.WithMutation(func(*gitw.Mutation) error {
			close(gitEntered)
			<-gitRelease
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()
	<-gitEntered

	indexAtLock := make(chan struct{})
	indexEntered := make(chan struct{})
	var wg2 sync.WaitGroup
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		if err := idx.upsertRootedBeforeLock(root, "a", "a", [][]float32{{1, 0}}, rootfs.WriteHooks{
			AfterOpen: func(*os.File, string) { close(indexEntered) },
		}, func() { close(indexAtLock) }); err != nil {
			t.Error(err)
		}
	}()
	<-indexAtLock
	select {
	case <-indexEntered:
		release()
		t.Fatal("index publication entered its write hook while git held the repository transaction")
	case <-time.After(5 * time.Second):
	}
	release()
	select {
	case <-indexEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("index publication never entered after git released the repository transaction")
	}
	wg.Wait()
	wg2.Wait()

	// Direction 2: the index holds the coordinator inside its publication; a
	// git mutation must not reach its mutation body until the index releases.
	indexEntered2 := make(chan struct{})
	indexRelease := make(chan struct{})
	var release2Once sync.Once
	release2 := func() { release2Once.Do(func() { close(indexRelease) }) }
	defer release2()

	var wg3 sync.WaitGroup
	wg3.Add(1)
	go func() {
		defer wg3.Done()
		if err := idx.upsertRootedBeforeLock(root, "b", "b", [][]float32{{0, 1}}, rootfs.WriteHooks{
			AfterOpen: func(*os.File, string) {
				close(indexEntered2)
				<-indexRelease
			},
		}, nil); err != nil {
			t.Error(err)
		}
	}()
	<-indexEntered2

	gitAtLock := make(chan struct{})
	gitEntered2 := make(chan struct{})
	var wg4 sync.WaitGroup
	wg4.Add(1)
	go func() {
		defer wg4.Done()
		if err := repo.WithMutationHooked(func(*gitw.Mutation) error {
			close(gitEntered2)
			return nil
		}, func() { close(gitAtLock) }); err != nil {
			t.Error(err)
		}
	}()
	<-gitAtLock
	select {
	case <-gitEntered2:
		release2()
		t.Fatal("git mutation entered its transaction while the index held the repository coordinator")
	case <-time.After(5 * time.Second):
	}
	release2()
	select {
	case <-gitEntered2:
	case <-time.After(5 * time.Second):
		t.Fatal("git mutation never entered after the index released the repository coordinator")
	}
	wg3.Wait()
	wg4.Wait()

	// Both publications must have landed, proving the transaction serialized
	// them rather than dropping one.
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	opened, err := OpenRooted(r2, dir, repo.Identity())
	if err != nil {
		t.Fatal(err)
	}
	if !opened.Contains("a") || !opened.Contains("b") {
		t.Fatalf("concurrent publication lost an entry: a=%v b=%v", opened.Contains("a"), opened.Contains("b"))
	}
}

// TestIndex_TransactionCannotDeadlockByReacquiring verifies the Under entry
// points used inside a transaction never acquire the coordinator a second
// time: a full upsert under the held gate (through the real production shape)
// completes rather than deadlocking. repoID is resolved explicitly here to
// pin down the discriminator: the gate passed must be the index's own.
func TestIndex_TransactionCannotDeadlockByReacquiring(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	id, err := pathid.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := CreateRooted(r, dir, 2, id)
	if err != nil {
		t.Fatal(err)
	}

	g := coord.Default().GateFor(id.Key())
	g.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Runs while g is held: must not block on g itself.
		if uerr := idx.UpsertRootedUnder(g, r, "a", "a", [][]float32{{1, 0}}); uerr != nil {
			t.Errorf("upsert under held gate: %v", uerr)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		g.Unlock()
		t.Fatal("upsert under a held gate deadlocked on the coordinator")
	}
	g.Unlock()
}
