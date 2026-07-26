package tools

import (
	"errors"
	"fmt"
)

// ErrMemoryScopeUnavailable is returned by a git write tool when the C2 scope
// of a call cannot be determined. It is never a "no" — it means the question
// could not be answered, which is treated as a rejection.
var ErrMemoryScopeUnavailable = errors.New("tools: memory-repo scope could not be resolved")

// MemoryRepoCheck reports whether absRoot is, or lies within, the memory
// repository of any project. It is the C2 hard lock for the git write tools.
//
// The contract is a predicate evaluated at call time, not a path list captured
// earlier (docs/tool_roadmap.md, "Repo scoping"): projects can be created,
// edited, or repointed while a task is running, and a snapshot taken when the
// task started would answer for a project layout that no longer exists.
//
// A non-nil error must reject the call. The predicate cannot distinguish "this
// is not a memory repo" from "I could not tell", so the caller fails closed on
// both a nil predicate and an error.
type MemoryRepoCheck func(absRoot string) (bool, error)

// NewMemoryRepoCheck builds the C2 predicate over list, which must return every
// project's memory-repo path at the moment it is called.
//
// Two failure sources are propagated rather than swallowed, because both would
// otherwise reach the tool layer as a plain "not a memory repo":
//
//   - list failing. The previous implementation dropped a project-store error
//     into an empty slice, which read as "no memory repos exist".
//   - isMemoryRepo failing to physically resolve either side. A path whose
//     location cannot be determined is not a path known to be outside every
//     memory repo.
func NewMemoryRepoCheck(list func() ([]string, error)) MemoryRepoCheck {
	return func(absRoot string) (bool, error) {
		if list == nil {
			return false, ErrMemoryScopeUnavailable
		}
		paths, err := list()
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrMemoryScopeUnavailable, err)
		}
		inMemoryRepo, err := isMemoryRepo(absRoot, paths)
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrMemoryScopeUnavailable, err)
		}
		return inMemoryRepo, nil
	}
}
