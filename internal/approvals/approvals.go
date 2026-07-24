// Package approvals provides the permission engine: layered rules,
// destructive-command classification, and once/always/reject decisions
// evaluated within the agent loop at tool-dispatch time.
package approvals

import (
	"strings"
	"sync"

	"github.com/vrnc/harness/internal/tools"
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

const (
	// ApprovalScopeOnce records a one-time approval decision.
	ApprovalScopeOnce = "once"
	// ApprovalScopeAlways records a remembered session approval decision.
	ApprovalScopeAlways = "always"
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

// ApprovalResponse carries a decision and whether the user chose
// to remember it (scope: once vs. always).
type ApprovalResponse struct {
	Decision Decision
	// Remember is true when the user selected "always" and the
	// resulting session rule should persist for future matching calls.
	Remember bool
}

// Rule is a single permission entry. It matches on tool id and optional
// command arguments (for exec). The decision applies when the rule
// is the best match within its layer.
type Rule struct {
	// ToolID is the tool this rule applies to (e.g. "edit",
	// "exec", or "*" for any tool).
	ToolID string
	// CommandPattern is an optional prefix or wildcard pattern matched
	// against exec command strings. Empty means "any command".
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

// Evaluate checks whether toolID with commandArg (for exec) is
// permitted. Returns the decision and the matching Rule source for
// audit trails.
//
// exec commands are never auto-allowed. The command classifier is
// intentionally conservative documentation until platform-aware parsing is
// reliable enough to distinguish safe shell forms from dangerous equivalents.
// Matching deny rules still deny; every other shell command asks.
func (e *Evaluator) Evaluate(toolID, commandArg string) (Decision, string) {
	e.mu.Lock()
	effectiveLayers := append([]Layer{}, e.layers...)
	if len(e.session.Rules) > 0 {
		effectiveLayers = append(effectiveLayers, *e.session)
	}
	e.mu.Unlock()

	// Evaluate layers in order (agent defaults → user config → session).
	// Last layer with a matching rule wins.
	best := Ask
	source := "default: no matching permission rule"
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

	if toolID == "exec" {
		if best == Denied {
			return Denied, source
		}
		return Ask, "requires approval: shell command"
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
	if toolID != "exec" {
		return r.ToolID == "exec"
	}
	return strings.HasPrefix(commandArg, r.CommandPattern) || commandArg == r.CommandPattern
}

// DefaultLayer returns a hardcoded agent-default layer that denies all
// destructive tools by default. Agents must opt-in via their persona
// or the user must configure explicit allows.
func DefaultLayer() Layer {
	descriptors := tools.BuiltinDescriptors()
	rules := make([]Rule, 0, len(descriptors))
	for _, desc := range descriptors {
		rules = append(rules, Rule{
			ToolID:   desc.ID,
			Decision: defaultDecision(desc.DefaultApproval),
			Source:   desc.DefaultApprovalSource,
		})
	}
	return Layer{Name: "builtin-defaults", Rules: rules}
}

func defaultDecision(defaultApproval tools.ApprovalDefault) Decision {
	switch defaultApproval {
	case tools.ApprovalDefaultAllow:
		return Allowed
	case tools.ApprovalDefaultAsk:
		return Ask
	default:
		return Denied
	}
}
