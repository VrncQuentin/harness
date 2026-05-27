//go:build windows || linux

package tray

import "fyne.io/systray"

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
	systray.SetIcon(trayIcon())
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
