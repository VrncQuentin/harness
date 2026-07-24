package approvals

import (
	"testing"

	"github.com/vrnc/harness/internal/tools"
)

func TestDefaultLayer(t *testing.T) {
	layer := DefaultLayer()
	if layer.Name != "builtin-defaults" {
		t.Fatalf("unexpected layer name: %s", layer.Name)
	}
	descriptors := tools.BuiltinDescriptors()
	if len(layer.Rules) != len(descriptors) {
		t.Fatalf("default rules = %d, want %d descriptor rules", len(layer.Rules), len(descriptors))
	}
	for i, desc := range descriptors {
		rule := layer.Rules[i]
		if rule.ToolID != desc.ID {
			t.Errorf("rule[%d].ToolID = %q, want %q", i, rule.ToolID, desc.ID)
		}
		if rule.Source != desc.DefaultApprovalSource {
			t.Errorf("rule[%d].Source = %q, want %q", i, rule.Source, desc.DefaultApprovalSource)
		}
		wantDecision := Ask
		if desc.DefaultApproval == tools.ApprovalDefaultAllow {
			wantDecision = Allowed
		}
		if rule.Decision != wantDecision {
			t.Errorf("rule[%d].Decision = %s, want %s", i, rule.Decision, wantDecision)
		}
	}
}

func TestEvaluator_AllowedByDefault(t *testing.T) {
	eval := NewEvaluator(DefaultLayer())
	dec, src := eval.Evaluate("read", "")
	if dec != Allowed {
		t.Errorf("read should be allowed, got %s", dec)
	}
	if src != "builtin: read-only tools allowed" {
		t.Errorf("unexpected source: %s", src)
	}
}

func TestEvaluator_DestructiveAsks(t *testing.T) {
	eval := NewEvaluator(DefaultLayer())
	dec, _ := eval.Evaluate("edit", "")
	if dec != Ask {
		t.Errorf("edit should ask, got %s", dec)
	}
	dec, _ = eval.Evaluate("shell_exec", "ls")
	if dec != Ask {
		t.Errorf("shell_exec should ask, got %s", dec)
	}
	dec, _ = eval.Evaluate("web_search", "")
	if dec != Ask {
		t.Errorf("web_search should ask, got %s", dec)
	}
}

func TestEvaluator_LayeredOverride(t *testing.T) {
	// Layer 1: agent defaults (destructive = ask)
	defaults := DefaultLayer()
	// Layer 2: user config (allow edit)
	userLayer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "edit", Decision: Allowed, Source: "user: writes allowed"},
		},
	}
	eval := NewEvaluator(defaults, userLayer)
	dec, src := eval.Evaluate("edit", "")
	if dec != Allowed {
		t.Errorf("edit should be allowed by user config, got %s", dec)
	}
	if src != "user: writes allowed" {
		t.Errorf("unexpected source: %s", src)
	}
	// shell_exec still asks
	dec, _ = eval.Evaluate("shell_exec", "ls")
	if dec != Ask {
		t.Errorf("shell_exec should still ask, got %s", dec)
	}
}

func TestEvaluator_SessionOverridesConfig(t *testing.T) {
	defaults := DefaultLayer()
	userLayer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "edit", Decision: Allowed, Source: "user: writes allowed"},
		},
	}
	sessionLayer := Layer{
		Name: "session",
		Rules: []Rule{
			{ToolID: "edit", Decision: Denied, Source: "session: denied by user"},
		},
	}
	eval := NewEvaluator(defaults, userLayer, sessionLayer)
	dec, src := eval.Evaluate("edit", "")
	if dec != Denied {
		t.Errorf("edit should be denied by session, got %s", dec)
	}
	if src != "session: denied by user" {
		t.Errorf("unexpected source: %s", src)
	}
}

func TestEvaluator_ShellCommandPatternAllowsStillAsk(t *testing.T) {
	layer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "shell_exec", CommandPattern: "git ", Decision: Allowed, Source: "user: git allowed"},
			{ToolID: "shell_exec", CommandPattern: "ls", Decision: Allowed, Source: "user: ls allowed"},
		},
	}
	eval := NewEvaluator(DefaultLayer(), layer)

	dec, _ := eval.Evaluate("shell_exec", "git status")
	if dec != Ask {
		t.Errorf("git status should still Ask despite allow rule, got %s", dec)
	}

	dec, _ = eval.Evaluate("shell_exec", "ls -la")
	if dec != Ask {
		t.Errorf("ls should still Ask despite allow rule, got %s", dec)
	}

	// rm is not matched by any user rule, falls back to Ask
	dec, _ = eval.Evaluate("shell_exec", "rm -rf /tmp/test")
	if dec != Ask {
		t.Errorf("rm should still ask, got %s", dec)
	}
}

func TestEvaluator_UnknownTool(t *testing.T) {
	eval := NewEvaluator(DefaultLayer())
	dec, _ := eval.Evaluate("unknown_tool", "")
	if dec != Ask {
		t.Errorf("unknown tools should ask by default, got %s", dec)
	}
}

func TestDecision_String(t *testing.T) {
	if Allowed.String() != "allowed" {
		t.Errorf("Allowed.String() = %q", Allowed.String())
	}
	if Denied.String() != "denied" {
		t.Errorf("Denied.String() = %q", Denied.String())
	}
	if Ask.String() != "ask" {
		t.Errorf("Ask.String() = %q", Ask.String())
	}
}

func TestMatchRule_WildcardTool(t *testing.T) {
	r := Rule{ToolID: "*", Decision: Denied, Source: "test"}
	if !matchRule(r, "edit", "") {
		t.Error("wildcard should match edit")
	}
	if !matchRule(r, "shell_exec", "ls") {
		t.Error("wildcard should match shell_exec")
	}
	if !matchRule(r, "unknown_tool", "") {
		t.Error("wildcard should match unknown_tool")
	}
}

func TestEvaluator_ShellCmdRequiresAskEvenWithBroadAllow(t *testing.T) {
	// A broad user-config rule allows "git" prefix, but "git push" is
	// a shell command. Shell commands must Ask even when they look safe.
	userLayer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "shell_exec", CommandPattern: "git", Decision: Allowed, Source: "user: git allowed"},
		},
	}
	eval := NewEvaluator(DefaultLayer(), userLayer)
	dec, src := eval.Evaluate("shell_exec", "git push origin main")
	if dec != Ask {
		t.Errorf("destructive git push should Ask even with broad git allow, got %s", dec)
	}
	if src != "requires approval: shell command" {
		t.Errorf("unexpected source: %s", src)
	}
}

func TestEvaluator_ShellCmdStillAsksWithExactSessionAllow(t *testing.T) {
	eval := NewEvaluator(DefaultLayer())
	eval.AddSessionRule(Rule{
		ToolID:         "shell_exec",
		CommandPattern: "git push origin main",
		Decision:       Allowed,
		Source:         "session: always allowed",
	})
	dec, _ := eval.Evaluate("shell_exec", "git push origin main")
	if dec != Ask {
		t.Errorf("exact session allow should still Ask for shell command, got %s", dec)
	}
	// A different destructive command still asks.
	dec, _ = eval.Evaluate("shell_exec", "rm -rf /tmp")
	if dec != Ask {
		t.Errorf("rm -rf should still Ask, got %s", dec)
	}
}

func TestEvaluator_SafeShellCmdStillAsksWithBroadRule(t *testing.T) {
	userLayer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "shell_exec", CommandPattern: "git", Decision: Allowed, Source: "user: git allowed"},
		},
	}
	eval := NewEvaluator(DefaultLayer(), userLayer)
	dec, _ := eval.Evaluate("shell_exec", "git status")
	if dec != Ask {
		t.Errorf("safe-looking shell command should still Ask by broad rule, got %s", dec)
	}
}

func TestEvaluator_ShellSessionAllowsNotBypassable(t *testing.T) {
	// Even with a remembered exact shell allow, shell commands still require approval.
	eval := NewEvaluator(DefaultLayer())
	eval.AddSessionRule(Rule{
		ToolID:         "shell_exec",
		CommandPattern: "git status",
		Decision:       Allowed,
		Source:         "session: always allowed",
	})
	// git status is stored as an exact match, but still asks.
	dec, _ := eval.Evaluate("shell_exec", "git status")
	if dec != Ask {
		t.Errorf("git status should still Ask despite exact session rule, got %s", dec)
	}
	// git push also asks.
	dec, _ = eval.Evaluate("shell_exec", "git push origin main")
	if dec != Ask {
		t.Errorf("git push should Ask despite git status session rule, got %s", dec)
	}
}

func TestEvaluator_DestructiveCmdDeniedRuleWins(t *testing.T) {
	userLayer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "shell_exec", CommandPattern: "rm -rf /tmp/test", Decision: Denied, Source: "user: denied exact command"},
		},
	}
	eval := NewEvaluator(DefaultLayer(), userLayer)
	dec, src := eval.Evaluate("shell_exec", "rm -rf /tmp/test")
	if dec != Denied {
		t.Errorf("destructive denied rule should deny, got %s", dec)
	}
	if src != "user: denied exact command" {
		t.Errorf("unexpected source: %s", src)
	}
}
