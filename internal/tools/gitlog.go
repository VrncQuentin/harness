package tools

import (
	"context"
	"fmt"
	"strings"
)

const (
	gitLogDefaultN = 20
	gitLogMaxN     = 100
)

// gitLogTool implements the git_log tool: tier-1 read, no gate.
type gitLogTool struct{}

var _ Tool = (*gitLogTool)(nil)

func (t *gitLogTool) ID() string { return "git_log" }

func (t *gitLogTool) Description() string {
	return "List recent commits of a workspace repository, newest first: SHA, date, author, and summary line."
}

func (t *gitLogTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root": gitRootProperty(),
			"n": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Number of commits to list (default %d, max %d)", gitLogDefaultN, gitLogMaxN),
			},
		},
		"required": []string{"root"},
	}
}

func (t *gitLogTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	repo, root, err := workspaceRepo(c, args)
	if err != nil {
		return Result{Error: "git_log: " + err.Error()}
	}
	n := intArg(args, "n")
	if n <= 0 {
		n = gitLogDefaultN
	}
	if n > gitLogMaxN {
		n = gitLogMaxN
	}
	entries, err := repo.Log(n)
	if err != nil {
		return Result{Error: fmt.Sprintf("git_log: %v", err)}
	}
	if len(entries) == 0 {
		return Result{Content: "no commits in " + root}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d commit(s) in %s\n", len(entries), root)
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s %s %s\n", e.SHA[:8], e.When, e.Author, e.Summary)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}
