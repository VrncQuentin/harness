package git

import (
	"sort"
	"strings"
)

// BuildMessage formats tags and summary into the commit message form
// "[k1:v1] [k2:v2] summary". Keys are emitted in sorted order so the
// output is deterministic for any input map. Tag values are written
// as-is; the producer is the disciplined side, so callers must use a
// charset compatible with the tag grammar (see ParseMessage). M3 callers
// only emit agent slugs and the literal "episode", both safe.
//
// Pairs with an empty key or value are silently dropped: emitting them
// would break the round-trip with ParseMessage, and callers building
// messages from untrusted data are expected to validate inputs first.
func BuildMessage(tags map[string]string, summary string) string {
	keys := make([]string, 0, len(tags))
	for k, v := range tags {
		if k == "" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteByte('[')
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(tags[k])
		b.WriteString("] ")
	}
	b.WriteString(summary)
	return b.String()
}

// ParseMessage pulls the leading run of "[k:v]" brackets off the front of
// msg and returns them as tags plus the trimmed remainder as summary.
//
// Parsing rules:
//   - Each bracket starts with '[' at the current scan position; anything
//     else (including stray text or whitespace beyond a single space)
//     ends the tag scan.
//   - Inside a bracket, the key runs from '[' to the first ':' and must
//     be non-empty.
//   - The value runs from ':' to the next ']' and may contain any
//     character including spaces, allowing legitimate values through.
//   - One optional space separator is consumed between adjacent brackets.
//   - Brackets later in the body are not parsed.
//
// On a malformed leading bracket (missing ':' or ']'), parsing aborts at
// the first bracket and the entire original message is returned as
// summary with an empty (non-nil) tags map. The same is true for plain
// messages with no brackets at all.
func ParseMessage(msg string) (map[string]string, string) {
	tags := make(map[string]string)
	rest := msg
	for {
		if !strings.HasPrefix(rest, "[") {
			break
		}
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			// Unclosed bracket: roll back, treat the whole input as
			// summary with no tags.
			return make(map[string]string), strings.TrimSpace(msg)
		}
		body := rest[1:end]
		colon := strings.IndexByte(body, ':')
		if colon <= 0 {
			// Missing colon or empty key: same treatment.
			return make(map[string]string), strings.TrimSpace(msg)
		}
		key := body[:colon]
		value := body[colon+1:]
		tags[key] = value
		rest = rest[end+1:]
		// Eat at most one space between brackets / before the summary.
		if strings.HasPrefix(rest, " ") {
			rest = rest[1:]
		}
	}
	return tags, strings.TrimSpace(rest)
}
