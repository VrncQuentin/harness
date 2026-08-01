package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
)

// linkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create directory link: %v: %s", err, out)
	}
}

// TestRepo_AliasAndTargetShareCoordinator verifies that two spellings of one
// physical repository retain the same verified identity and acquire the same
// repository-wide coordinator, so an alias spelling cannot split one
// repository across two gates.
//
// go-git itself refuses to open a repository through a directory link (its
// pathname API does not follow a junction or symlink — go-billy reports
// "path escapes from parent"), so the alias/target spellings here are the
// lexical ones go-git accepts: a trailing separator, a ".." bounce, and the
// parent-junction form. The memory and index sides do exercise true link
// aliases through their pinned handles.
func TestRepo_AliasAndTargetShareCoordinator(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainInit(real, false); err != nil {
		t.Fatal(err)
	}

	spellings := []string{
		real,
		real + string(filepath.Separator),
		filepath.Join(real, "..", filepath.Base(real)),
	}
	handles := make([]*Repo, 0, len(spellings))
	for _, spelling := range spellings {
		h, err := Open(spelling)
		if err != nil {
			t.Fatalf("Open(%s): %v", spelling, err)
		}
		handles = append(handles, h)
	}
	for i := 1; i < len(handles); i++ {
		if !handles[0].Identity().Equal(handles[i].Identity()) {
			t.Errorf("spelling %d retained a different identity: %s vs %s", i, handles[0].Identity(), handles[i].Identity())
		}
		if handles[0].gate != handles[i].gate {
			t.Errorf("spelling %d acquired a different coordinator instance", i)
		}
	}
}

// TestRepo_RePointedAliasDuringOpenFailsClosed stages a repoint of a stable
// alias in the window between the repository boundary being pinned and its
// identity being resolved. The identity must be bound to the pinned boundary,
// so the open fails closed rather than silently identifying the replacement.
//
// go-git cannot open through a link alias at all, so the handle being opened
// is a go-git repository on the real target; the wrapper's identity open is
// what binds the alias spelling. That is precisely the boundary the invariant
// protects: the spelling a caller used must not silently come to mean another
// repository.
func TestRepo_RePointedAliasDuringOpenFailsClosed(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	evil := filepath.Join(base, "evil")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainInit(real, false); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainInit(evil, false); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	linkDir(t, real, alias)

	gr, err := gogit.PlainOpen(real)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newRepo(gr, alias, func() {
		if rmErr := os.RemoveAll(alias); rmErr != nil {
			t.Fatalf("remove alias: %v", rmErr)
		}
		linkDir(t, evil, alias)
	})
	if err == nil {
		t.Fatal("open accepted a repointed alias; identity must be bound to the pinned boundary")
	}
}

// TestRepo_TwoHandlesThroughDifferentSpellingsShareCoordinator verifies the
// real contention guarantee behind the identity equality: two handles on one
// repository reached through different spellings block on one coordinator, so
// only one commit runs at a time.
func TestRepo_TwoHandlesThroughDifferentSpellingsShareCoordinator(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainInit(real, false); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(real, "..", filepath.Base(real))

	seed, err := Open(real)
	if err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, seed, "base.txt", "base\n")
	if _, _, _, err := seed.WorkspaceStageAndCommit([]string{"base.txt"}, "base"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(firstRelease) }) }
	defer release()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h, oerr := Open(real)
		if oerr != nil {
			t.Error(oerr)
			return
		}
		if err := h.WithMutation(func(*Mutation) error {
			close(firstEntered)
			<-firstRelease
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	var wg2 sync.WaitGroup
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		h, oerr := Open(other)
		if oerr != nil {
			t.Error(oerr)
			return
		}
		// The mutation body is the "hook" that sits inside the critical
		// section: reaching it proves the second spelling entered the same
		// coordinator the first spelling already held.
		if err := h.WithMutation(func(*Mutation) error {
			close(secondEntered)
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()

	select {
	case <-secondEntered:
		release()
		t.Fatal("second spelling entered its mutation while the first held the shared coordinator")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case <-secondEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second spelling never entered after the coordinator was released")
	}
	wg.Wait()
	wg2.Wait()
}

// TestRepoTransaction_CommitInsideSessionDoesNotReacquire verifies the
// transaction API cannot deadlock by reacquiring its own coordinator: the
// Mutation commit methods operate on the already-held gate, so a publish and
// commit inside one WithMutation completes.
func TestRepoTransaction_CommitInsideSessionDoesNotReacquire(t *testing.T) {
	dir := t.TempDir()
	repo, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, repo, "a.txt", "one\n")

	err = repo.WithMutation(func(m *Mutation) error {
		_, cerr := m.Commit("first", []string{"a.txt"})
		return cerr
	})
	if err != nil {
		t.Fatalf("commit inside transaction: %v", err)
	}
	sha, err := repo.HeadSHA()
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Fatal("no commit was created inside the transaction")
	}
}

// TestRepoTransaction_FailedMutationReleasesCoordinator verifies a failed
// mutation releases the coordinator and leaves the repository usable: the
// failure propagates, a second writer can proceed once it is released, and a
// subsequent commit succeeds.
func TestRepoTransaction_FailedMutationReleasesCoordinator(t *testing.T) {
	dir := t.TempDir()
	repo, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, repo, "a.txt", "one\n")
	if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unlock()

	boom := errors.New("injected mutation failure")
	go func() {
		err := repo.WithMutation(func(*Mutation) error {
			close(entered)
			<-release
			return boom
		})
		if !errors.Is(err, boom) {
			t.Errorf("expected injected failure, got %v", err)
		}
		close(done)
	}()
	<-entered

	// A second mutation must not run until the failing one is released.
	secondEntered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := repo.WithMutation(func(*Mutation) error {
			close(secondEntered)
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-secondEntered:
		unlock()
		t.Fatal("second mutation entered while the failing mutation held the coordinator")
	case <-time.After(500 * time.Millisecond):
	}
	unlock()
	<-done
	<-secondEntered
	wg.Wait()

	// The coordinator was released: a fresh commit succeeds.
	writeRepoFile(t, repo, "b.txt", "two\n")
	if _, err := repo.Commit("second", []string{"b.txt"}); err != nil {
		t.Fatalf("commit after failed mutation: %v", err)
	}
}
