package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/tim-riep/go-platen/internal/desktop"
)

//go:embed platen.ico
var icon []byte

// runDefault shows the tray icon of the signed-in user. The work is done
// by the service; the icon shows its state and opens its settings page.
// A second start opens the page and exits.
func runDefault(ctx context.Context, stop context.CancelFunc, openPage bool) error {
	if svcRunning, _ := isService(); svcRunning {
		return runService()
	}
	name, _ := windows.UTF16PtrFromString(`Local\PlatenTray`)
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		openSettings()
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if openPage {
		openSettings()
	}
	runTray(ctx)
	return nil
}

func isService() (bool, error) { return svc.IsWindowsService() }

var httpClient = &http.Client{Timeout: 5 * time.Second}

// serviceState returns the page and the state of the running service.
func serviceState() (string, *desktop.State, bool) {
	url, ok := desktop.RunningUI(uiInfoPath())
	if !ok {
		return "", nil, false
	}
	resp, err := httpClient.Get(url + "api/state")
	if err != nil {
		return "", nil, false
	}
	defer resp.Body.Close()
	var st desktop.State
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return "", nil, false
	}
	return url, &st, true
}

func openSettings() {
	if url, _, ok := serviceState(); ok {
		openBrowser(url)
		return
	}
	text, _ := windows.UTF16PtrFromString("The Platen service is not running. Ask your administrator to reinstall Platen.")
	title, _ := windows.UTF16PtrFromString("Platen")
	windows.MessageBox(0, text, title, windows.MB_ICONWARNING)
}

func runTray(ctx context.Context) {
	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTitle("Platen")
		status := systray.AddMenuItem("Platen", "")
		status.Disable()
		systray.AddSeparator()
		open := systray.AddMenuItem("Open Platen", "Show the settings")
		syncNow := systray.AddMenuItem("Sync printers now", "Update the printers from the server")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Hide icon", "Remove the icon; printing goes on")

		update := func() {
			_, st, ok := serviceState()
			text := "The Platen service is not running"
			if ok {
				text = statusText(st)
			}
			status.SetTitle(text)
			systray.SetTooltip("Platen: " + text)
		}
		update()
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-t.C:
					update()
				case <-open.ClickedCh:
					openSettings()
				case <-syncNow.ClickedCh:
					if url, _, ok := serviceState(); ok {
						req, _ := http.NewRequest(http.MethodPost, url+"api/sync", nil)
						req.Header.Set("X-Platen-Desktop", "1")
						if resp, err := httpClient.Do(req); err == nil {
							resp.Body.Close()
						}
					}
					update()
				case <-quit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
		systray.SetOnTapped(openSettings)
	}, func() {})
}

func statusText(st *desktop.State) string {
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
	return text
}

func openBrowser(url string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	u, _ := windows.UTF16PtrFromString(url)
	return windows.ShellExecute(0, verb, u, nil, nil, windows.SW_SHOWNORMAL)
}
