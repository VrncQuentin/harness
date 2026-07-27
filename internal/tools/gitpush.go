package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitw "github.com/VrncQuentin/harness/internal/git"
)

// gitPushTool is a tier-3 proposal tool: it validates inputs and returns a
// push proposal for human review rather than executing the push. The
// enforcement is in the return type — Content describes what WOULD happen;
// no network call is made.
type gitPushTool struct{}

var _ Tool = (*gitPushTool)(nil)

func (t *gitPushTool) ID() string { return "git_push" }

func (t *gitPushTool) Description() string {
	return "Returns a proposal describing a git push command for human review and execution. " +
		"Does NOT push — the proposal must be executed by the user. " +
		"Rejected if root resolves to a project memory repo (C2)."
}

func (t *gitPushTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root":   gitRootProperty(),
			"remote": map[string]any{"type": "string", "description": "Remote name (default: origin)"},
			"branch": map[string]any{"type": "string", "description": "Branch to push. Omit to use the current HEAD branch."},
			"force":  map[string]any{"type": "boolean", "description": "Force-push flag (default: false). Use with caution."},
		},
		"required": []string{"root"},
	}
}

func (t *gitPushTool) Execute(_ context.Context, c CallInfo, args map[string]any) Result {
	repo, absRoot, err := workspaceWriteRepo(c, args)
	if err != nil {
		return Result{Error: "git_push: " + err.Error()}
	}

	remote := "origin"
	if r, ok := args["remote"].(string); ok && strings.TrimSpace(r) != "" {
		remote = strings.TrimSpace(r)
	}

	branch, ok := args["branch"].(string)
	if !ok || strings.TrimSpace(branch) == "" {
		// Asked of the repository already opened and scope-checked above,
		// rather than by reading absRoot/.git/HEAD — which resolved the
		// repository's pathname a second time after it had been authorized, and
		// assumed .git is a directory, which it is not in a linked worktree.
		branch, err = repo.CurrentBranch()
		if err != nil {
			var detached *gitw.ErrDetachedHEAD
			if errors.As(err, &detached) {
				return Result{Error: fmt.Sprintf("git_push: %s — specify branch explicitly", detached.Error())}
			}
			return Result{Error: "git_push: " + err.Error()}
		}
	} else {
		branch = strings.TrimSpace(branch)
	}

	force := false
	if f, ok := args["force"].(bool); ok {
		force = f
	}

	cmd := buildPushCommand(remote, branch, force)
	content := fmt.Sprintf("PROPOSAL: git push\n\n"+
		"Repository: %s\n"+
		"Command:    %s\n\n"+
		"This action requires human execution. Run the command above in the repository directory "+
		"or use your preferred Git client.\n", absRoot, cmd)
	return Result{Content: content, Proposal: true}
}

func buildPushCommand(remote, branch string, force bool) string {
	if force {
		return fmt.Sprintf("git push --force-with-lease %s %s", remote, branch)
	}
	return fmt.Sprintf("git push %s %s", remote, branch)
}
