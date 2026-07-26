package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitw "github.com/VrncQuentin/harness/internal/git"
)

// gitBranchTool implements the git_branch tool: tier-2 local write.
// It requires explicit approval and is rejected for memory-repo roots (C2).
type gitBranchTool struct{}

var _ Tool = (*gitBranchTool)(nil)

func (t *gitBranchTool) ID() string { return "git_branch" }

func (t *gitBranchTool) Description() string {
	return "Create a new local git branch in a workspace repository. " +
		"Rejected if the root resolves to a project memory repo (C2). " +
		"Returns the SHA the branch was created at and the pre-operation HEAD SHA for undo."
}

func (t *gitBranchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root": gitRootProperty(),
			"name": map[string]any{
				"type":        "string",
				"description": "Name of the new branch",
			},
			"start_point": map[string]any{
				"type":        "string",
				"description": "Branch name, tag, or commit SHA to start from. Defaults to HEAD.",
			},
		},
		"required": []string{"root", "name"},
	}
}

func (t *gitBranchTool) Execute(_ context.Context, c CallInfo, args map[string]any) Result {
	name, ok := args["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return Result{Error: "git_branch: name is required"}
	}

	repo, absRoot, err := workspaceWriteRepo(c, args)
	if err != nil {
		return Result{Error: "git_branch: " + err.Error()}
	}

	startPoint, _ := args["start_point"].(string)

	sha, preOpSHA, err := repo.CreateBranch(name, startPoint)
	if err != nil {
		if errors.Is(err, gitw.ErrBranchExists) {
			return Result{Error: fmt.Sprintf(
				"git_branch: %v — creating would discard that tip. Use git_checkout to switch to it, or pick another name.", err)}
		}
		return Result{Error: fmt.Sprintf("git_branch: %v", err)}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "created branch %q at %s in %s\n", name, sha, absRoot)
	// The branch did not exist before this call, so the reversal is deletion,
	// not restoring a previous tip.
	fmt.Fprintf(&b, "undo: git branch -D %s\n", name)
	if preOpSHA != "" {
		fmt.Fprintf(&b, "pre-op HEAD SHA: %s", preOpSHA)
	}
	return Result{Content: b.String()}
}
