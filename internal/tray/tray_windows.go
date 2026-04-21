//go:build windows

package tray

import (
	"fmt"
	"os/exec"
	"unsafe"

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
		if err == windows.ERROR_ALREADY_EXISTS {
			return false, nil
		}
		return false, fmt.Errorf("tray: CreateMutex: %w", err)
	}

	// Keep the handle open for the lifetime of the process.
	// We intentionally never close it — Windows will release it when the process exits.
	_ = handle
	return true, nil
}

// Run starts the system tray. onReady is called after the tray is set up.
// onQuit is called when the user selects Quit. This function blocks until quit.
func Run(uiURL string, onQuit func()) {
	systray.Run(func() {
		onReady(uiURL, onQuit)
	}, func() {
		if onQuit != nil {
			onQuit()
		}
	})
}

// onReady configures the tray icon and menu.
func onReady(uiURL string, onQuit func()) {
	systray.SetIcon(iconICO)
	systray.SetTitle("Harness")
	systray.SetTooltip("Local AI Inference Harness")

	mOpenUI := systray.AddMenuItem("Open UI", "Open the management interface in your browser")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the harness")

	go func() {
		for {
			select {
			case <-mOpenUI.ClickedCh:
				openBrowser(uiURL)
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

// openBrowser opens the default browser to the given URL.
func openBrowser(url string) {
	cmd := exec.Command("cmd", "/c", "start", url)
	cmd.Run() //nolint:errcheck
}

// Keep the handle variable alive to prevent GC (Windows-specific handle).
var _ unsafe.Pointer
