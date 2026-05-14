package tray

import "testing"

func TestIconICO_NotEmpty(t *testing.T) {
	if len(iconICO) == 0 {
		t.Error("iconICO must not be empty")
	}
	// Check ICO signature: reserved=0x0000, type=0x0001.
	if len(iconICO) < 4 {
		t.Fatal("iconICO too short to contain ICO signature")
	}
	sig := []byte{0x00, 0x00, 0x01, 0x00}
	for i, b := range sig {
		if iconICO[i] != b {
			t.Errorf("byte %d: expected 0x%02x, got 0x%02x", i, b, iconICO[i])
		}
	}
}
