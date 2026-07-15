package approvals

import (
	"testing"
)

func TestDefaultLayer(t *testing.T) {
	layer := DefaultLayer()
	if layer.Name != "builtin-defaults" {
		t.Fatalf("unexpected layer name: %s", layer.Name)
	}
	seen := make(map[string]int)
	for _, r := range layer.Rules {
		seen[r.ToolID]++
	}
	if seen["file_read"] != 1 {
		t.Errorf("expected file_read rule")
	}
	if seen["file_list"] != 1 {
		t.Errorf("expected file_list rule")
	}
	if seen["file_write"] != 1 {
		t.Errorf("expected file_write rule")
	}
	if seen["shell_exec"] != 1 {
		t.Errorf("expected shell_exec rule")
	}
	if seen["web_search"] != 1 {
		t.Errorf("expected web_search rule")
	}
}

func TestEvaluator_AllowedByDefault(t *testing.T) {
	eval := NewEvaluator(DefaultLayer())
	dec, src := eval.Evaluate("file_read", "")
	if dec != Allowed {
		t.Errorf("file_read should be allowed, got %s", dec)
	}
	if src != "builtin: read-only tools allowed" {
		t.Errorf("unexpected source: %s", src)
	}
}

func TestEvaluator_DestructiveAsks(t *testing.T) {
	eval := NewEvaluator(DefaultLayer())
	dec, _ := eval.Evaluate("file_write", "")
	if dec != Ask {
		t.Errorf("file_write should ask, got %s", dec)
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
	// Layer 2: user config (allow file_write)
	userLayer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "file_write", Decision: Allowed, Source: "user: writes allowed"},
		},
	}
	eval := NewEvaluator(defaults, userLayer)
	dec, src := eval.Evaluate("file_write", "")
	if dec != Allowed {
		t.Errorf("file_write should be allowed by user config, got %s", dec)
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
			{ToolID: "file_write", Decision: Allowed, Source: "user: writes allowed"},
		},
	}
	sessionLayer := Layer{
		Name: "session",
		Rules: []Rule{
			{ToolID: "file_write", Decision: Denied, Source: "session: denied by user"},
		},
	}
	eval := NewEvaluator(defaults, userLayer, sessionLayer)
	dec, src := eval.Evaluate("file_write", "")
	if dec != Denied {
		t.Errorf("file_write should be denied by session, got %s", dec)
	}
	if src != "session: denied by user" {
		t.Errorf("unexpected source: %s", src)
	}
}

func TestEvaluator_CommandPatternMatch(t *testing.T) {
	layer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "shell_exec", CommandPattern: "git ", Decision: Allowed, Source: "user: git allowed"},
			{ToolID: "shell_exec", CommandPattern: "ls", Decision: Allowed, Source: "user: ls allowed"},
		},
	}
	eval := NewEvaluator(DefaultLayer(), layer)

	dec, _ := eval.Evaluate("shell_exec", "git status")
	if dec != Allowed {
		t.Errorf("git status should be allowed, got %s", dec)
	}

	dec, _ = eval.Evaluate("shell_exec", "ls -la")
	if dec != Allowed {
		t.Errorf("ls should be allowed, got %s", dec)
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

func TestClassifyShellCmd_Destructive(t *testing.T) {
	destructive := []string{
		"rm -rf /tmp/test",
		"del C:\\tmp\\file.txt",
		"rd /s C:\\tmp\\old",
		"Remove-Item -Recurse C:\\tmp\\old",
		"ri -Recurse C:\\tmp\\old",
		"rd C:\\tmp\\old",
		"Clear-Content C:\\tmp\\file.txt",
		"clc C:\\tmp\\file.txt",
		"Format-Volume -DriveLetter E",
		"Clear-Disk -Number 1 -RemoveData",
		"Remove-ItemProperty HKCU:\\Software\\Test -Name Value",
		"rp HKCU:\\Software\\Test -Name Value",
		"Stop-Computer -Force",
		"Set-ExecutionPolicy Bypass -Scope LocalMachine",
		"Get-ChildItem | Remove-Item -Recurse",
		"Write-Host ok; Clear-Disk -Number 2",
		"Get-Item . && SeT-ExEcUtIoNpOlIcY RemoteSigned",
		"echo hi & del C:\\tmp\\old.txt",
		"reg delete HKCU\\Software\\Test /f",
		"format C:",
		"rm file.txt",
		"mv /etc/passwd /tmp",
		"chmod 777 /tmp",
		"chown root /tmp",
		"sudo rm -rf /",
		"kill 1234",
		"killall python",
		"shutdown -h now",
		"wget http://example.com",
		"curl http://example.com",
		"nc -l 4444",
		"ssh user@host",
		"git push origin main",
		"git pull",
		"cat /etc/passwd > /etc/shadow",
		"ls && rm -rf /",
		"pkill java",
		"iptables -L",
		"bash -c 'echo hi'",
		"sh script.sh",
	}
	for _, cmd := range destructive {
		if !ClassifyShellCmd(cmd) {
			t.Errorf("%q should be classified as destructive", cmd)
		}
	}
}

func TestClassifyShellCmd_Safe(t *testing.T) {
	safe := []string{
		"ls",
		"ls -la",
		"pwd",
		"whoami",
		"echo hello",
		"cat file.txt",
		"cat /etc/hostname",
		"grep pattern file.txt",
		"find . -name '*.go'",
		"which go",
		"go test ./...",
		"go build",
		"git status",
		"git diff",
		"git log --oneline",
		"Get-ChildItem",
		"Get-Item C:\\tmp\\file.txt",
		"Get-Content C:\\tmp\\file.txt",
		"Set-Location C:\\tmp",
		"wc -l file.txt",
		"head -n 10 file.txt",
		"tail -n 5 file.txt",
		"sort file.txt",
		"uniq file.txt",
		"env",
		"printenv",
		"uname -a",
		"df -h",
		"du -sh .",
		"file file.txt",
		"stat file.txt",
		"date",
		"id",
		"ps aux",
		"top -n 1",
	}
	for _, cmd := range safe {
		if ClassifyShellCmd(cmd) {
			t.Errorf("%q should NOT be classified as destructive", cmd)
		}
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
	if !matchRule(r, "file_write", "") {
		t.Error("wildcard should match file_write")
	}
	if !matchRule(r, "shell_exec", "ls") {
		t.Error("wildcard should match shell_exec")
	}
	if !matchRule(r, "unknown_tool", "") {
		t.Error("wildcard should match unknown_tool")
	}
}

func TestEvaluator_DestructiveCmdRequiresAskEvenWithBroadAllow(t *testing.T) {
	// A broad user-config rule allows "git" prefix, but "git push" is
	// classified as destructive. Without an exact session match it must Ask.
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
	if src != "requires approval: destructive command" {
		t.Errorf("unexpected source: %s", src)
	}
}

func TestEvaluator_DestructiveCmdAllowedWithExactSessionMatch(t *testing.T) {
	eval := NewEvaluator(DefaultLayer())
	eval.AddSessionRule(Rule{
		ToolID:         "shell_exec",
		CommandPattern: "git push origin main",
		Decision:       Allowed,
		Source:         "session: always allowed",
	})
	dec, _ := eval.Evaluate("shell_exec", "git push origin main")
	if dec != Allowed {
		t.Errorf("exact session match should allow destructive git push, got %s", dec)
	}
	// A different destructive command still asks.
	dec, _ = eval.Evaluate("shell_exec", "rm -rf /tmp")
	if dec != Ask {
		t.Errorf("rm -rf should still Ask, got %s", dec)
	}
}

func TestEvaluator_SafeCmdAllowedByBroadRule(t *testing.T) {
	userLayer := Layer{
		Name: "user-config",
		Rules: []Rule{
			{ToolID: "shell_exec", CommandPattern: "git", Decision: Allowed, Source: "user: git allowed"},
		},
	}
	eval := NewEvaluator(DefaultLayer(), userLayer)
	dec, _ := eval.Evaluate("shell_exec", "git status")
	if dec != Allowed {
		t.Errorf("safe git status should be allowed by broad rule, got %s", dec)
	}
}

func TestEvaluator_DestructiveCwdCommandsNotBypassable(t *testing.T) {
	// Even with a broad "git" prefix allow, destructive git commands still require approval.
	eval := NewEvaluator(DefaultLayer())
	eval.AddSessionRule(Rule{
		ToolID:         "shell_exec",
		CommandPattern: "git status",
		Decision:       Allowed,
		Source:         "session: always allowed",
	})
	// git status is safe and stored as exact match → allowed.
	dec, _ := eval.Evaluate("shell_exec", "git status")
	if dec != Allowed {
		t.Errorf("git status should be allowed by exact session rule, got %s", dec)
	}
	// git push does not match the exact pattern → classified destructive → Ask.
	dec, _ = eval.Evaluate("shell_exec", "git push origin main")
	if dec != Ask {
		t.Errorf("git push should Ask despite git status session rule, got %s", dec)
	}
}

func TestApprovalResponse(t *testing.T) {
	r1 := ApprovalResponse{Decision: Allowed, Remember: false}
	r2 := ApprovalResponse{Decision: Allowed, Remember: true}
	r3 := ApprovalResponse{Decision: Denied, Remember: false}
	if r1.Remember {
		t.Error("allow once should not remember")
	}
	if !r2.Remember {
		t.Error("always should remember")
	}
	if r3.Remember {
		t.Error("reject should not remember")
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
