package main

import (
	_ "embed"

	"golang.org/x/sys/windows"
)

//go:embed platen.ico
var trayIcon []byte

// lockTray makes sure each user runs one tray icon.
func lockTray() (release func(), ok bool, err error) {
	name, _ := windows.UTF16PtrFromString(`Local\PlatenTray`)
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { windows.CloseHandle(h) }, true, nil
}

func alert(msg string) {
	text, _ := windows.UTF16PtrFromString(msg)
	title, _ := windows.UTF16PtrFromString("Platen")
	windows.MessageBox(0, text, title, windows.MB_ICONWARNING)
}

func openBrowser(url string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	u, _ := windows.UTF16PtrFromString(url)
	return windows.ShellExecute(0, verb, u, nil, nil, windows.SW_SHOWNORMAL)
}
