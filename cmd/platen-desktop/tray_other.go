//go:build !windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/tim-riep/go-platen/internal/desktop"
)

// Other systems have no tray icon: the app runs until interrupted, which
// is enough to work on the settings page.
func runTray(ctx context.Context, stop context.CancelFunc, app *desktop.App, ui *desktop.UI) {
	fmt.Println("settings:", ui.URL)
	<-ctx.Done()
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start()
}
