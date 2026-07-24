package tools

import (
	"context"
	"strings"
	"testing"
)

func TestGHPRMergeTool_MissingRequired(t *testing.T) {
	tool := &ghPRMergeTool{}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing owner", map[string]any{"repo": "r", "pr_number": float64(1)}, "owner"},
		{"missing repo", map[string]any{"owner": "o", "pr_number": float64(1)}, "repo"},
		{"missing pr_number", map[string]any{"owner": "o", "repo": "r"}, "pr_number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tool.Execute(context.Background(), CallInfo{}, tc.args)
			if res.Error == "" {
				t.Fatal("expected error")
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("error %q should mention %q", res.Error, tc.want)
			}
		})
	}
}

func TestGHPRMergeTool_InvalidMethod(t *testing.T) {
	tool := &ghPRMergeTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{
		"owner": "o", "repo": "r", "pr_number": float64(42), "method": "fast-forward",
	})
	if res.Error == "" || !strings.Contains(res.Error, "method") {
		t.Errorf("expected method error, got %q", res.Error)
	}
}

func TestGHPRMergeTool_Proposal(t *testing.T) {
	tool := &ghPRMergeTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{
		"owner": "VrncQuentin", "repo": "harness", "pr_number": float64(42),
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.Proposal {
		t.Error("result.Proposal should be true for gh_pr_merge")
	}
	for _, want := range []string{"PROPOSAL", "VrncQuentin/harness", "#42", "squash"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q: %s", want, res.Content)
		}
	}
}

func TestGHPRMergeTool_MethodVariants(t *testing.T) {
	tool := &ghPRMergeTool{}
	for _, method := range []string{"merge", "squash", "rebase"} {
		res := tool.Execute(context.Background(), CallInfo{}, map[string]any{
			"owner": "o", "repo": "r", "pr_number": float64(1), "method": method,
		})
		if res.Error != "" {
			t.Errorf("method %s: unexpected error: %s", method, res.Error)
		}
		if !strings.Contains(res.Content, "--"+method) {
			t.Errorf("method %s: content should contain --%s: %s", method, method, res.Content)
		}
	}
}

func TestGHPRMergeTool_NoDeleteBranch(t *testing.T) {
	tool := &ghPRMergeTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{
		"owner": "o", "repo": "r", "pr_number": float64(1), "delete_branch": false,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if strings.Contains(res.Content, "--delete-branch") {
		t.Error("content should not contain --delete-branch when delete_branch=false")
	}
}
