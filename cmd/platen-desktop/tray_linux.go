package main

import (
	_ "embed"
	"fmt"
	"net"
	"os"
	"os/exec"
)

//go:embed platen.png
var trayIcon []byte

// lockTray makes sure each user runs one tray icon, with an abstract
// Unix socket that disappears with the process.
func lockTray() (release func(), ok bool, err error) {
	ln, err := net.Listen("unix", fmt.Sprintf("@platen-tray-%d", os.Getuid()))
	if err != nil {
		return nil, false, nil
	}
	return func() { ln.Close() }, true, nil
}

func openBrowser(url string) error { return exec.Command("xdg-open", url).Start() }

func alert(msg string) {
	if exec.Command("notify-send", "Platen", msg).Run() != nil {
		fmt.Fprintln(os.Stderr, "platen:", msg)
	}
}
