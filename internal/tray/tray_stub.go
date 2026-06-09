//go:build !windows && !linux

// Package tray provides stubs for unsupported platforms.
package tray

import (
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var (
	headlessMu       sync.Mutex
	headlessQuitCh   chan struct{}
	headlessOnQuit   func()
	headlessQuitOnce sync.Once
)

// AcquireSingleInstance is a no-op on unsupported platforms.
func AcquireSingleInstance() (bool, error) {
	return true, nil
}

// Run is a no-op stub on unsupported platforms.
func Run(uiURL string, onQuit func()) {
	slog.Info("tray: systray not supported on this platform; running in headless mode")

	headlessMu.Lock()
	headlessQuitCh = make(chan struct{})
	headlessOnQuit = onQuit
	headlessMu.Unlock()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("tray: received signal, shutting down")
		Quit()
	}()

	<-headlessQuitCh
}

// Quit is a no-op stub on unsupported platforms.
func Quit() {
	headlessMu.Lock()
	ch := headlessQuitCh
	fn := headlessOnQuit
	headlessMu.Unlock()

	if ch != nil {
		headlessQuitOnce.Do(func() {
			if fn != nil {
				fn()
			}
			close(ch)
		})
	}
}

// OpenBrowser is a no-op stub on unsupported platforms.
func OpenBrowser(_ string) {}
