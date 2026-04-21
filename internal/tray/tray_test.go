package tray

import "testing"

func TestIconPNG_NotEmpty(t *testing.T) {
	if len(iconPNG) == 0 {
		t.Error("iconPNG must not be empty")
	}
	// Check PNG signature.
	if len(iconPNG) < 8 {
		t.Fatal("iconPNG too short to contain PNG signature")
	}
	sig := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	for i, b := range sig {
		if iconPNG[i] != b {
			t.Errorf("byte %d: expected 0x%02x, got 0x%02x", i, b, iconPNG[i])
		}
	}
}
