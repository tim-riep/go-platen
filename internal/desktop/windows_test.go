//go:build windows

package desktop

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/store"
)

// TestWindowsPrinting installs real printers, prints on one and checks
// that the job reaches the server as PDF. It changes the computer's
// printers and needs administrator rights and port 515, so it runs only
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
		if p.PortName == PortPrefix+"lab" {
			lab = p.Name
		}
	}
	if lab == "" {
		t.Fatalf("printer not installed: %+v", installed)
	}

	// Print a text through the printer as any application would.
	if _, err := runPowerShell(ctx, psPrologue+"'platen test page' | Out-Printer -Name "+psQuote(lab)); err != nil {
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
	t.Logf("job %q, %d bytes", jobs[0].Title, jobs[0].Size)

	if err := app.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	installed, _ = InstalledPrinters(ctx)
	for _, p := range installed {
		if strings.HasPrefix(p.PortName, PortPrefix) {
			t.Fatalf("printer left: %+v", p)
		}
	}
}
