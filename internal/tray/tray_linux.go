//go:build linux

package tray

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

// lockFile holds the fd open for the process lifetime; released on exit.
var lockFile *os.File

var headlessQuitCh chan struct{}
var headlessOnQuit func()

// AcquireSingleInstance uses a file lock to ensure only one harness instance
// runs at a time. Returns (true, nil) for the first instance, (false, nil)
// if another instance holds the lock.
func AcquireSingleInstance() (bool, error) {
	lockPath := filepath.Join(os.TempDir(), "harness.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, fmt.Errorf("tray: open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return false, nil
	}

	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	lockFile = f
	_ = lockFile // referenced to prevent unused warning; fd held for lifetime
	return true, nil
}

func trayIcon() []byte { return iconPNG }

// OpenBrowser opens the default browser to the given URL.
func OpenBrowser(url string) {
	exec.Command("xdg-open", url).Start() //nolint:errcheck
}

// hasStatusNotifierWatcher probes the session DBus to see whether a
// StatusNotifierWatcher is available (required by fyne-io/systray on Linux).
func hasStatusNotifierWatcher() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	var names []string
	err = conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names)
	if err != nil {
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
		fmt.Println("tray: HARNESS_HEADLESS=1; running in headless mode")
		headless(onQuit)
		return
	}

	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		fmt.Println("tray: no graphical session detected; running in headless mode")
		headless(onQuit)
		return
	}

	if !hasStatusNotifierWatcher() {
		fmt.Println("tray: no StatusNotifierWatcher detected; running in headless mode")
		headless(onQuit)
		return
	}

	systray.Run(func() {
		onReady(uiURL, onQuit)
	}, func() {
		if onQuit != nil {
			onQuit()
		}
	})
}

func headless(onQuit func()) {
	headlessQuitCh = make(chan struct{})
	headlessOnQuit = onQuit
	<-headlessQuitCh
}

// Quit signals the tray to exit. In headless mode it invokes the quit
// callback directly and unblocks Run.
func Quit() {
	if headlessQuitCh != nil {
		if headlessOnQuit != nil {
			headlessOnQuit()
		}
		close(headlessQuitCh)
		return
	}
	systray.Quit()
}
