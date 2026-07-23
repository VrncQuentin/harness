package tools

import (
	"errors"
	"reflect"
	"testing"
)

func TestRegistry_DuplicateReturnsError(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fileReadTool{}); err != nil {
		t.Fatalf("Register first tool: %v", err)
	}
	if err := r.Register(&fileReadTool{}); !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("Register duplicate error = %v, want ErrDuplicateTool", err)
	}
}

func TestBuiltinDescriptorsDefinePolicyMetadata(t *testing.T) {
	descriptors := BuiltinDescriptors()
	want := []Descriptor{
		{ID: "file_read", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "file_list", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "ast_map", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "ast_find", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
		{ID: "file_write", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: writes require approval"},
		{ID: "shell_exec", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: shell commands require approval"},
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
	if len(all) != 7 {
		t.Fatalf("expected 7 tools, got %d", len(all))
	}
	for _, id := range []string{"file_read", "file_list", "ast_map", "ast_find", "file_write", "shell_exec", "web_search"} {
		if r.Get(id) == nil {
			t.Errorf("%s not found", id)
		}
	}
}
