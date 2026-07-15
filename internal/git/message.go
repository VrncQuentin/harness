package git

import (
	"sort"
	"strings"
)

// BuildMessage formats tags and summary into the commit message form
// "[k1:v1] [k2:v2] summary". Keys are emitted in sorted order so the
// output is deterministic for any input map. Tag values are written
// as-is; the producer is the disciplined side, so callers should keep
// values short and machine-readable.
//
// Pairs with an empty key or value are silently dropped so malformed tag
// data does not produce empty structured prefixes.
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
