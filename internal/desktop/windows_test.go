//go:build windows

package desktop

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/store"
)

// largeDocument is about 50 pages of text, so that the PDF is larger than
// the buffers between spooler and receiver and the spooler has to wait for
// the receiver.
const minLargeDocument = 64 << 10

const largeDocument = `(1..3000 | ForEach-Object { "platen test line $_ " + ('x' * 60) })`

// TestWindowsPrinting installs real printers, prints on one and checks
// that the job reaches the server as PDF. It changes the computer's
// printers and needs administrator rights, so it runs only
// with PLATEN_WINDOWS_E2E=1 (CI).
func TestWindowsPrinting(t *testing.T) {
	if os.Getenv("PLATEN_WINDOWS_E2E") != "1" {
		t.Skip("set PLATEN_WINDOWS_E2E=1 to change this computer's printers")
	}
	st, addr := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	now := time.Now()
	st.CreatePanelUser(ctx, &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now})

	app := &App{Dir: t.TempDir(), Version: "test", Printers: SystemPrinters{}, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if s := app.State(); s.ReceiverError != "" {
		t.Fatal(s.ReceiverError)
	}
	if err := app.Connect(ctx, addr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Disconnect(context.Background()); err != nil {
			t.Error("disconnect:", err)
		}
	})
	installed, err := InstalledPrinters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lab string
	for _, p := range installed {
		if id, _, ok := p.Managed(); ok && id == "lab" {
			lab = p.Name
		}
	}
	if lab == "" {
		t.Fatalf("printer not installed: %+v", installed)
	}

	// Print a text through the printer as any application would.
	if _, err := runPowerShell(ctx, psPrologue+largeDocument+" | Out-Printer -Name "+psQuote(lab)); err != nil {
		t.Fatal("print:", err)
	}
	var jobs []store.Job
	for ctx.Err() == nil {
		jobs, _ = st.ListJobs(ctx, store.JobFilter{})
		if len(jobs) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(jobs) != 1 || jobs[0].QueueID != "lab" || jobs[0].ContentType != "application/pdf" {
		t.Fatalf("jobs %+v, spool %+v", jobs, app.State().Jobs)
	}
	// Out-Printer names its document; the title comes from the spooler.
	if jobs[0].Title == "" || jobs[0].Title == "Untitled" {
		t.Errorf("no job title: %+v", jobs[0])
	}
	t.Logf("job %q, %d bytes", jobs[0].Title, jobs[0].Size)
	if jobs[0].Size <= minLargeDocument {
		t.Errorf("the document (%d bytes) is small, so the test does not cover a spooler that waits for the receiver", jobs[0].Size)
	}

	if err := app.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	installed, _ = InstalledPrinters(ctx)
	for _, p := range installed {
		if _, ok := p.PrinterID(); ok {
			t.Fatalf("printer left: %+v", p)
		}
	}
}

// TestWindowsService installs the real service with a preset server, waits
// until it installed the printers, prints and uninstalls it again.
func TestWindowsService(t *testing.T) {
	if os.Getenv("PLATEN_WINDOWS_E2E") != "1" {
		t.Skip("set PLATEN_WINDOWS_E2E=1 to install the service")
	}
	st, addr := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	now := time.Now()
	st.CreatePanelUser(ctx, &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now})

	exe := filepath.Join(t.TempDir(), "Platen.exe")
	if out, err := exec.Command("go", "build", "-o", exe, "../../cmd/platen-desktop").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	platen := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(exe, args...).CombinedOutput(); err != nil {
			t.Fatalf("platen %v: %v\n%s", args, err, out)
		}
	}
	data := filepath.Join(os.Getenv("ProgramData"), "Platen")
	t.Cleanup(func() {
		exec.Command(exe, "uninstall-service").Run()
		if log, err := os.ReadFile(filepath.Join(data, "platen.log")); err == nil {
			t.Logf("service log:\n%s", log)
		}
		os.RemoveAll(data)
	})
	platen("set-server", addr, "-lock")
	platen("install-service")

	var lab string
	for ctx.Err() == nil && lab == "" {
		installed, _ := InstalledPrinters(ctx)
		for _, p := range installed {
			if id, _, ok := p.Managed(); ok && id == "lab" {
				lab = p.Name
			}
		}
		time.Sleep(time.Second)
	}
	if lab == "" {
		t.Fatal("the service did not install the printers")
	}

	// The settings page of the service is reachable through the info file.
	url, ok := RunningUI(filepath.Join(data, "public", "ui.json"))
	if !ok {
		t.Fatal("settings page of the service not reachable")
	}
	resp, err := http.Get(url + "api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state State
	json.NewDecoder(resp.Body).Decode(&state)
	resp.Body.Close()
	if !state.Locked || state.Address != addr || state.Me == nil {
		t.Fatalf("service state %+v", state)
	}

	if _, err := runPowerShell(ctx, psPrologue+largeDocument+" | Out-Printer -Name "+psQuote(lab)); err != nil {
		t.Fatal("print:", err)
	}
	var jobs []store.Job
	for ctx.Err() == nil {
		jobs, _ = st.ListJobs(ctx, store.JobFilter{})
		if len(jobs) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(jobs) != 1 || jobs[0].QueueID != "lab" || jobs[0].ContentType != "application/pdf" || jobs[0].AppClientID != "device:127.0.0.1" {
		t.Fatalf("jobs %+v", jobs)
	}
	t.Logf("job %q, %d bytes", jobs[0].Title, jobs[0].Size)
	if jobs[0].Size <= minLargeDocument {
		t.Errorf("the document (%d bytes) is small, so the test does not cover a spooler that waits for the receiver", jobs[0].Size)
	}

	platen("uninstall-service")
	installed, _ := InstalledPrinters(ctx)
	for _, p := range installed {
		if _, ok := p.PrinterID(); ok {
			t.Fatalf("printer left: %+v", p)
		}
	}
}
