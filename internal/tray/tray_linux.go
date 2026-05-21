//go:build linux

package tray

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

var lockFile *os.File // held for process lifetime; released on exit

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
	return true, nil
}

func trayIcon() []byte { return iconPNG }

// OpenBrowser opens the default browser to the given URL.
func OpenBrowser(url string) {
	exec.Command("xdg-open", url).Start() //nolint:errcheck
}
