package tools

import (
	"fmt"

	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/pathid"
)

// workspaceRepo resolves the required root argument against the sandbox and
// opens it as a git repository. git_* tools take an explicit root rather than
// assuming a singular workspace because a project may attach several
// directories.
func workspaceRepo(c CallInfo, args map[string]any) (*gitw.Repo, string, error) {
	root, ok := args["root"].(string)
	if !ok || root == "" {
		return nil, "", fmt.Errorf("missing root argument — pass one of the project's attached directories")
	}
	absRoot, err := validatePath(root, c.SandboxRoots)
	if err != nil {
		return nil, "", err
	}
	repo, err := gitw.Open(absRoot)
	if err != nil {
		return nil, "", fmt.Errorf("%s is not a git repository", absRoot)
	}
	return repo, absRoot, nil
}

// workspaceWriteRepo is workspaceRepo with an additional C2 scope check: git
// write tools are rejected when their resolved root is a project memory repo.
//
// The check fails closed. An unconfigured predicate or an error resolving the
// scope both reject the call, because neither is evidence that the root is
// outside every memory repo.
func workspaceWriteRepo(c CallInfo, args map[string]any) (*gitw.Repo, string, error) {
	repo, absRoot, err := workspaceRepo(c, args)
	if err != nil {
		return nil, "", err
	}
	if c.MemoryRepoCheck == nil {
		_ = repo.Close()
		return nil, "", fmt.Errorf("C2 scope check unavailable: %w", ErrMemoryScopeUnavailable)
	}
	inMemoryRepo, err := c.MemoryRepoCheck(absRoot)
	if err != nil {
		_ = repo.Close()
		return nil, "", fmt.Errorf("C2 scope check failed for %s: %w", absRoot, err)
	}
	if inMemoryRepo {
		_ = repo.Close()
		return nil, "", fmt.Errorf("C2 scope violation: %s resolves to a project memory repository", absRoot)
	}
	return repo, absRoot, nil
}

// isMemoryRepo reports whether absRoot matches or is contained within any of
// the given memory repo paths. Both sides are physically resolved so that
// junction/symlink-based setups (common on Windows) do not bypass the
// predicate: filepath.EvalSymlinks leaves a junction unresolved, so a junction
// attached as a project directory and pointing at a memory repo compared
// unequal and the C2 lock let the write through.
//
// A resolution failure is an error, never a false. The predicate guards a hard
// lock, and "I could not locate this path" must not be delivered to the caller
// as "this path is not a memory repo".
func isMemoryRepo(absRoot string, memoryPaths []string) (bool, error) {
	return pathid.SameOrWithin(absRoot, memoryPaths)
}

// gitRootProperty is the shared schema for the explicit root argument.
func gitRootProperty() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "Workspace repository root — one of the active project's attached directories",
	}
}
