package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/server"
	"github.com/tim-riep/go-platen/internal/spool"
	"github.com/tim-riep/go-platen/internal/store"
)

// peers makes every connection the NetBird peer "pc".
type peers struct{}

func (peers) PeerForAddr(string) (server.Peer, bool) {
	return server.Peer{ID: "pc-key", FQDN: "pc.netbird.test"}, true
}

// fakePrinters records the local printers in memory.
type fakePrinters struct {
	mu      sync.Mutex
	list    []LocalPrinter
	applied [][]PrinterAction
}

func (f *fakePrinters) Installed(context.Context) ([]LocalPrinter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LocalPrinter(nil), f.list...), nil
}

func (f *fakePrinters) Apply(_ context.Context, actions []PrinterAction, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, actions)
	for _, a := range actions {
		switch a.Kind {
		case "add":
			f.list = append(f.list, LocalPrinter{Name: a.Name, PortName: PortPrefix + a.PrinterID})
		case "rename", "remove":
			for i, p := range f.list {
				if p.PortName == PortPrefix+a.PrinterID {
					if a.Kind == "remove" {
						f.list = append(f.list[:i], f.list[i+1:]...)
					} else {
						f.list[i].Name = a.Name
					}
					break
				}
			}
		}
	}
	return nil
}

type login struct{ on bool }

func (l *login) Enabled() bool            { return l.on }
func (l *login) SetEnabled(on bool) error { l.on = on; return nil }

func newServer(t *testing.T) (store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sp, _ := spool.Open(filepath.Join(dir, "spool"))
	srv, err := server.New(server.Options{Store: st, Spool: sp, Peers: peers{}, LocalWorker: true, Panel: true,
		PanelHosts: func() []string { return []string{"127.0.0.1"} },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	now := time.Now()
	for _, p := range []*store.Printer{
		{ID: "office", Name: "Office", WorkerID: store.LocalWorkerID, Address: "10.0.0.5:9100", Backend: "pdf-to-raster", PaperSize: "A4", ResolutionDPI: 600},
		{ID: "lab", Name: "Lab", WorkerID: store.LocalWorkerID, Address: "10.0.0.6:9100", Backend: "pdf-to-raster", PaperSize: "A4", ResolutionDPI: 600},
	} {
		p.Status, p.CreatedAt, p.UpdatedAt = store.PrinterReady, now, now
		if err := st.CreatePrinter(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	return st, strings.TrimPrefix(hs.URL, "http://")
}

func TestAppEndToEnd(t *testing.T) {
	st, addr := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	printers := &fakePrinters{list: []LocalPrinter{{Name: "Office", PortName: "USB001"}}}
	lg := &login{}
	app := &App{Dir: t.TempDir(), Version: "test", Printers: printers, Login: lg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// The device has no print access yet.
	if err := app.Connect(ctx, addr); !strings.Contains(fmt.Sprint(err), "may not print") {
		t.Fatalf("connect without access: %v", err)
	}
	if app.Address() != "" || lg.on {
		t.Fatal("failed connect stored")
	}
	now := time.Now()
	st.CreatePanelUser(ctx, &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now})

	if err := app.Connect(ctx, addr); err != nil {
		t.Fatal(err)
	}
	s := app.State()
	if s.Me == nil || s.Me.PanelRole != "viewer" || len(s.Printers) != 2 || !lg.on {
		t.Fatalf("state %+v", s)
	}
	names := map[string]string{}
	for _, p := range s.Printers {
		names[p.ID] = p.LocalName
	}
	if names["office"] != "Office (platen)" || names["lab"] != "Lab" {
		t.Fatalf("local names %v", names)
	}

	// A printer delivers a document, as the Windows spooler does through
	// the printer's pipe.
	pdf := []byte("%PDF-1.7 test document")
	if err := app.Receive("lab", "Invoice 42", bytes.NewReader(pdf)); err != nil {
		t.Fatal(err)
	}
	if err := app.Receive("lab", "", bytes.NewReader(nil)); err == nil {
		t.Fatal("empty document accepted")
	}

	var jobs []store.Job
	for i := 0; i < 100; i++ {
		jobs, _ = st.ListJobs(ctx, store.JobFilter{})
		if len(jobs) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(jobs) != 1 || jobs[0].QueueID != "lab" || jobs[0].Title != "Invoice 42" || jobs[0].AppClientID != "device:127.0.0.1" ||
		jobs[0].ContentType != "application/pdf" || jobs[0].Size != int64(len(pdf)) {
		t.Fatalf("jobs %+v", jobs)
	}
	for i := 0; i < 100 && len(app.State().Jobs) > 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(app.State().Jobs); n != 0 {
		t.Fatalf("%d jobs left in the spool", n)
	}

	// A job for an unknown printer fails for good.
	if _, err := app.spool.Add("nope", "x", 1, writeTemp(t, pdf), time.Now()); err != nil {
		t.Fatal(err)
	}
	app.fwd.Run(ctx, time.Now())
	if js := app.State().Jobs; len(js) != 1 || !js[0].Failed {
		t.Fatalf("failed job %+v", js)
	}

	// Disconnecting removes only the app's printers.
	if err := app.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if l, _ := printers.Installed(ctx); len(l) != 1 || l[0].PortName != "USB001" || app.Address() != "" {
		t.Fatalf("after disconnect %+v", l)
	}
}

func writeTemp(t *testing.T, data []byte) string {
	p := filepath.Join(t.TempDir(), "doc")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUI(t *testing.T) {
	app := &App{Dir: t.TempDir(), Version: "test", Printers: &fakePrinters{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ui, err := StartUI(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	if url, ok := RunningUI(app.Dir); !ok || url != ui.URL {
		t.Fatalf("running UI %q %v", url, ok)
	}
	get := func(url string) int {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get(ui.URL) != 200 || get(ui.URL+"app.js") != 200 || get(ui.URL+"api/state") != 200 {
		t.Fatal("page not served")
	}
	if get(strings.Replace(ui.URL, ui.prefix, "/wrong/", 1)+"api/state") != 404 {
		t.Fatal("served without the token")
	}
	post := func(path string, h map[string]string, body any) (int, map[string]any) {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ui.URL+path, bytes.NewReader(data))
		for k, v := range h {
			req.Header.Set(k, v)
		}
		if h["Host"] != "" {
			req.Host = h["Host"]
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post("api/connect", nil, map[string]string{"address": "x"}); code != 403 {
		t.Fatalf("change without header: %d", code)
	}
	if code, _ := post("api/sync", map[string]string{uiHeader: "1", "Host": "evil.example:80"}, nil); code != 403 {
		t.Fatalf("foreign host: %d", code)
	}
	code, out := post("api/connect", map[string]string{uiHeader: "1"}, map[string]string{"address": "http://x/path"})
	if code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "invalid server address") {
		t.Fatalf("bad address: %d %v", code, out)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if _, ok := RunningUI(app.Dir); ok {
		t.Fatal("UI still registered")
	}
}
