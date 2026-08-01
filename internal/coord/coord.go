// Package coord provides the repository-wide mutation coordinator.
//
// One gate exists per physical repository identity. Every component that
// mutates a repository — a git commit, an index publication, and the combined
// publish-then-commit transaction — acquires the gate for that repository from
// this single registry, so all of them serialize on the same object. Two
// components that resolve the same physical repository to the same identity
// key are handed the same gate; alias, junction, and case spellings therefore
// cannot split one repository across two coordinators.
//
// The registry is deliberately tiny. It holds nothing but the gates; the
// identity keys that select them come from the components themselves, each
// verified against the object the component actually pinned or opened.
package coord

import "sync"

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

// Registry maps repository identity keys to their gates.
type Registry struct {
	mu    sync.Mutex
	gates map[string]*Gate
}

func newRegistry() *Registry {
	return &Registry{gates: make(map[string]*Gate)}
}

// GateFor returns the gate for key, creating it on first use. The same key
// always yields the same gate, so two components on one physical repository
// must derive the key the same way (identity.Key, never a pathname) or they
// will receive separate gates and serialize against nothing.
func (r *Registry) GateFor(key string) *Gate {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g := r.gates[key]; g != nil {
		return g
	}
	g := &Gate{}
	r.gates[key] = g
	return g
}

// defaultRegistry is the process-wide coordinator registry. Every component
// that mutates a repository acquires its gate from here, so git mutations and
// index publications on one repository land on one gate. It is the single
// registry: a component that kept its own would hand out a second, separate
// gate for the same repository and the two writers would exclude nothing.
var defaultRegistry = newRegistry()

// Default returns the process-wide coordinator registry.
func Default() *Registry { return defaultRegistry }
