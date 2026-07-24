package tools

import (
	"context"
	"fmt"
	"strings"
)

// gitCommitTool implements the git_commit tool: tier-2 local write.
// It requires explicit approval and is rejected for memory-repo roots (C2).
type gitCommitTool struct{}

var _ Tool = (*gitCommitTool)(nil)

func (t *gitCommitTool) ID() string { return "git_commit" }

func (t *gitCommitTool) Description() string {
	return "Stage files and create a git commit in a workspace repository. " +
		"Rejected if the root resolves to a project memory repo (C2). " +
		"Returns the new SHA and the pre-operation SHA for undo."
}

func (t *gitCommitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"root": gitRootProperty(),
			"message": map[string]any{
				"type":        "string",
				"description": "Commit message",
			},
			"files": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Files to stage (repo-relative, forward slashes). Omit or pass [] to stage all changes (git add -A).",
			},
		},
		"required": []string{"root", "message"},
	}
}

func (t *gitCommitTool) Execute(_ context.Context, c CallInfo, args map[string]any) Result {
	msg, ok := args["message"].(string)
	if !ok || strings.TrimSpace(msg) == "" {
		return Result{Error: "git_commit: message is required"}
	}

	repo, absRoot, err := workspaceWriteRepo(c, args)
	if err != nil {
		return Result{Error: "git_commit: " + err.Error()}
	}

	var files []string
	if raw, ok := args["files"]; ok {
		switch v := raw.(type) {
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					files = append(files, s)
				}
			}
		case []string:
			files = v
		}
	}

	if err := repo.WorkspaceStage(files); err != nil {
		return Result{Error: fmt.Sprintf("git_commit: stage %s: %v", absRoot, err)}
	}

	newSHA, preOpSHA, err := repo.WorkspaceCommit(msg)
	if err != nil {
		return Result{Error: fmt.Sprintf("git_commit: %v", err)}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "committed %s in %s\n", newSHA, absRoot)
	if preOpSHA != "" {
		fmt.Fprintf(&b, "pre-op SHA: %s  (undo: git reset --hard %s)", preOpSHA, preOpSHA)
	} else {
		b.WriteString("initial commit — no pre-op SHA")
	}
	return Result{Content: b.String()}
}
