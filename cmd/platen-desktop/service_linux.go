package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/tim-riep/go-platen/internal/desktop"
)

// On Linux the service runs under systemd (platen-desktop.service from the
// .deb) as the user platen, a member of the CUPS system group lpadmin, so
// it can change printers without root. Users need no rights.

// serviceDir holds configuration, spool and log; only the service user
// may enter it (systemd StateDirectory, mode 0700).
func serviceDir() string {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return d
	}
	return "/var/lib/platen-desktop"
}

// uiInfoPath tells the users' tray icons where the settings page is; it
// is readable for everyone (systemd RuntimeDirectory, mode 0755).
func uiInfoPath() string {
	dir := os.Getenv("RUNTIME_DIRECTORY")
	if dir == "" {
		dir = "/run/platen-desktop"
	}
	return filepath.Join(dir, "ui.json")
}

func runService() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(serviceDir(), 0o700); err != nil {
		return err
	}
	log, closeLog := newLogger(serviceDir())
	defer closeLog()
	if _, _, err := startApp(ctx, serviceDir(), uiInfoPath(), log); err != nil {
		return err
	}
	<-ctx.Done()
	log.Info("service stopping")
	return nil
}

var errPackage = errors.New("on Linux the service is installed by the platen-desktop package; use systemctl")

func installService() error                      { return errPackage }
func uninstallService(ctx context.Context) error { return errPackage }

// setServer stores the server for the service and restarts it. It runs as
// root; the file is handed to the owner of the data directory.
func setServer(cfg desktop.Config) error {
	dir := serviceDir()
	if err := desktop.WriteConfig(dir, cfg); err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			os.Chown(desktop.ConfigPath(dir), int(st.Uid), int(st.Gid))
		}
	}
	exec.Command("systemctl", "try-restart", "platen-desktop.service").Run()
	return nil
}

// removePrinters removes all printers of the app (package removal).
func removePrinters(ctx context.Context) error {
	installed, err := desktop.InstalledPrinters(ctx)
	if err != nil {
		return err
	}
	return desktop.ApplyPrinters(ctx, desktop.PlanPrinters(installed, nil, nil), "")
}
