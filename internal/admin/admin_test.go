package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/server"
	"github.com/tim-riep/go-platen/internal/spool"
	"github.com/tim-riep/go-platen/internal/store"
)

type noPeers struct{}

func (noPeers) PeerForAddr(string) (server.Peer, bool) { return server.Peer{}, false }

func newClient(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sp, _ := spool.Open(filepath.Join(dir, "spool"))
	token := auth.NewToken(auth.PrefixAdmin)
	srv, err := server.New(server.Options{Store: st, Spool: sp, Peers: noPeers{}, AdminTokenHash: auth.Hash(token),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), LocalWorker: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &Client{BaseURL: hs.URL, HTTP: http.DefaultClient, Token: token}
}

func run(t *testing.T, c *Client, line string, out any) {
	t.Helper()
	var buf bytes.Buffer
	if err := Run(context.Background(), c, strings.Fields(line), &buf); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	if out != nil {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			t.Fatalf("%s: decode %q: %v", line, buf.String(), err)
		}
	}
}

func TestCommands(t *testing.T) {
	c := newClient(t)
	var w api.Worker
	run(t, c, "worker create site1 -name Warehouse -location Hamburg", &w)
	if w.Name != "Warehouse" || w.Location != "Hamburg" {
		t.Fatalf("worker: %+v", w)
	}
	run(t, c, "worker update site1 -location Bremen", &w)
	if w.Name != "Warehouse" || w.Location != "Bremen" {
		t.Fatalf("updated worker: %+v", w)
	}
	var tok api.EnrollmentToken
	run(t, c, "worker enroll site1 -ttl 1h", &tok)
	if !strings.HasPrefix(tok.Token, auth.PrefixEnrollment) {
		t.Fatalf("token: %+v", tok)
	}

	var p api.AdminPrinter
	run(t, c, "printer create zebra -worker site1 -address 10.0.0.5 -backend raw -option dither=threshold -option x=1", &p)
	if p.Address != "10.0.0.5:9100" || p.Options["x"] != "1" || p.Status != "ready" {
		t.Fatalf("printer: %+v", p)
	}
	run(t, c, "printer update zebra -status paused -dpi 300", &p)
	if p.Status != "paused" || p.ResolutionDPI != 300 || p.Options["x"] != "1" {
		t.Fatalf("updated printer: %+v", p)
	}
	var printers api.List[api.AdminPrinter]
	run(t, c, "printer list -worker site1", &printers)
	if len(printers.Items) != 1 {
		t.Fatalf("printers: %+v", printers)
	}

	var ac api.AppClient
	run(t, c, "client create erp -printers zebra", &ac)
	if ac.Token == "" || len(ac.Printers) != 1 {
		t.Fatalf("client: %+v", ac)
	}
	var rotated api.AppClient
	run(t, c, "client rotate-token erp", &rotated)
	if rotated.Token == "" || rotated.Token == ac.Token {
		t.Fatal("token not rotated")
	}
	run(t, c, "client update erp -all-printers -printers=", &ac)
	if !ac.AllPrinters || len(ac.Printers) != 0 {
		t.Fatalf("updated client: %+v", ac)
	}

	var jobs api.List[api.AdminJob]
	run(t, c, "job list -status queued -limit 5", &jobs)
	if len(jobs.Items) != 0 {
		t.Fatal(jobs)
	}

	var pu api.PanelUser
	run(t, c, "panel-user add 100.64.0.5 -name Laptop -role admin", &pu)
	if pu.IP != "100.64.0.5" || pu.Role != "admin" || pu.Bound {
		t.Fatalf("panel user: %+v", pu)
	}
	run(t, c, "panel-user add 100.64.0.6", &pu)
	if pu.Role != "viewer" {
		t.Fatalf("default role: %+v", pu)
	}
	run(t, c, "panel-user update 100.64.0.6 -role admin", &pu)
	run(t, c, "panel-user reset 100.64.0.5", &pu)
	run(t, c, "panel-user delete 100.64.0.6", nil)
	var pus api.List[api.PanelUser]
	run(t, c, "panel-user list", &pus)
	if len(pus.Items) != 1 {
		t.Fatalf("panel users: %+v", pus)
	}

	run(t, c, "printer delete zebra", nil)
	run(t, c, "client delete erp", nil)
	run(t, c, "worker delete site1", nil)
	var workers api.List[api.Worker]
	run(t, c, "worker list", &workers)
	if len(workers.Items) != 1 || workers.Items[0].ID != store.LocalWorkerID {
		t.Fatalf("workers: %+v", workers)
	}
}

func TestErrors(t *testing.T) {
	c := newClient(t)
	err := Run(context.Background(), c, []string{"worker", "get", "nope"}, io.Discard)
	if !api.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("expected 404, got %v", err)
	}
	for _, line := range []string{"", "worker", "fleet list", "worker frobnicate x", "worker get", "worker get a b", "printer create x -bogus"} {
		if err := Run(context.Background(), c, strings.Fields(line), io.Discard); !errors.Is(err, ErrUsage) {
			t.Errorf("%q: expected usage error, got %v", line, err)
		}
	}
	bad := &Client{BaseURL: c.BaseURL, HTTP: http.DefaultClient, Token: "wrong"}
	if err := Run(context.Background(), bad, []string{"worker", "list"}, io.Discard); !api.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("expected 401, got %v", err)
	}
}
