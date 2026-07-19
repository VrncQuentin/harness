// Package tokens contains lightweight token-counting helpers shared by
// presentation and prompt budgeting code.
package tokens

import "unicode/utf8"

// Estimate returns the rune-quarter token estimate for s. It is fast,
// deterministic, and close enough for UI display and prompt budgeting
// against caps that already carry generous headroom.
func Estimate(s string) int {
	return (utf8.RuneCountInString(s) + 3) / 4
}
