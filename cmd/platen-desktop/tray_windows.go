package main

import (
	"context"
	_ "embed"
	"fmt"

	"fyne.io/systray"
	"golang.org/x/sys/windows"

	"github.com/tim-riep/go-platen/internal/desktop"
)

//go:embed platen.ico
var icon []byte

// runTray shows the notification area icon until Quit or ctx ends.
func runTray(ctx context.Context, stop context.CancelFunc, app *desktop.App, ui *desktop.UI) {
	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTitle("Platen")
		status := systray.AddMenuItem("Platen", "")
		status.Disable()
		systray.AddSeparator()
		open := systray.AddMenuItem("Open Platen", "Show the settings")
		syncNow := systray.AddMenuItem("Sync printers now", "Update the printers from the server")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop Platen; printing waits until it runs again")

		update := func() {
			st := app.State()
			text := "No print server set up"
			switch {
			case st.Error != "" || st.ReceiverError != "":
				text = "Problem: open Platen for details"
			case st.Address != "" && st.Me != nil:
				text = "Connected to " + st.Address
			case st.Address != "":
				text = "Not connected"
			}
			if st.Waiting > 0 {
				text += fmt.Sprintf(" · %d job(s) waiting", st.Waiting)
			}
			status.SetTitle(text)
			systray.SetTooltip("Platen: " + text)
		}
		app.OnChange = update
		update()
		go func() {
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-open.ClickedCh:
					openBrowser(ui.URL)
				case <-syncNow.ClickedCh:
					go app.Sync(ctx)
				case <-quit.ClickedCh:
					stop()
				}
			}
		}()
		systray.SetOnTapped(func() { openBrowser(ui.URL) })
	}, func() {})
}

func openBrowser(url string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	u, _ := windows.UTF16PtrFromString(url)
	return windows.ShellExecute(0, verb, u, nil, nil, windows.SW_SHOWNORMAL)
}
