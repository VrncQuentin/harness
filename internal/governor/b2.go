package governor

import (
	"fmt"

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
	if len(res.Content) <= b2Head+b2Tail {
		// Too short for head/tail elision; hard-cap at the tool limit.
		res.Content = res.Content[:limit] + "\n… (truncated)"
		return res
	}
	head := res.Content[:b2Head]
	tail := res.Content[len(res.Content)-b2Tail:]
	dropped := len(res.Content) - b2Head - b2Tail
	res.Content = fmt.Sprintf("%s\n… (%d bytes elided) …\n%s", head, dropped, tail)
	return res
}
