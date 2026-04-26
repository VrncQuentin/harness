// Package git wraps github.com/go-git/go-git/v5 for the harness memory
// repo. It exposes a small surface over an already-existing repo: open,
// commit specific files, walk the log filtered by structured tags, and
// fetch the bytes of a single-file commit.
//
// The package never initializes or clones a repo. Creating the memory
// repo is the user's responsibility - see the "Memory repo is never
// auto-created" decision in docs/architecture.md.
package git
