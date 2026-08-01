package tools

import (
	"context"
	"fmt"
	"strings"
)

// gitCheckoutTool implements the git_checkout tool: tier-2 local write.
// It requires explicit approval and is rejected for memory-repo roots (C2).
type gitCheckoutTool struct{}

var _ Tool = (*gitCheckoutTool)(nil)

func (t *gitCheckoutTool) ID() string { return "git_checkout" }

func (t *gitCheckoutTool) Description() string {
	return "Switch to an existing local branch in a workspace repository. " +
		"Rejected if the root resolves to a project memory repo (C2). " +
		"Returns the pre-operation branch and HEAD SHA for undo (git checkout <preOpBranch>)."
}

func (t *gitCheckoutTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root": gitRootProperty(),
			"branch": map[string]any{
				"type":        "string",
				"description": "Name of the existing local branch to switch to",
			},
		},
		"required": []string{"root", "branch"},
	}
}

func (t *gitCheckoutTool) Execute(_ context.Context, c CallInfo, args map[string]any) Result {
	branch, ok := args["branch"].(string)
	if !ok || strings.TrimSpace(branch) == "" {
		return Result{Error: "git_checkout: branch is required"}
	}

	repo, absRoot, err := workspaceWriteRepo(c, args)
	if err != nil {
		return Result{Error: "git_checkout: " + err.Error()}
	}
	defer func() { _ = repo.Close() }()

	preOpBranch, preOpSHA, warn, err := repo.Checkout(branch)
	if err != nil {
		return Result{Error: fmt.Sprintf("git_checkout: %v", err)}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "switched to branch %q in %s\n", branch, absRoot)
	if preOpBranch != "" {
		// Described, not rendered as a command, for the same reason as
		// git_branch: git accepts branch names containing shell
		// metacharacters, so "git checkout <name>" would be an executable line
		// built from an untrusted string.
		fmt.Fprintf(&b, "pre-op branch: %q  pre-op SHA: %s  (undo: switch back to branch %q)", preOpBranch, preOpSHA, preOpBranch)
	}
	if warn != nil {
		fmt.Fprintf(&b, "\nWARNING: checkout succeeded but the HEAD reflog was not updated: %v", warn)
	}
	return Result{Content: b.String()}
}
