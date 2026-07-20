package ui

import (
	"strings"
	"testing"
)

// TestRenderApprovalHidesAlwaysAllowForShell verifies the approval card omits
// the "Always Allow" control for shell_exec. The approval evaluator forces Ask
// for every shell command regardless of a remembered rule, so offering "always"
// would store an ineffective rule and re-prompt the user forever.
func TestRenderApprovalHidesAlwaysAllowForShell(t *testing.T) {
	s := NewServer(0)

	shell := s.renderTaskEvent(TaskEvent{
		Type:       TaskEventApprovalNeeded,
		ToolID:     "shell_exec",
		ApprovalID: "a1",
		Content:    "approve?",
	})
	if strings.Contains(shell, "Always Allow") || strings.Contains(shell, `"decision":"always"`) {
		t.Fatalf("shell_exec approval must not offer Always Allow:\n%s", shell)
	}
	if !strings.Contains(shell, `"decision":"allow"`) || !strings.Contains(shell, `"decision":"reject"`) {
		t.Fatalf("shell_exec approval must still offer Allow and Reject:\n%s", shell)
	}

	fileWrite := s.renderTaskEvent(TaskEvent{
		Type:       TaskEventApprovalNeeded,
		ToolID:     "file_write",
		ApprovalID: "a2",
		Content:    "approve?",
	})
	if !strings.Contains(fileWrite, "Always Allow") || !strings.Contains(fileWrite, `"decision":"always"`) {
		t.Fatalf("non-shell approval should still offer Always Allow:\n%s", fileWrite)
	}
}
