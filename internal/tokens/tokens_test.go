package tokens

import "testing"

func TestEstimateUsesRuneQuarterHeuristic(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"abc":   1,
		"abcd":  1,
		"abcde": 2,
		"ééééé": 2,
	}
	for input, want := range cases {
		if got := Estimate(input); got != want {
			t.Fatalf("Estimate(%q) = %d, want %d", input, got, want)
		}
	}
}
