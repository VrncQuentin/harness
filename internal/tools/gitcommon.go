package tools

import (
	"fmt"

	gitw "github.com/vrnc/harness/internal/git"
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

// gitRootProperty is the shared schema for the explicit root argument.
func gitRootProperty() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "Workspace repository root — one of the active project's attached directories",
	}
}
