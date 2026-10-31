// Command platen-desktop is the Windows desktop client of platen: it
// installs the server's printers as local printers and forwards what is
// printed on them. See docs/windows.md.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/tim-riep/go-platen/internal/desktop"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "platen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "":
	case "apply-printers":
		// Elevated step of a printer change, started by the app itself.
		if len(args) != 2 {
			return fmt.Errorf("usage: platen-desktop apply-printers <file>")
		}
		return desktop.ApplyFile(ctx, args[1])
	case "remove-printers":
		// Used by the uninstaller, which runs as administrator.
		installed, err := desktop.InstalledPrinters(ctx)
		if err != nil {
			return err
		}
		return desktop.ApplyPrinters(ctx, desktop.PlanPrinters(installed, nil), "")
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	dir, err := dataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A second start opens the page of the running app.
	if url, ok := desktop.RunningUI(dir); ok {
		return openBrowser(url)
	}
	log, closeLog := newLogger(dir)
	defer closeLog()

	app := &desktop.App{Dir: dir, Version: version, Printers: desktop.SystemPrinters{}, Login: loginItem{}, Log: log}
	if err := app.Start(ctx); err != nil {
		return err
	}
	ui, err := desktop.StartUI(ctx, app)
	if err != nil {
		return err
	}
	log.Info("platen desktop started", "version", version, "ui", "127.0.0.1")
	if app.Address() == "" {
		openBrowser(ui.URL)
	}
	runTray(ctx, stop, app, ui)
	return nil
}

func dataDir() (string, error) {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "Platen"), nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "Platen"), nil
}

// newLogger writes to platen.log in the data directory, started afresh
// when it grew beyond 5 MB.
func newLogger(dir string) (*slog.Logger, func()) {
	path := filepath.Join(dir, "platen.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var w io.Writer = os.Stderr
	if err == nil {
		w = f
	}
	log := slog.New(slog.NewTextHandler(w, nil))
	slog.SetDefault(log)
	return log, func() {
		if f != nil {
			f.Close()
		}
	}
}
