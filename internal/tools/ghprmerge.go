package tools

import (
	"context"
	"fmt"
	"strings"
)

// ghPRMergeTool is a tier-3 proposal tool: returns a proposal for merging a
// GitHub PR rather than executing it. No GitHub API call is made.
type ghPRMergeTool struct{}

var _ Tool = (*ghPRMergeTool)(nil)

func (t *ghPRMergeTool) ID() string { return "gh_pr_merge" }

func (t *ghPRMergeTool) Description() string {
	return "Returns a proposal describing a GitHub PR merge command for human review. " +
		"Does NOT merge the PR — the proposal must be reviewed and executed by the user."
}

func (t *ghPRMergeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner":         map[string]any{"type": "string", "description": "Repository owner (org or username)"},
			"repo":          map[string]any{"type": "string", "description": "Repository name"},
			"pr_number":     map[string]any{"type": "integer", "description": "Pull request number"},
			"method":        map[string]any{"type": "string", "enum": []string{"merge", "squash", "rebase"}, "description": "Merge strategy (default: squash)"},
			"delete_branch": map[string]any{"type": "boolean", "description": "Delete source branch after merging (default: true)"},
		},
		"required": []string{"owner", "repo", "pr_number"},
	}
}

func (t *ghPRMergeTool) Execute(_ context.Context, _ CallInfo, args map[string]any) Result {
	owner, ok := args["owner"].(string)
	if !ok || strings.TrimSpace(owner) == "" {
		return Result{Error: "gh_pr_merge: owner is required"}
	}
	repo, ok := args["repo"].(string)
	if !ok || strings.TrimSpace(repo) == "" {
		return Result{Error: "gh_pr_merge: repo is required"}
	}
	prNum, ok := args["pr_number"].(float64)
	if !ok || prNum <= 0 {
		return Result{Error: "gh_pr_merge: pr_number must be a positive integer"}
	}

	method := "squash"
	if m, ok := args["method"].(string); ok && strings.TrimSpace(m) != "" {
		method = strings.TrimSpace(m)
		switch method {
		case "merge", "squash", "rebase":
		default:
			return Result{Error: fmt.Sprintf("gh_pr_merge: method must be merge, squash, or rebase (got %q)", method)}
		}
	}

	deleteBranch := true
	if d, ok := args["delete_branch"].(bool); ok {
		deleteBranch = d
	}

	flag := "--" + method
	cmd := fmt.Sprintf("gh pr merge %d %s --repo %s/%s",
		int(prNum), flag,
		strings.TrimSpace(owner), strings.TrimSpace(repo))
	if deleteBranch {
		cmd += " --delete-branch"
	}

	content := fmt.Sprintf("PROPOSAL: gh pr merge\n\n"+
		"Repository: %s/%s\n"+
		"PR:         #%d\n"+
		"Method:     %s\n"+
		"Delete branch: %v\n"+
		"Command:    %s\n\n"+
		"This action requires human execution. Run the command above or use the GitHub UI.\n",
		strings.TrimSpace(owner), strings.TrimSpace(repo),
		int(prNum), method, deleteBranch, cmd)
	return Result{Content: content, Proposal: true}
}
