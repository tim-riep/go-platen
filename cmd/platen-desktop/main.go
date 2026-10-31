// Command platen-desktop is the Windows desktop client of platen. A
// Windows service (LocalSystem) installs the server's printers as local
// printers and forwards what is printed on them; the tray icon of each
// user opens its settings page. See docs/windows.md.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/tim-riep/go-platen/internal/desktop"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: platen-desktop [command]

  (none)                     tray icon and settings page (Windows), the app in
                             the foreground elsewhere (development)
  tray                       tray icon only, as started at sign-in
  service                    run as the Windows service (started by Windows)
  install-service            install or update and start the service (admin)
  uninstall-service          stop and remove the service and its printers (admin)
  set-server <addr> [-lock]  set the print server of the service (admin)
  version                    print the version`

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
	case "", "tray":
		return runDefault(ctx, stop, cmd == "")
	case "service":
		return runService()
	case "install-service":
		return installService()
	case "uninstall-service":
		return uninstallService(ctx)
	case "set-server":
		if len(args) < 2 || len(args) > 3 || (len(args) == 3 && strings.TrimLeft(args[2], "-") != "lock") {
			return fmt.Errorf("%s", usage)
		}
		addr := strings.TrimSpace(args[1])
		if _, err := desktop.ServerURL(addr); err != nil {
			return err
		}
		return setServer(desktop.Config{Address: addr, Locked: len(args) == 3})
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

// startApp runs the app and its settings page in dir until ctx ends.
func startApp(ctx context.Context, dir, uiInfo string, log *slog.Logger) (*desktop.App, *desktop.UI, error) {
	app := &desktop.App{Dir: dir, Version: version, Printers: desktop.SystemPrinters{}, Log: log}
	if err := app.Start(ctx); err != nil {
		return nil, nil, err
	}
	ui, err := desktop.StartUI(ctx, app, uiInfo)
	if err != nil {
		return nil, nil, err
	}
	log.Info("platen desktop started", "version", version)
	return app, ui, nil
}

// newLogger writes to platen.log in dir, started afresh when it grew
// beyond 5 MB.
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
