//go:build !windows

// Package tray provides stubs for non-Windows builds.
package tray

import "fmt"

// AcquireSingleInstance is a no-op on non-Windows platforms.
// Always returns (true, nil) - single-instance enforcement only runs on Windows.
func AcquireSingleInstance() (bool, error) {
	return true, nil
}

// Run is a no-op stub on non-Windows platforms.
func Run(uiURL string, onQuit func()) {
	fmt.Println("tray: systray not supported on this platform; running in headless mode")
	// Block forever (or until the caller's context is done).
	select {}
}

// Quit is a no-op stub on non-Windows platforms.
func Quit() {}
