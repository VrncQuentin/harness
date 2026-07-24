package tools

import (
	"errors"
	"reflect"
	"testing"
)

func TestRegistry_DuplicateReturnsError(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&readTool{}); err != nil {
		t.Fatalf("Register first tool: %v", err)
	}
	if err := r.Register(&readTool{}); !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("Register duplicate error = %v, want ErrDuplicateTool", err)
	}
}

func TestBuiltinDescriptorsDefinePolicyMetadata(t *testing.T) {
	descriptors := BuiltinDescriptors()
	want := []Descriptor{
		{ID: "read", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "file_list", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "ast_map", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "ast_find", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "git_status", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "git_diff", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "git_log", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "edit", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: edits require approval"},
		{ID: "exec", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: exec commands require approval"},
		{ID: "go_test", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: go_test runs the test suite"},
		{ID: "web_search", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: web search uses the network"},
	}
	if !reflect.DeepEqual(descriptors, want) {
		t.Fatalf("BuiltinDescriptors() = %#v, want %#v", descriptors, want)
	}
	for _, desc := range descriptors {
		got, ok := BuiltinDescriptor(desc.ID)
		if !ok {
			t.Fatalf("BuiltinDescriptor(%q) not found", desc.ID)
		}
		if got != desc {
			t.Fatalf("BuiltinDescriptor(%q) = %#v, want %#v", desc.ID, got, desc)
		}
		if BuiltinDefaultEnabled(desc.ID) != desc.DefaultEnabled {
			t.Fatalf("BuiltinDefaultEnabled(%q) = %v, want %v", desc.ID, BuiltinDefaultEnabled(desc.ID), desc.DefaultEnabled)
		}
	}
}

func TestRegistry_ListAndGet(t *testing.T) {
	r := NewRegistry()
	if err := RegisterBuiltins(r); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	all := r.List()
	if len(all) != 11 {
		t.Fatalf("expected 11 tools, got %d", len(all))
	}
	for _, id := range []string{"read", "file_list", "ast_map", "ast_find", "git_status", "git_diff", "git_log", "edit", "exec", "go_test", "web_search"} {
		if r.Get(id) == nil {
			t.Errorf("%s not found", id)
		}
	}
}
