package tools

import (
	"context"
	"fmt"
	"strings"
)

// ghPRCreateTool is a tier-3 proposal tool: returns a proposal for creating a
// GitHub PR rather than executing it. No GitHub API call is made.
type ghPRCreateTool struct{}

var _ Tool = (*ghPRCreateTool)(nil)

func (t *ghPRCreateTool) ID() string { return "gh_pr_create" }

func (t *ghPRCreateTool) Description() string {
	return "Returns a proposal describing a GitHub PR creation command for human review. " +
		"Does NOT create the PR — the proposal must be reviewed and executed by the user."
}

func (t *ghPRCreateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner (org or username)"},
			"repo":  map[string]any{"type": "string", "description": "Repository name"},
			"title": map[string]any{"type": "string", "description": "Pull request title"},
			"head":  map[string]any{"type": "string", "description": "Head branch (source, e.g. feat/my-feature)"},
			"base":  map[string]any{"type": "string", "description": "Base branch (target, default: main)"},
			"body":  map[string]any{"type": "string", "description": "Pull request body in Markdown (optional)"},
		},
		"required": []string{"owner", "repo", "title", "head"},
	}
}

func (t *ghPRCreateTool) Execute(_ context.Context, _ CallInfo, args map[string]any) Result {
	owner, ok := args["owner"].(string)
	if !ok || strings.TrimSpace(owner) == "" {
		return Result{Error: "gh_pr_create: owner is required"}
	}
	repo, ok := args["repo"].(string)
	if !ok || strings.TrimSpace(repo) == "" {
		return Result{Error: "gh_pr_create: repo is required"}
	}
	title, ok := args["title"].(string)
	if !ok || strings.TrimSpace(title) == "" {
		return Result{Error: "gh_pr_create: title is required"}
	}
	head, ok := args["head"].(string)
	if !ok || strings.TrimSpace(head) == "" {
		return Result{Error: "gh_pr_create: head is required"}
	}
	base := "main"
	if b, ok := args["base"].(string); ok && strings.TrimSpace(b) != "" {
		base = strings.TrimSpace(b)
	}
	body, _ := args["body"].(string)

	cmd := fmt.Sprintf("gh pr create --title %q --head %s --base %s --repo %s/%s",
		strings.TrimSpace(title), strings.TrimSpace(head), base,
		strings.TrimSpace(owner), strings.TrimSpace(repo))
	if strings.TrimSpace(body) != "" {
		cmd += ` --body "..."`
	}

	content := fmt.Sprintf("PROPOSAL: gh pr create\n\n"+
		"Repository: %s/%s\n"+
		"Head:       %s → %s\n"+
		"Title:      %s\n"+
		"Command:    %s\n\n"+
		"This action requires human execution. Run the command above or use the GitHub UI.\n",
		strings.TrimSpace(owner), strings.TrimSpace(repo),
		strings.TrimSpace(head), base,
		strings.TrimSpace(title),
		cmd)
	return Result{Content: content, Proposal: true}
}
