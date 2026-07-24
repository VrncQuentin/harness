package governor

import (
	"strings"

	"github.com/vrnc/harness/internal/parser"
	"github.com/vrnc/harness/internal/tools"
)

// applyB1 runs the query-aware skeletonizer on read results for
// parser-supported files. It fires only on whole-file reads (path arg set,
// no locator, no start_line).
func (g *Governor) applyB1(toolID string, args map[string]any, res tools.Result, query string) tools.Result {
	if toolID != "read" || res.Error != "" || g.parsers == nil {
		return res
	}
	// Only whole-file reads: locator or start_line present → skip.
	if _, hasLoc := args["locator"]; hasLoc {
		return res
	}
	if _, hasSL := args["start_line"]; hasSL {
		return res
	}
	path, _ := args["path"].(string)
	if path == "" {
		return res
	}
	fe, ok := g.parsers.ForPath(path)
	if !ok {
		return res
	}
	tokens := activeQueryTokens(query)
	if len(tokens) == 0 {
		return res
	}

	symbols, err := fe.Outline([]byte(res.Content))
	if err != nil || len(symbols) == 0 {
		return res
	}

	skeletonized := skeletonize(res.Content, symbols, tokens)
	if skeletonized == res.Content {
		return res
	}
	res.Content = skeletonized
	return res
}

// skeletonize rebuilds content with irrelevant func/method bodies replaced
// by a single { … } stub line. Lines not covered by any symbol span are
// emitted verbatim.
func skeletonize(content string, symbols []parser.Symbol, queryTokens []string) string {
	lines := splitLines(content)
	if len(lines) == 0 {
		return content
	}

	// Build a set of line ranges to suppress (replaced by stub).
	// Key: 1-based start line of body; value: 1-based end line.
	type bodyRange struct{ start, end int }
	var suppress []bodyRange

	for _, sym := range symbols {
		if sym.Kind != "func" && sym.Kind != "method" {
			continue
		}
		if sym.Body.IsZero() {
			continue
		}
		if isRelevant(sym, queryTokens) {
			continue
		}
		// Suppress lines Body.StartLine+1 .. Body.EndLine-1 (the interior).
		// We keep the signature line and closing brace but collapse the interior
		// to keep bracket balance visible to the model.
		if sym.Body.EndLine > sym.Body.StartLine+1 {
			suppress = append(suppress, bodyRange{sym.Body.StartLine + 1, sym.Body.EndLine - 1})
		}
	}

	if len(suppress) == 0 {
		return content
	}

	// Build a suppression map keyed by 1-based line number.
	suppressed := make(map[int]bool)
	for _, br := range suppress {
		for ln := br.start; ln <= br.end; ln++ {
			suppressed[ln] = true
		}
	}
	// Mark the first suppressed line of each range to emit the stub comment.
	stubLines := make(map[int]bool)
	for _, br := range suppress {
		stubLines[br.start] = true
	}

	var b strings.Builder
	for i, line := range lines {
		ln := i + 1 // 1-based
		if stubLines[ln] {
			b.WriteString("\t… (body skeletonized)\n")
			continue
		}
		if suppressed[ln] {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// splitLines returns the lines of s, each with its trailing newline preserved.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines
}

// isRelevant reports whether a symbol's name or receiver contains a word
// that starts with any query token. The name is split on camelCase and
// underscore boundaries before comparison so "IrrelevantFunc" does not
// match the token "relevant".
func isRelevant(sym parser.Symbol, tokens []string) bool {
	words := symbolWords(sym.Name + "_" + sym.Receiver)
	for _, tok := range tokens {
		for _, w := range words {
			if strings.HasPrefix(w, tok) {
				return true
			}
		}
	}
	return false
}

// symbolWords splits a camelCase / snake_case identifier into lowercase
// word segments. "RelevantFunc" → ["relevant", "func"].
func symbolWords(s string) []string {
	s = strings.ToLower(s)
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
}
