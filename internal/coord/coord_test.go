package coord

import (
	"testing"

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

	// TryLock on the private mutex is the deterministic exclusion assertion:
	// while this goroutine holds the gate it must report the gate held, and
	// after release it must report the gate free. No goroutine scheduling is
	// involved.
	g.Lock()
	if g.mu.TryLock() {
		g.Unlock()
		t.Fatal("TryLock succeeded while the gate was held")
	}
	g.Unlock()

	if !g.mu.TryLock() {
		t.Fatal("TryLock failed after the gate was released")
	}
	g.Unlock()
}
