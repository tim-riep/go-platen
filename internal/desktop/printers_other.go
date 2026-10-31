//go:build !windows && !linux

package desktop

import (
	"context"
	"errors"
)

// The desktop client manages local printers on Windows and Linux; elsewhere
// these report an error so the rest of the app can run for development.

var errNoPrinters = errors.New("local printers are managed only on Windows and Linux")

func InstalledPrinters(ctx context.Context) ([]LocalPrinter, error) { return nil, errNoPrinters }

func ApplyPrinters(ctx context.Context, actions []PrinterAction, location string) error {
	return errNoPrinters
}
