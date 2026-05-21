//go:build windows || linux

package tray

import (
	"runtime"
	"testing"
)

func TestTrayIcon_NotEmpty(t *testing.T) {
	icon := trayIcon()
	if len(icon) == 0 {
		t.Fatal("trayIcon() must not be empty")
	}

	switch runtime.GOOS {
	case "windows":
		if len(icon) < 4 {
			t.Fatal("iconICO too short to contain ICO signature")
		}
		sig := []byte{0x00, 0x00, 0x01, 0x00}
		for i, b := range sig {
			if icon[i] != b {
				t.Errorf("byte %d: expected 0x%02x, got 0x%02x", i, b, icon[i])
			}
		}
	case "linux":
		if len(icon) < 4 {
			t.Fatal("iconPNG too short to contain PNG signature")
		}
		sig := []byte{0x89, 0x50, 0x4E, 0x47}
		for i, b := range sig {
			if icon[i] != b {
				t.Errorf("byte %d: expected 0x%02x, got 0x%02x", i, b, icon[i])
			}
		}
	}
}
