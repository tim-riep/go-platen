package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Printer management with the CUPS command line tools. Members of the
// CUPS system group (lpadmin on Debian and Ubuntu) need no password; the
// service user is one.

// PPDPath is the app's PPD, installed by the package.
func PPDPath() string {
	if p := os.Getenv("PLATEN_PPD"); p != "" {
		return p
	}
	return "/usr/share/platen-desktop/platen.ppd"
}

func runCUPS(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Parsed output must not be translated.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// InstalledPrinters lists the CUPS queues the app manages.
func InstalledPrinters(ctx context.Context) ([]LocalPrinter, error) {
	out, err := runCUPS(ctx, "lpstat", "-v")
	if err != nil && !strings.Contains(out, "No destinations") {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, fmt.Errorf("list printers: %w", err)
		}
	}
	var printers []LocalPrinter
	for queue, uri := range parseCUPSDevices(out) {
		if !strings.HasPrefix(queue, cupsQueuePrefix) {
			continue
		}
		opts, _ := runCUPS(ctx, "lpoptions", "-p", queue)
		if p, ok := cupsPrinter(queue, uri, parseCUPSOptions(opts)["printer-info"]); ok {
			printers = append(printers, p)
		}
	}
	return printers, nil
}

// ApplyPrinters performs the actions with lpadmin.
func ApplyPrinters(ctx context.Context, actions []PrinterAction, location string) error {
	if len(actions) == 0 {
		return nil
	}
	if err := (PrinterChange{Location: location, Actions: actions}).Validate(); err != nil {
		return err
	}
	var failed []string
	for _, a := range actions {
		if out, err := runCUPS(ctx, "lpadmin", lpadminArgs(a, location, PPDPath())...); err != nil {
			failed = append(failed, fmt.Sprintf("%s %s: %s", a.Kind, a.Name, strings.TrimSpace(out)))
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}
