package index

import (
	"os"
	"path/filepath"
	"sync"
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
// git mutation enter the same repository transaction. Git holds the
// coordinator inside WithMutation; the index publication signals immediately
// before its gate acquisition (proving it reached the lock) and must not
// reach its write hook until git releases. Mutex exclusion is symmetric, so a
// single direction discriminates: if git and index used separate mutexes with
// the same-looking key, the index would publish while git held "its" gate.
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

	// Git holds the coordinator inside WithMutation; the index publication
	// must not reach its write hook until git releases.
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
	case <-time.After(250 * time.Millisecond):
	}
	release()
	select {
	case <-indexEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("index publication never entered after git released the repository transaction")
	}
	wg.Wait()
	wg2.Wait()

	// The publication must have landed, proving the transaction serialized
	// rather than dropping the index write.
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
