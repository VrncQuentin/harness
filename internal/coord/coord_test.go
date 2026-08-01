package coord

import (
	"sync"
	"testing"
	"time"
)

func TestRegistry_SameKeyYieldsSameGate(t *testing.T) {
	reg := newRegistry()
	g1 := reg.GateFor("repo-A")
	g2 := reg.GateFor("repo-A")
	if g1 != g2 {
		t.Fatal("same key must yield the same gate")
	}
}

func TestRegistry_DifferentKeysYieldDifferentGates(t *testing.T) {
	reg := newRegistry()
	g1 := reg.GateFor("repo-A")
	g2 := reg.GateFor("repo-B")
	if g1 == g2 {
		t.Fatal("different keys must yield different gates")
	}
}

func TestGate_MutuallyExcludes(t *testing.T) {
	reg := newRegistry()
	g := reg.GateFor("repo")

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
	case <-time.After(5 * time.Second):
	}
	unlock()
	<-done
	<-acquired
}

func TestDefault_IsSingleton(t *testing.T) {
	a := Default()
	if a == nil {
		t.Fatal("Default registry is nil")
	}
	b := Default()
	if a != b {
		t.Fatal("Default must return the same registry every call")
	}
}
