package coord

import (
	"sync"
	"testing"
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
	// must block until the first releases.
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

	blocked := make(chan struct{})
	go func() {
		g.Lock()
		close(blocked)
		g.Unlock()
	}()
	select {
	case <-blocked:
		unlock()
		t.Fatal("second holder entered while the gate was held")
	default:
	}
	unlock()
	<-done
	<-blocked
}

func TestDefault_IsSingleton(t *testing.T) {
	if Default() == nil {
		t.Fatal("Default registry is nil")
	}
	if Default() != Default() {
		t.Fatal("Default must return the same registry every call")
	}
}
