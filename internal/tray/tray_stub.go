//go:build !windows && !linux

// Package tray provides stubs for unsupported platforms.
package tray

import "fmt"

// AcquireSingleInstance is a no-op on unsupported platforms.
func AcquireSingleInstance() (bool, error) {
	return true, nil
}

// Run is a no-op stub on unsupported platforms.
func Run(uiURL string, onQuit func()) {
	fmt.Println("tray: systray not supported on this platform; running in headless mode")
	select {}
}

// Quit is a no-op stub on unsupported platforms.
func Quit() {}

// OpenBrowser is a no-op stub on unsupported platforms.
func OpenBrowser(_ string) {}
