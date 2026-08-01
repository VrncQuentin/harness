// Package coord provides the repository-wide mutation coordinator.
//
// One gate exists per physical repository identity. Every component that
// mutates a repository — a git commit, an index publication, and the combined
// publish-then-commit transaction — acquires the gate for that repository
// from For, so all of them serialize on the same object. The identity is
// carried by a pathid.ID, not a string, so the coordinator key can only be
// chosen by physical identity; there is no pathname key a caller could spell
// two ways.
package coord

import (
	"sync"

	"github.com/VrncQuentin/harness/internal/pathid"
)

// Gate is one repository's mutation coordinator. It is a non-reentrant
// mutex: a caller that holds it must use the transaction session
// (gitw.Repo.WithMutation) rather than acquiring it again.
type Gate struct {
	mu sync.Mutex
}

// Lock acquires the gate. The caller must hold it only briefly; a
// publish-and-commit transaction holds it across both operations through a
// session object and never reacquires it.
func (g *Gate) Lock() { g.mu.Lock() }

// Unlock releases the gate.
func (g *Gate) Unlock() { g.mu.Unlock() }

// gates is the process-wide coordinator registry. It is deliberately the
// single registry: a component that kept its own would hand out a second,
// separate gate for the same repository and the two writers would exclude
// nothing.
var gates = struct {
	sync.Mutex
	m map[string]*Gate
}{m: make(map[string]*Gate)}

// For returns the repository-wide mutation coordinator for the physical
// repository id identifies. The same identity always yields the same gate, so
// two components on one physical repository serialize on one object.
func For(id pathid.ID) *Gate {
	key := id.Key()
	gates.Lock()
	defer gates.Unlock()
	if g := gates.m[key]; g != nil {
		return g
	}
	g := &Gate{}
	gates.m[key] = g
	return g
}
