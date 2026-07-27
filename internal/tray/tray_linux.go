//go:build linux

package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

// lockFile holds the fd open for the process lifetime; released on exit.
var lockFile *os.File

var (
	headlessMu       sync.Mutex
	headlessQuitCh   chan struct{}
	headlessOnQuit   func()
	headlessQuitOnce sync.Once
)

// AcquireSingleInstance uses a file lock to ensure only one harness instance
// runs at a time. Returns (true, nil) for the first instance, (false, nil)
// if another instance holds the lock.
func AcquireSingleInstance() (bool, error) {
	// The lock lives on the descriptor, not on the name: flock is taken on the
	// fd this call opens, so whatever the name resolved to is the thing being
	// locked, and a second instance racing on the same name contends on the
	// same object or fails to take the lock. There is no configured tree to
	// contain it and no second resolution to defend against. See the filesystem
	// access ledger in docs/architecture.md.
	lockPath := filepath.Join(os.TempDir(), "harness.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, fmt.Errorf("tray: open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if isLockHeldError(err) {
			return false, nil
		}
		return false, fmt.Errorf("tray: lock file: %w", err)
	}

	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	lockFile = f
	_ = lockFile // referenced to prevent unused warning; fd held for lifetime
	return true, nil
}

func isLockHeldError(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
func trayIcon() []byte { return iconPNG }

// OpenBrowser opens the default browser to the given URL.
func OpenBrowser(url string) {
	exec.Command("xdg-open", url).Start() //nolint:errcheck
}

const dbusProbeTimeout = 2 * time.Second

// hasStatusNotifierWatcher probes the session DBus to see whether a
// StatusNotifierWatcher is available (required by fyne-io/systray on Linux).
func hasStatusNotifierWatcher() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		slog.Warn("tray: DBus session bus unavailable", "err", err)
		return false
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), dbusProbeTimeout)
	defer cancel()

	var names []string
	call := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.ListNames", 0)
	if call.Err != nil {
		slog.Warn("tray: DBus ListNames call failed", "err", call.Err)
		return false
	}
	if err := call.Store(&names); err != nil {
		slog.Warn("tray: DBus ListNames store failed", "err", err)
		return false
	}
	for _, name := range names {
		if name == "org.kde.StatusNotifierWatcher" {
			return true
		}
	}
	return false
}

// Run starts the system tray on Linux, or falls back to headless mode
// when no StatusNotifierWatcher is available (e.g. i3, headless servers).
func Run(uiURL string, onQuit func()) {
	if os.Getenv("HARNESS_HEADLESS") == "1" {
		slog.Info("tray: HARNESS_HEADLESS=1; running in headless mode")
		headless(onQuit)
		return
	}

	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		slog.Info("tray: no graphical session detected; running in headless mode")
		headless(onQuit)
		return
	}

	if !hasStatusNotifierWatcher() {
		slog.Info("tray: no StatusNotifierWatcher detected; running in headless mode")
		headless(onQuit)
		return
	}

	quit := onceFunc(onQuit)
	systray.Run(func() {
		onReady(uiURL, quit)
	}, quit)
}

func headless(onQuit func()) {
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

// Quit signals the tray to exit. In headless mode it invokes the quit
// callback directly and unblocks Run.
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
		return
	}
	systray.Quit()
}
