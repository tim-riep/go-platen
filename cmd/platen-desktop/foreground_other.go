//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/tim-riep/go-platen/internal/desktop"
)

// Outside Windows the app runs in the foreground for development; it
// cannot manage printers there.

var errWindowsOnly = errors.New("only available on Windows")

func runDefault(ctx context.Context, stop context.CancelFunc, openPage bool) error {
	d, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(d, "Platen")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	log, closeLog := newLogger(dir)
	defer closeLog()
	_, ui, err := startApp(ctx, dir, filepath.Join(dir, "ui.json"), log)
	if err != nil {
		return err
	}
	fmt.Println("settings:", ui.URL)
	if openPage {
		name := "xdg-open"
		if runtime.GOOS == "darwin" {
			name = "open"
		}
		exec.Command(name, ui.URL).Start()
	}
	<-ctx.Done()
	return nil
}

func runService() error                          { return errWindowsOnly }
func installService() error                      { return errWindowsOnly }
func uninstallService(ctx context.Context) error { return errWindowsOnly }
func setServer(cfg desktop.Config) error         { return errWindowsOnly }
