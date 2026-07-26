package governor

import (
	"fmt"
	"unicode/utf8"

	"github.com/VrncQuentin/harness/internal/tools"
)

// b2Limits maps tool IDs to the maximum Content length (bytes) before B2
// elides the middle. Only high-volume toolchain tools are listed; unlisted
// tools pass through unchanged.
var b2Limits = map[string]int{
	"exec":     8 * 1024,
	"go_test":  8 * 1024,
	"go_lint":  4 * 1024,
	"git_diff": 16 * 1024,
	"git_log":  4 * 1024,
}

// b2Head and b2Tail are the bytes kept at each end of elided content.
//
// Every entry in b2Limits must exceed b2Head+b2Tail, so content that passes the
// limit check always has room for head/tail elision. TestB2LimitsExceedHeadTail
// enforces it; a smaller limit would make elision produce more bytes than it
// removed.
const (
	b2Head = 512
	b2Tail = 512
)

// applyB2 caps large Content values for high-volume toolchain tools using
// head/tail elision. Content at or under the per-tool limit passes through
// unchanged. Tools without a registered limit are skipped entirely.
func (g *Governor) applyB2(toolID string, res tools.Result) tools.Result {
	limit, ok := b2Limits[toolID]
	if !ok || res.Content == "" || len(res.Content) <= limit {
		return res
	}
	// Both cuts below land on a rune boundary. B2 runs after the tools have
	// already bounded their output safely, and slicing at a fixed byte offset
	// here would undo that: a multi-byte character split across the head or
	// tail boundary puts invalid UTF-8 into the conversation and from there
	// into the session record.
	//
	// There is no separate short-content branch. Reaching this point means
	// len(Content) > limit, and every limit exceeds b2Head+b2Tail, so there is
	// always room for head/tail elision.
	head := res.Content[:runeSafeCutEnd(res.Content, b2Head)]
	tail := res.Content[runeSafeCutStart(res.Content, len(res.Content)-b2Tail):]
	dropped := len(res.Content) - len(head) - len(tail)
	res.Content = fmt.Sprintf("%s\n… (%d bytes elided) …\n%s", head, dropped, tail)
	return res
}

// runeSafeCutEnd returns the largest offset at or below limit that starts a
// rune, so s[:runeSafeCutEnd(s, limit)] is always valid UTF-8.
func runeSafeCutEnd(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return cut
}

// runeSafeCutStart returns the smallest offset at or above from that starts a
// rune, so s[runeSafeCutStart(s, from):] is always valid UTF-8.
func runeSafeCutStart(s string, from int) int {
	if from <= 0 {
		return 0
	}
	cut := from
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return cut
}
