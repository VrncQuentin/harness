//go:build linux

package tray

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"fyne.io/systray"
)

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
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return true, nil
}

func Run(uiURL string, onQuit func()) {
	systray.Run(func() {
		onReady(uiURL, onQuit)
	}, func() {
		if onQuit != nil {
			onQuit()
		}
	})
}

func Quit() {
	systray.Quit()
}

func onReady(uiURL string, onQuit func()) {
	systray.SetIcon(iconPNG)
	systray.SetTitle("Harness")
	systray.SetTooltip("Local AI Inference Harness")

	mOpenUI := systray.AddMenuItem("Open UI", "Open the management interface in your browser")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the harness")

	go func() {
		for {
			select {
			case <-mOpenUI.ClickedCh:
				OpenBrowser(uiURL)
			case <-mQuit.ClickedCh:
				if onQuit != nil {
					onQuit()
				}
				systray.Quit()
				return
			}
		}
	}()
}

func OpenBrowser(url string) {
	exec.Command("xdg-open", url).Start() //nolint:errcheck
}
