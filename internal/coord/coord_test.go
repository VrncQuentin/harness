package coord

import (
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/pathid"
)

func TestFor_SameIdentityYieldsSameGate(t *testing.T) {
	id, err := pathid.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g1 := For(id)
	g2 := For(id)
	if g1 != g2 {
		t.Fatal("same identity must yield the same gate")
	}
}

func TestFor_DifferentIdentitiesYieldDifferentGates(t *testing.T) {
	a, err := pathid.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := pathid.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ga, gb := For(a), For(b)
	if ga == gb {
		t.Fatal("different identities must yield different gates")
	}
}

func TestGate_MutuallyExcludes(t *testing.T) {
	id, err := pathid.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := For(id)

	// The gate is not reentrant: a second acquisition from another goroutine
	// must block until the first releases. The contender signals immediately
	// before its acquisition, so the test provably knows it reached the lock
	// before asserting it is blocked on it.
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	defer unlock()

	go func() {
		g.Lock()
		defer g.Unlock()
		close(entered)
		<-release
		close(done)
	}()
	<-entered

	atLock := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(atLock)
		g.Lock()
		close(acquired)
		g.Unlock()
	}()
	<-atLock

	// The contender is provably blocked on the gate: it signalled immediately
	// before acquisition and the first holder has not released.
	select {
	case <-acquired:
		unlock()
		t.Fatal("second holder acquired while the gate was held")
	case <-time.After(250 * time.Millisecond):
	}
	unlock()
	<-done
	<-acquired
}
