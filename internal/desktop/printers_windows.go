package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Printer management with PowerShell's PrintManagement module.

func powershell() string {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func runPowerShell(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", EncodeCommand(script))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

// InstalledPrinters lists the printers of this computer.
func InstalledPrinters(ctx context.Context) ([]LocalPrinter, error) {
	out, err := runPowerShell(ctx, ListPrintersScript)
	if err != nil {
		return nil, fmt.Errorf("list printers: %w", err)
	}
	var printers []LocalPrinter
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(out), []byte("\xef\xbb\xbf")), &printers); err != nil {
		return nil, fmt.Errorf("list printers: %w", err)
	}
	return printers, nil
}

// ApplyPrinters performs the actions. It needs administrator rights; the
// Windows service runs as LocalSystem.
func ApplyPrinters(ctx context.Context, actions []PrinterAction, location string) error {
	if len(actions) == 0 {
		return nil
	}
	change := PrinterChange{Location: location, Actions: actions}
	if err := change.Validate(); err != nil {
		return err
	}
	_, err := runPowerShell(ctx, ApplyScript(actions, location))
	return err
}
