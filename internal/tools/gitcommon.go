package tools

import (
	"fmt"
	"path/filepath"
	"strings"

	gitw "github.com/VrncQuentin/harness/internal/git"
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
func workspaceWriteRepo(c CallInfo, args map[string]any) (*gitw.Repo, string, error) {
	repo, absRoot, err := workspaceRepo(c, args)
	if err != nil {
		return nil, "", err
	}
	if isMemoryRepo(absRoot, c.MemoryRepoPaths) {
		return nil, "", fmt.Errorf("C2 scope violation: %s resolves to a project memory repository", absRoot)
	}
	return repo, absRoot, nil
}

// isMemoryRepo reports whether absRoot matches or is contained within any of
// the given memory repo paths. Both sides are symlink-resolved so that
// junction/symlink-based setups (common on Windows) do not bypass the predicate.
func isMemoryRepo(absRoot string, memoryPaths []string) bool {
	resolvedAbs, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		resolvedAbs = absRoot
	}
	for _, mp := range memoryPaths {
		if strings.TrimSpace(mp) == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(mp))
		if err != nil {
			resolved = filepath.Clean(mp)
		}
		if pathWithinRoot(resolvedAbs, resolved) {
			return true
		}
	}
	return false
}

// gitRootProperty is the shared schema for the explicit root argument.
func gitRootProperty() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "Workspace repository root — one of the active project's attached directories",
	}
}
