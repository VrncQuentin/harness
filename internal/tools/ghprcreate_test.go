package tools

import (
	"context"
	"strings"
	"testing"
)

func TestGHPRCreateTool_MissingRequired(t *testing.T) {
	tool := &ghPRCreateTool{}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing owner", map[string]any{"repo": "r", "title": "t", "head": "feat/x"}, "owner"},
		{"missing repo", map[string]any{"owner": "o", "title": "t", "head": "feat/x"}, "repo"},
		{"missing title", map[string]any{"owner": "o", "repo": "r", "head": "feat/x"}, "title"},
		{"missing head", map[string]any{"owner": "o", "repo": "r", "title": "t"}, "head"},
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

func TestGHPRCreateTool_Proposal(t *testing.T) {
	tool := &ghPRCreateTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{
		"owner": "VrncQuentin",
		"repo":  "harness",
		"title": "feat: add thing",
		"head":  "feat/add-thing",
		"base":  "main",
		"body":  "Some details here.",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.Proposal {
		t.Error("result.Proposal should be true for gh_pr_create")
	}
	for _, want := range []string{"PROPOSAL", "VrncQuentin/harness", "feat/add-thing", "main", "feat: add thing"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q: %s", want, res.Content)
		}
	}
}

func TestGHPRCreateTool_DefaultBase(t *testing.T) {
	tool := &ghPRCreateTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{
		"owner": "o", "repo": "r", "title": "t", "head": "feat/x",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "main") {
		t.Errorf("default base should be 'main': %s", res.Content)
	}
}
