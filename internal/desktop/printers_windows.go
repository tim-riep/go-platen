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
	"unsafe"

	"golang.org/x/sys/windows"
)

// Printer management with PowerShell's PrintManagement module. Changes
// need administrator rights: a process without them asks for elevation
// (UAC) once per change set.

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

// ApplyPrinters performs the actions. Without administrator rights it
// runs this program elevated with the "apply-printers" command (UAC
// prompt), which reads the change from a file and calls ApplyFile.
func ApplyPrinters(ctx context.Context, actions []PrinterAction, location string) error {
	if len(actions) == 0 {
		return nil
	}
	change := PrinterChange{Location: location, Actions: actions}
	if err := change.Validate(); err != nil {
		return err
	}
	if windows.GetCurrentProcessToken().IsElevated() {
		_, err := runPowerShell(ctx, ApplyScript(actions, location))
		return err
	}
	f, err := os.CreateTemp("", "platen-printers-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(change); err != nil {
		f.Close()
		return err
	}
	f.Close()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// The elevated process cannot return its output, so failures are
	// detected by the exit code and described by the next listing.
	code, err := runElevated(self, `apply-printers "`+f.Name()+`"`)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("changing the printers failed (exit code %d)", code)
	}
	return nil
}

// ApplyFile performs a change stored by ApplyPrinters; it runs elevated.
func ApplyFile(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var change PrinterChange
	if err := json.Unmarshal(data, &change); err != nil {
		return err
	}
	if err := change.Validate(); err != nil {
		return err
	}
	_, err = runPowerShell(ctx, ApplyScript(change.Actions, change.Location))
	return err
}

// ErrElevationCanceled is returned when the user declines the UAC prompt.
var ErrElevationCanceled = errors.New("the administrator permission was not granted")

type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIcon        uintptr
	hProcess     windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	swHide                = 0
)

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// runElevated starts file with the "runas" verb and waits for it.
func runElevated(file, params string) (uint32, error) {
	verb, _ := windows.UTF16PtrFromString("runas")
	f, _ := windows.UTF16PtrFromString(file)
	p, _ := windows.UTF16PtrFromString(params)
	info := shellExecuteInfo{fMask: seeMaskNoCloseProcess | seeMaskNoAsync, lpVerb: verb, lpFile: f, lpParameters: p, nShow: swHide}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return 0, ErrElevationCanceled
		}
		return 0, fmt.Errorf("start elevated: %w", err)
	}
	defer windows.CloseHandle(info.hProcess)
	if _, err := windows.WaitForSingleObject(info.hProcess, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return 0, err
	}
	return code, nil
}
