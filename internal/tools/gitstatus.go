package tools

import (
	"context"
	"fmt"
	"strings"
)

// gitStatusTool implements the git_status tool: tier-1 read, no gate.
type gitStatusTool struct{}

var _ Tool = (*gitStatusTool)(nil)

func (t *gitStatusTool) ID() string { return "git_status" }

func (t *gitStatusTool) Description() string {
	return "Show the git worktree status of a workspace repository (porcelain-style: staging and worktree codes per changed path)."
}

func (t *gitStatusTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root": gitRootProperty(),
		},
		"required": []string{"root"},
	}
}

func (t *gitStatusTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	repo, root, err := workspaceRepo(c, args)
	if err != nil {
		return Result{Error: "git_status: " + err.Error()}
	}
	defer func() { _ = repo.Close() }()
	entries, err := repo.Status()
	if err != nil {
		return Result{Error: fmt.Sprintf("git_status: %v", err)}
	}
	if len(entries) == 0 {
		return Result{Content: "clean: no uncommitted changes in " + root}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d changed path(s) in %s\n", len(entries), root)
	for _, e := range entries {
		fmt.Fprintf(&b, "%c%c %s\n", e.Staging, e.Worktree, e.Path)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}
