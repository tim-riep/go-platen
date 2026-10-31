//go:build linux

package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/store"
)

// writeLargeText writes about 50 pages of text, so that the PDF is larger
// than the socket buffers between CUPS and the receiver.
func writeLargeText(t *testing.T) string {
	var b strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&b, "platen test line %d %s\n", i, strings.Repeat("x", 60))
	}
	p := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitForJob(ctx context.Context, t *testing.T, st store.Store) store.Job {
	t.Helper()
	for ctx.Err() == nil {
		if jobs, _ := st.ListJobs(ctx, store.JobFilter{}); len(jobs) > 0 {
			return jobs[0]
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no job reached the server")
	return store.Job{}
}

func lp(t *testing.T, queue, title, file string) {
	t.Helper()
	if out, err := exec.Command("lp", "-d", queue, "-t", title, file).CombinedOutput(); err != nil {
		t.Fatalf("lp: %v\n%s", err, out)
	}
}

// TestLinuxPrinting adds real CUPS queues, prints on one and checks that
// the job reaches the server as PDF with its title. It changes the
// computer's printers and needs CUPS admin rights, so it runs only with
// PLATEN_LINUX_E2E=1 (CI, as root).
func TestLinuxPrinting(t *testing.T) {
	if os.Getenv("PLATEN_LINUX_E2E") != "1" {
		t.Skip("set PLATEN_LINUX_E2E=1 to change this computer's printers")
	}
	ppd, _ := filepath.Abs("../../desktop/macos/Resources/platen.ppd")
	t.Setenv("PLATEN_PPD", ppd)
	st, addr := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	now := time.Now()
	st.CreatePanelUser(ctx, &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now})

	app := &App{Dir: t.TempDir(), Version: "test", Printers: SystemPrinters{}, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Connect(ctx, addr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Disconnect(context.Background()) })
	installed, err := InstalledPrinters(ctx)
	if err != nil || len(installed) != 2 {
		t.Fatalf("queues %+v %v", installed, err)
	}

	lp(t, CUPSQueue("lab"), "Linux test page", writeLargeText(t))
	job := waitForJob(ctx, t, st)
	if job.QueueID != "lab" || job.ContentType != "application/pdf" || job.Title != "Linux test page" || job.Size <= 64<<10 {
		t.Fatalf("job %+v", job)
	}
	t.Logf("job %q, %d bytes", job.Title, job.Size)

	if err := app.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if installed, _ := InstalledPrinters(ctx); len(installed) != 0 {
		t.Fatalf("queues left: %+v", installed)
	}
}

// TestLinuxPackage installs the .deb in PLATEN_LINUX_DEB with systemd,
// sets a locked server, waits until the service added the queues, prints
// and purges the package again. It runs as root (CI).
func TestLinuxPackage(t *testing.T) {
	deb := os.Getenv("PLATEN_LINUX_DEB")
	if deb == "" {
		t.Skip("set PLATEN_LINUX_DEB to a package to install")
	}
	st, addr := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	now := time.Now()
	st.CreatePanelUser(ctx, &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now})

	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	t.Cleanup(func() {
		if out, err := exec.Command("journalctl", "-u", "platen-desktop", "--no-pager", "-n", "50").CombinedOutput(); err == nil {
			t.Logf("journal:\n%s", out)
		}
		if log, err := os.ReadFile("/var/lib/platen-desktop/platen.log"); err == nil {
			t.Logf("service log:\n%s", log)
		}
		exec.Command("apt-get", "purge", "-y", "platen-desktop").Run()
	})
	run("apt-get", "install", "-y", deb)
	run("platen-desktop", "set-server", addr, "-lock")

	var lab string
	for ctx.Err() == nil && lab == "" {
		installed, _ := InstalledPrinters(ctx)
		for _, p := range installed {
			if id, ok := p.PrinterID(); ok && id == "lab" {
				lab = CUPSQueue(id)
			}
		}
		time.Sleep(time.Second)
	}
	if lab == "" {
		t.Fatal("the service did not add the queues")
	}

	// The settings page of the service, as the users' tray icons find it.
	url, ok := RunningUI("/run/platen-desktop/ui.json")
	if !ok {
		t.Fatal("settings page of the service not reachable")
	}
	var state State
	for ctx.Err() == nil {
		resp, err := http.Get(url + "api/state")
		if err != nil {
			t.Fatal(err)
		}
		state = State{}
		json.NewDecoder(resp.Body).Decode(&state)
		resp.Body.Close()
		if !state.Busy && state.LastSync != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !state.Locked || state.Address != addr || state.Me == nil {
		t.Fatalf("service state %+v", state)
	}

	lp(t, lab, "Package test page", writeLargeText(t))
	job := waitForJob(ctx, t, st)
	if job.QueueID != "lab" || job.ContentType != "application/pdf" || job.Title != "Package test page" {
		t.Fatalf("job %+v", job)
	}
	t.Logf("job %q, %d bytes", job.Title, job.Size)

	run("apt-get", "purge", "-y", "platen-desktop")
	if installed, _ := InstalledPrinters(ctx); len(installed) != 0 {
		t.Fatalf("queues left after purge: %+v", installed)
	}
}
