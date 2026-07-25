package governor

import (
	"github.com/VrncQuentin/harness/internal/tokens"
	"github.com/VrncQuentin/harness/internal/tools"
)

// wrapB5 applies transform to pre and returns the result only when the
// estimated token count did not increase. If the transform increases tokens,
// the pre-transform result is returned unchanged (auto-revert).
//
// Both sides use the same counter (tokens.Estimate or g.tokenizer when set),
// so the guard is correct regardless of the underlying heuristic.
func (g *Governor) wrapB5(pre tools.Result, transform func(tools.Result) tools.Result) tools.Result {
	post := transform(pre)
	if g.resultTokens(post) > g.resultTokens(pre) {
		return pre
	}
	return post
}

// resultTokens returns the estimated token count for a result, combining
// Content and Error so both output paths are accounted for.
func (g *Governor) resultTokens(r tools.Result) int {
	combined := r.Content + r.Error
	if g.tokenizer != nil {
		return g.tokenizer(combined)
	}
	return tokens.Estimate(combined)
}
