//go:build windows

package tray

import (
	"errors"
	"fmt"
	"os/exec"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

const mutexName = "Global\\HarnessInstance"

// AcquireSingleInstance tries to acquire the named Windows mutex.
// Returns (true, nil) if this is the first instance.
// Returns (false, nil) if another instance holds the mutex (caller should exit).
// Returns (false, err) on unexpected errors.
func AcquireSingleInstance() (bool, error) {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return false, fmt.Errorf("tray: UTF16PtrFromString: %w", err)
	}

	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return false, nil
		}
		return false, fmt.Errorf("tray: CreateMutex: %w", err)
	}

	// Keep the handle open for the lifetime of the process.
	// We intentionally never close it - Windows will release it when the process exits.
	_ = handle
	return true, nil
}

func trayIcon() []byte { return iconICO }

func OpenBrowser(url string) {
	exec.Command("cmd", "/c", "start", url).Run() //nolint:errcheck
}

// Run starts the system tray on Windows.
func Run(uiURL string, onQuit func()) {
	quit := onceFunc(onQuit)
	systray.Run(func() {
		onReady(uiURL, quit)
	}, quit)
}

// Quit signals the tray to exit.
func Quit() {
	systray.Quit()
}
