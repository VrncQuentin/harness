// Package approvals provides the M7 permission engine: layered rules,
// destructive-command classification, and once/always/reject decisions
// evaluated within the agent loop at tool-dispatch time.
package approvals

import (
	"strings"
	"sync"
)

// Decision is the outcome of a permission check for a single tool call.
type Decision int

const (
	// Allowed means the tool call may proceed without user interaction.
	Allowed Decision = iota
	// Denied means the tool call is blocked; the loop injects a
	// tool-error result and continues.
	Denied
	// Ask means the tool call requires user approval before it can run.
	Ask
)

// String returns a compact representation for audit trails.
func (d Decision) String() string {
	switch d {
	case Allowed:
		return "allowed"
	case Denied:
		return "denied"
	case Ask:
		return "ask"
	default:
		return "unknown"
	}
}

// Rule is a single permission entry. It matches on tool id and optional
// command arguments (for shell_exec). The decision applies when the rule
// is the best match within its layer.
type Rule struct {
	// ToolID is the tool this rule applies to (e.g. "file_write",
	// "shell_exec", or "*" for any tool).
	ToolID string
	// CommandPattern is an optional prefix or wildcard pattern matched
	// against shell_exec command strings. Empty means "any command".
	CommandPattern string
	// Decision is the outcome when this rule is the best match.
	Decision Decision
	// Source records where this rule came from for audit trails.
	Source string
}

// Layer is a named, ordered group of rules. Rules are evaluated in order
// within a layer; the first match wins per layer.
type Layer struct {
	Name  string
	Rules []Rule
}

// Evaluator evaluates permission decisions across ordered layers.
// When the best-match decision in a layer is Ask, evaluation stops
// and the caller must present the approval prompt. Denied in any
// layer takes precedence over Ask in earlier layers; the tool call
// is blocked immediately.
type Evaluator struct {
	mu      sync.Mutex
	layers  []Layer
	session *Layer // mutable session layer, appended to on "always"
}

// NewEvaluator creates an Evaluator with the given layers. Layers are
// evaluated in order: agent defaults → user config → session approvals,
// with last-match-wins semantics.
//
// Use AddSessionRule to append runtime decisions during a session
// (e.g. when the user clicks "Always Allow").
func NewEvaluator(layers ...Layer) *Evaluator {
	session := &Layer{Name: "session"}
	return &Evaluator{
		layers:  layers,
		session: session,
	}
}

// AddSessionRule appends a runtime rule to the session layer. Session
// rules are evaluated after all configured layers and take precedence.
// Call this when the user chooses "always allow" or "always reject".
func (e *Evaluator) AddSessionRule(r Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.session.Rules = append(e.session.Rules, r)
}

// Evaluate checks whether toolID with commandArg (for shell_exec) is
// permitted. Returns the decision and the matching Rule source for
// audit trails.
func (e *Evaluator) Evaluate(toolID, commandArg string) (Decision, string) {
	e.mu.Lock()
	effectiveLayers := append([]Layer{}, e.layers...)
	if len(e.session.Rules) > 0 {
		effectiveLayers = append(effectiveLayers, *e.session)
	}
	e.mu.Unlock()

	// Evaluate layers in order (agent defaults → user config → session).
	// Last layer with a matching rule wins.
	best := Decision(Allowed)
	source := "default"
	for _, layer := range effectiveLayers {
		for _, rule := range layer.Rules {
			if !matchRule(rule, toolID, commandArg) {
				continue
			}
			best = rule.Decision
			source = rule.Source
			break // first match in this layer wins
		}
	}
	return best, source
}

// matchRule checks whether r matches the given toolID and optional
// command argument.
func matchRule(r Rule, toolID, commandArg string) bool {
	if r.ToolID != "*" && r.ToolID != toolID {
		return false
	}
	if r.CommandPattern == "" {
		return true
	}
	if toolID != "shell_exec" {
		return r.ToolID == "shell_exec"
	}
	return strings.HasPrefix(commandArg, r.CommandPattern) || commandArg == r.CommandPattern
}

// ClassifyShellCmd categorizes a shell command string. Returns true
// when the command is classified as destructive and should require
// approval.
func ClassifyShellCmd(command string) bool {
	cmd := strings.TrimSpace(command)
	cmdLower := strings.ToLower(cmd)

	// Destructive patterns that should always require approval.
	destructive := []string{
		"rm ", "rm -", "rmdir",
		"mv ", "cp -r", "cp -R",
		"chmod ", "chown ",
		"sudo ", "su ",
		"> /", ">> /", "> ~/",
		"dd if=",
		"mkfs", "mkswap",
		"kill ", "killall", "pkill",
		"shutdown", "reboot", "halt", "poweroff",
		"init 0", "init 6",
		"iptables", "nft ",
		":(){ :|:& };:", // fork bomb
		"wget ", "curl ",
		"nc ", "ncat ",
		"ssh ", "scp ",
		"git push", "git pull",
		"bash ", "sh ",
	}
	for _, pattern := range destructive {
		if strings.HasPrefix(cmdLower, pattern) {
			return true
		}
		// Also match when destructive command appears after a pipeline.
		if strings.Contains(cmdLower, "| "+pattern) || strings.Contains(cmdLower, "; "+pattern) || strings.Contains(cmdLower, "&& "+pattern) {
			return true
		}
	}

	// Redirect-to-file outside sandbox is destructive.
	if strings.Contains(cmd, "> /etc") || strings.Contains(cmd, "> /dev") {
		return true
	}

	return false
}

// DefaultLayer returns a hardcoded agent-default layer that denies all
// destructive tools by default. Agents must opt-in via their persona
// or the user must configure explicit allows.
func DefaultLayer() Layer {
	return Layer{
		Name: "builtin-defaults",
		Rules: []Rule{
			{ToolID: "file_read", Decision: Allowed, Source: "builtin: read-only tools allowed"},
			{ToolID: "file_list", Decision: Allowed, Source: "builtin: read-only tools allowed"},
			{ToolID: "file_write", Decision: Ask, Source: "builtin: writes require approval"},
			{ToolID: "shell_exec", Decision: Ask, Source: "builtin: shell commands require approval"},
		},
	}
}
