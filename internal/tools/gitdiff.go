package tools

import (
	"context"
	"fmt"
	"strings"
)

// gitDiffOutputLimit caps diff text injected into context. The governor's B2
// folder (M10.2) compresses within this bound; the cap protects the context
// window regardless.
const gitDiffOutputLimit = 64 * 1024

// gitDiffTool implements the git_diff tool: tier-1 read, no gate.
type gitDiffTool struct{}

var _ Tool = (*gitDiffTool)(nil)

func (t *gitDiffTool) ID() string { return "git_diff" }

func (t *gitDiffTool) Description() string {
	return "Unified diff of a workspace repository. Default: HEAD vs the working tree (uncommitted changes). Pass from (and optionally to) for a revision diff, e.g. from=HEAD~1."
}

func (t *gitDiffTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root": gitRootProperty(),
			"from": map[string]any{
				"type":        "string",
				"description": "Base revision (e.g. HEAD~1, a SHA, a branch). Omit to diff HEAD against the working tree.",
			},
			"to": map[string]any{
				"type":        "string",
				"description": "Target revision. Defaults to HEAD when from is set.",
			},
		},
		"required": []string{"root"},
	}
}

func (t *gitDiffTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	repo, root, err := workspaceRepo(c, args)
	if err != nil {
		return Result{Error: "git_diff: " + err.Error()}
	}
	from, _ := args["from"].(string)
	to, _ := args["to"].(string)

	var diff string
	switch {
	case from == "" && to == "":
		diff, err = repo.DiffWorktree(ctx)
	case from == "":
		return Result{Error: "git_diff: to requires from"}
	default:
		if to == "" {
			to = "HEAD"
		}
		diff, err = repo.DiffCommits(ctx, from, to)
	}
	if err != nil {
		return Result{Error: fmt.Sprintf("git_diff: %v", err)}
	}
	if strings.TrimSpace(diff) == "" {
		return Result{Content: "no changes in " + root}
	}
	if len(diff) > gitDiffOutputLimit {
		diff = diff[:gitDiffOutputLimit] + "\n... (diff truncated)"
	}
	return Result{Content: diff}
}
