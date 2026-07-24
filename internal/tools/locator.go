package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// spanHashLen is the number of hex characters kept from the sha256 digest.
// 64 bits of prefix is plenty for anchor comparison and keeps locator lines
// short in model context.
const spanHashLen = 16

// FormatLocator renders the stable locator for a line span:
// "<path>:<start>-<end>", 1-based inclusive.
func FormatLocator(path string, start, end int) string {
	return fmt.Sprintf("%s:%d-%d", path, start, end)
}

// ParseLocator splits "path:start-end" into its parts. The path may itself
// contain colons (Windows drive letters), so the range is taken from the
// last colon.
func ParseLocator(s string) (path string, start, end int, err error) {
	errInvalid := fmt.Errorf("invalid locator %q — want path:start-end", s)
	i := strings.LastIndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", 0, 0, errInvalid
	}
	rawStart, rawEnd, ok := strings.Cut(s[i+1:], "-")
	if !ok {
		return "", 0, 0, errInvalid
	}
	start, convErr := strconv.Atoi(rawStart)
	if convErr != nil {
		return "", 0, 0, errInvalid
	}
	end, convErr = strconv.Atoi(rawEnd)
	if convErr != nil {
		return "", 0, 0, errInvalid
	}
	if start < 1 || end < start {
		return "", 0, 0, fmt.Errorf("invalid locator range %d-%d", start, end)
	}
	return s[:i], start, end, nil
}

// SpanHash hashes the exact bytes of lines start..end (1-based, inclusive,
// line terminators included) and returns "h:" plus the first 16 hex chars of
// the sha256 digest. It is the anchor format ast_find emits and edit verifies.
func SpanHash(src []byte, start, end int) (string, error) {
	span, err := spanBytes(src, start, end)
	if err != nil {
		return "", err
	}
	return hashBytes(span), nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "h:" + hex.EncodeToString(sum[:])[:spanHashLen]
}

// spanBytes returns the exact bytes of lines start..end, terminators
// included. The final line may lack a terminator.
func spanBytes(src []byte, start, end int) ([]byte, error) {
	if start < 1 || end < start {
		return nil, fmt.Errorf("tools: invalid line span %d-%d", start, end)
	}
	lines := splitLinesKeepEnds(src)
	if end > len(lines) {
		return nil, fmt.Errorf("tools: span %d-%d exceeds file length %d", start, end, len(lines))
	}
	var out []byte
	for _, line := range lines[start-1 : end] {
		out = append(out, line...)
	}
	return out, nil
}

// splitLinesKeepEnds splits src into lines, each keeping its terminator.
// A trailing newline does not produce an empty final line.
func splitLinesKeepEnds(src []byte) [][]byte {
	var lines [][]byte
	begin := 0
	for i, b := range src {
		if b == '\n' {
			lines = append(lines, src[begin:i+1])
			begin = i + 1
		}
	}
	if begin < len(src) {
		lines = append(lines, src[begin:])
	}
	return lines
}
