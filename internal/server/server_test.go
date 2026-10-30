package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/spool"
	"github.com/tim-riep/go-platen/internal/store"
)

// fakePeers attributes every connection to the currently set peer.
type fakePeers struct {
	mu   sync.Mutex
	peer string
}

func (f *fakePeers) set(id string) {
	f.mu.Lock()
	f.peer = id
	f.mu.Unlock()
}

func (f *fakePeers) PeerForAddr(string) (Peer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.peer == "" {
		return Peer{}, false
	}
	return Peer{ID: f.peer, FQDN: f.peer + ".netbird.test"}, true
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t      *testing.T
	srv    *Server
	http   *httptest.Server
	peers  *fakePeers
	clock  *clock
	admin  string
	store  store.Store
	tokens map[string]string
}

func newEnv(t *testing.T, mod ...func(*Options)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sp, err := spool.Open(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	admin := auth.NewToken(auth.PrefixAdmin)
	e := &env{t: t, peers: &fakePeers{}, clock: &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, admin: admin, store: st, tokens: map[string]string{}}
	opts := Options{
		Store: st, Spool: sp, Peers: e.peers, AdminTokenHash: auth.Hash(admin),
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxDocumentSize: 1024, Now: e.clock.Now, MaxAttempts: 2, LocalWorker: true,
		Backends: func(b string) ([]string, bool) {
			switch b {
			case "raw":
				return []string{"*/*"}, true
			case "pdf":
				return []string{"application/pdf"}, true
			}
			return nil, false
		},
	}
	for _, m := range mod {
		m(&opts)
	}
	e.srv, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.http.Close)
	return e
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r resp) errCode(t *testing.T) string {
	t.Helper()
	var b api.ErrorBody
	r.decode(t, &b)
	if b.Error == nil {
		t.Fatalf("no error in body %s", r.body)
	}
	return b.Error.Code
}

func (e *env) do(method, path, token string, body any, header ...string) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		rd = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, e.http.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func (e *env) expect(r resp, status int) resp {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

// setup creates worker w1 bound to peer "peer-a" with printers p1 (raw) and
// p2 (pdf), and app client erp allowed to use p1 and p2. It returns the
// worker credential and the app token.
func (e *env) setup() (workerCred, appToken string) {
	e.t.Helper()
	e.expect(e.do("POST", "/admin/v1/workers", e.admin, map[string]any{"id": "w1", "name": "Site 1"}), 201)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "p1", "worker_id": "w1", "address": "192.168.1.10", "backend": "raw"}), 201)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "p2", "worker_id": "w1", "address": "192.168.1.11:9100", "backend": "pdf"}), 201)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "p3", "worker_id": "w1", "address": "192.168.1.12", "backend": "raw"}), 201)
	var c api.AppClient
	e.expect(e.do("POST", "/admin/v1/app-clients", e.admin, map[string]any{"id": "erp", "printers": []string{"p1", "p2"}}), 201).decode(e.t, &c)
	var tok api.EnrollmentToken
	e.expect(e.do("POST", "/admin/v1/workers/w1/enrollment-tokens", e.admin, nil), 201).decode(e.t, &tok)
	e.peers.set("peer-a")
	var reg api.RegisterResponse
	e.expect(e.do("POST", "/worker/v1/register", "", api.RegisterRequest{Token: tok.Token}), 200).decode(e.t, &reg)
	if reg.WorkerID != "w1" || !strings.HasPrefix(reg.Credential, auth.PrefixWorker) {
		e.t.Fatalf("register: %+v", reg)
	}
	e.expect(e.do("POST", "/worker/v1/register", "", api.RegisterRequest{Token: tok.Token}), 401)
	return reg.Credential, c.Token
}

func (e *env) submit(token, printer string, doc []byte, contentType string, header ...string) resp {
	return e.do("POST", "/v1/printers/"+printer+"/jobs?copies=2&title=Invoice", token, doc, append([]string{"Content-Type", contentType}, header...)...)
}

func (e *env) lease(cred string, wait int, printers ...string) *api.Lease {
	e.t.Helper()
	r := e.do("POST", "/worker/v1/lease", cred, api.LeaseRequest{WaitSeconds: wait, Printers: printers})
	if r.status == 204 {
		return nil
	}
	var l api.Lease
	e.expect(r, 200).decode(e.t, &l)
	return &l
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	var h api.Health
	e.expect(e.do("GET", "/v1/health", "", nil), 200).decode(t, &h)
	if h.Status != "ok" {
		t.Fatal(h)
	}
	if code := e.expect(e.do("GET", "/nope", "", nil), 404).errCode(t); code != api.CodeNotFound {
		t.Fatal(code)
	}
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()

	var cfg api.WorkerConfig
	e.expect(e.do("GET", "/worker/v1/config", cred, nil), 200).decode(t, &cfg)
	if cfg.WorkerID != "w1" || len(cfg.Printers) != 3 || cfg.Printers[0].Address != "192.168.1.10:9100" {
		t.Fatalf("config: %+v", cfg)
	}

	var printers api.PrinterList
	e.expect(e.do("GET", "/v1/printers", app, nil), 200).decode(t, &printers)
	if len(printers.Printers) != 2 {
		t.Fatalf("app sees %d printers, want 2", len(printers.Printers))
	}

	var job api.Job
	e.expect(e.submit(app, "p1", []byte("^XA^XZ"), "application/vnd.zebra-zpl"), 202).decode(t, &job)
	if job.Status != "queued" || job.Copies != 2 || job.Title != "Invoice" || job.Size != 6 {
		t.Fatalf("job: %+v", job)
	}

	l := e.lease(cred, 0)
	if l == nil || l.Job.ID != job.ID || l.Printer.ID != "p1" || l.Job.Attempt != 1 {
		t.Fatalf("lease: %+v", l)
	}
	doc := e.expect(e.do("GET", "/worker/v1/jobs/"+job.ID+"/document", cred, nil), 200)
	if string(doc.body) != "^XA^XZ" || doc.header.Get("X-Document-SHA256") != job.DocumentHash {
		t.Fatalf("document: %q %v", doc.body, doc.header)
	}
	e.expect(e.do("POST", "/worker/v1/jobs/"+job.ID+"/progress", cred, nil), 200)
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 200).decode(t, &job)
	if job.Status != "printing" {
		t.Fatalf("status after progress: %s", job.Status)
	}
	e.expect(e.do("POST", "/worker/v1/jobs/"+job.ID+"/complete", cred, nil), 204)
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 200).decode(t, &job)
	if job.Status != "done" || job.FinishedAt == nil {
		t.Fatalf("status after complete: %+v", job)
	}
	e.expect(e.do("GET", "/worker/v1/jobs/"+job.ID+"/document", cred, nil), 409)

	e.expect(e.do("POST", "/worker/v1/heartbeat", cred, api.HeartbeatRequest{Version: "v1", Printers: []api.Reachability{{PrinterID: "p1", Reachable: true}}}), 204)
	var w api.Worker
	e.expect(e.do("GET", "/admin/v1/workers/w1", e.admin, nil), 200).decode(t, &w)
	if w.Version != "v1" || w.LastHeartbeat == nil || w.PeerID != "peer-a" || !w.Enrolled {
		t.Fatalf("worker: %+v", w)
	}
}

func TestAuthSeparation(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	e.expect(e.do("GET", "/admin/v1/workers", app, nil), 401)
	e.expect(e.do("GET", "/admin/v1/workers", cred, nil), 401)
	e.expect(e.do("GET", "/admin/v1/workers", "", nil), 401)
	e.expect(e.do("GET", "/v1/printers", e.admin, nil), 401)
	e.expect(e.do("GET", "/v1/printers", cred, nil), 401)
	e.expect(e.do("GET", "/worker/v1/config", app, nil), 401)
	e.expect(e.do("GET", "/worker/v1/config", e.admin, nil), 401)
	e.expect(e.do("GET", "/worker/v1/config", cred+"x", nil), 401)
}

func TestAdminDisabledWithoutHash(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.AdminTokenHash = "" })
	e.expect(e.do("GET", "/admin/v1/workers", e.admin, nil), 403)
}

func TestPeerBinding(t *testing.T) {
	e := newEnv(t)
	cred, _ := e.setup()
	e.peers.set("peer-b")
	if code := e.expect(e.do("GET", "/worker/v1/config", cred, nil), 403).errCode(t); code != api.CodeForbidden {
		t.Fatal(code)
	}
	e.peers.set("")
	e.expect(e.do("GET", "/worker/v1/config", cred, nil), 403)
	e.expect(e.do("POST", "/worker/v1/register", "", api.RegisterRequest{Token: "x"}), 403)
	e.peers.set("peer-a")
	e.expect(e.do("GET", "/worker/v1/config", cred, nil), 200)
}

func TestAppPermissions(t *testing.T) {
	e := newEnv(t)
	_, app := e.setup()
	e.expect(e.submit(app, "p3", []byte("x"), "text/plain"), 404)
	e.expect(e.submit(app, "missing", []byte("x"), "text/plain"), 404)

	var other api.AppClient
	e.expect(e.do("POST", "/admin/v1/app-clients", e.admin, map[string]any{"id": "other", "all_printers": true}), 201).decode(t, &other)
	var job api.Job
	e.expect(e.submit(other.Token, "p3", []byte("x"), "text/plain"), 202).decode(t, &job)
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 404)
	e.expect(e.do("DELETE", "/v1/jobs/"+job.ID, app, nil), 404)

	var rotated api.AppClient
	e.expect(e.do("POST", "/admin/v1/app-clients/other/rotate-token", e.admin, nil), 200).decode(t, &rotated)
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, other.Token, nil), 401)
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, rotated.Token, nil), 200)
}

func TestSubmitValidation(t *testing.T) {
	e := newEnv(t)
	_, app := e.setup()
	if code := e.expect(e.submit(app, "p2", []byte("x"), "text/plain"), 415).errCode(t); code != api.CodeUnsupportedMedia {
		t.Fatal(code)
	}
	e.expect(e.submit(app, "p2", []byte("%PDF-1.4"), "application/pdf"), 202)
	e.expect(e.do("POST", "/v1/printers/p1/jobs", app, []byte("x")), 415)
	e.expect(e.do("POST", "/v1/printers/p1/jobs?copies=0", app, []byte("x"), "Content-Type", "text/plain"), 400)
	e.expect(e.do("POST", "/v1/printers/p1/jobs", app, []byte{}, "Content-Type", "text/plain"), 400)
	if code := e.expect(e.submit(app, "p1", bytes.Repeat([]byte("x"), 1025), "text/plain"), 413).errCode(t); code != api.CodeTooLarge {
		t.Fatal(code)
	}
	e.expect(e.submit(app, "p1", bytes.Repeat([]byte("x"), 1024), "text/plain"), 202)
}

func TestIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	_, app := e.setup()
	var a, b api.Job
	e.expect(e.submit(app, "p1", []byte("doc"), "text/plain", "Idempotency-Key", "order-1"), 202).decode(t, &a)
	r := e.expect(e.submit(app, "p1", []byte("doc"), "text/plain", "Idempotency-Key", "order-1"), 202)
	r.decode(t, &b)
	if a.ID != b.ID || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("not deduplicated: %s %s", a.ID, b.ID)
	}
	e.expect(e.submit(app, "p1", []byte("other"), "text/plain", "Idempotency-Key", "order-1"), 422)
	var jobs api.List[api.AdminJob]
	e.expect(e.do("GET", "/admin/v1/jobs", e.admin, nil), 200).decode(t, &jobs)
	if len(jobs.Items) != 1 {
		t.Fatalf("%d jobs, want 1", len(jobs.Items))
	}
	entries, _ := e.srv.spool.List()
	if len(entries) != 1 {
		t.Fatalf("%d spool files, want 1", len(entries))
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	var a, b api.Job
	e.expect(e.submit(app, "p1", []byte("a"), "text/plain"), 202).decode(t, &a)
	e.expect(e.submit(app, "p1", []byte("b"), "text/plain"), 202).decode(t, &b)
	if l := e.lease(cred, 0); l == nil || l.Job.ID != a.ID {
		t.Fatalf("lease: %+v", l)
	}
	e.expect(e.do("DELETE", "/v1/jobs/"+a.ID, app, nil), 409)
	var canceled api.Job
	e.expect(e.do("DELETE", "/v1/jobs/"+b.ID, app, nil), 200).decode(t, &canceled)
	if canceled.Status != "canceled" {
		t.Fatal(canceled.Status)
	}
}

func TestLongPoll(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	done := make(chan *api.Lease)
	start := time.Now()
	go func() {
		l, _ := e.srv.Workers.Lease(context.Background(), "w1", nil, 10*time.Second)
		done <- l
	}()
	time.Sleep(100 * time.Millisecond)
	e.expect(e.submit(app, "p1", []byte("a"), "text/plain"), 202)
	select {
	case l := <-done:
		if l == nil {
			t.Fatal("long poll returned no job")
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("long poll not woken up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not return")
	}
	// Without jobs the request returns 204 after the wait.
	if l := e.lease(cred, 1); l != nil {
		t.Fatalf("unexpected lease %+v", l)
	}
}

func TestLeaseExpiryRequeues(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	var job api.Job
	e.expect(e.submit(app, "p1", []byte("a"), "text/plain"), 202).decode(t, &job)
	if l := e.lease(cred, 0); l == nil {
		t.Fatal("no lease")
	}
	e.clock.advance(2 * time.Minute)
	e.srv.ExpireLeases(context.Background())
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 200).decode(t, &job)
	if job.Status != "queued" {
		t.Fatalf("status %s", job.Status)
	}
	e.expect(e.do("POST", "/worker/v1/jobs/"+job.ID+"/progress", cred, nil), 409)
	if l := e.lease(cred, 0); l == nil || l.Job.Attempt != 2 {
		t.Fatalf("second lease: %+v", l)
	}
	e.clock.advance(2 * time.Minute)
	e.srv.ExpireLeases(context.Background())
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 200).decode(t, &job)
	if job.Status != "failed" {
		t.Fatalf("status after max attempts: %s", job.Status)
	}
}

func TestFailUncertain(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	var job api.Job
	e.expect(e.submit(app, "p1", []byte("a"), "text/plain"), 202).decode(t, &job)
	e.lease(cred, 0)
	e.expect(e.do("POST", "/worker/v1/jobs/"+job.ID+"/fail", cred, api.FailRequest{Error: "worker crashed", Retryable: true, Uncertain: true}), 204)
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 200).decode(t, &job)
	if job.Status != "failed" || !strings.Contains(job.Error, "uncertain") {
		t.Fatalf("job: %+v", job)
	}
}

func TestFailRetryable(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	var job api.Job
	e.expect(e.submit(app, "p1", []byte("a"), "text/plain"), 202).decode(t, &job)
	e.lease(cred, 0)
	e.expect(e.do("POST", "/worker/v1/jobs/"+job.ID+"/fail", cred, api.FailRequest{Error: "connection refused", Retryable: true}), 204)
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 200).decode(t, &job)
	if job.Status != "queued" {
		t.Fatalf("job: %+v", job)
	}
}

func TestPausedPrinterAndPrinterFilter(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	e.expect(e.do("PATCH", "/admin/v1/printers/p1", e.admin, map[string]any{"status": "paused"}), 200)
	e.expect(e.submit(app, "p1", []byte("a"), "text/plain"), 202)
	if l := e.lease(cred, 0); l != nil {
		t.Fatal("paused printer leased")
	}
	e.expect(e.do("PATCH", "/admin/v1/printers/p1", e.admin, map[string]any{"status": "ready"}), 200)
	if l := e.lease(cred, 0, "p2"); l != nil {
		t.Fatal("printer filter ignored")
	}
	if l := e.lease(cred, 0, "p1"); l == nil {
		t.Fatal("no lease after unpause")
	}
}

func TestAdminCRUD(t *testing.T) {
	e := newEnv(t)
	e.setup()
	e.expect(e.do("POST", "/admin/v1/workers", e.admin, map[string]any{"id": "local"}), 400)
	e.expect(e.do("POST", "/admin/v1/workers", e.admin, map[string]any{"id": "Bad ID"}), 400)
	e.expect(e.do("POST", "/admin/v1/workers", e.admin, map[string]any{"id": "w1"}), 409)
	e.expect(e.do("DELETE", "/admin/v1/workers/local", e.admin, nil), 400)
	e.expect(e.do("POST", "/admin/v1/workers/local/enrollment-tokens", e.admin, nil), 400)
	e.expect(e.do("POST", "/admin/v1/workers/nope/enrollment-tokens", e.admin, nil), 404)
	e.expect(e.do("DELETE", "/admin/v1/workers/w1", e.admin, nil), 409)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "x", "worker_id": "w1", "address": "a", "backend": "nope"}), 400)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "x", "worker_id": "nope", "address": "a", "backend": "raw"}), 404)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "x", "worker_id": "local", "address": "a", "backend": "raw", "unknown": 1}), 400)

	var p api.AdminPrinter
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "lp", "worker_id": "local", "address": "10.1.1.1", "backend": "raw",
		"options": map[string]string{"dither": "threshold"}}), 201).decode(t, &p)
	if p.Address != "10.1.1.1:9100" || p.Options["dither"] != "threshold" || p.ResolutionDPI != 600 || p.PaperSize != "A4" {
		t.Fatalf("printer: %+v", p)
	}
	e.expect(e.do("PATCH", "/admin/v1/printers/lp", e.admin, map[string]any{"name": "Label", "resolution_dpi": 300}), 200).decode(t, &p)
	if p.Name != "Label" || p.ResolutionDPI != 300 || p.Backend != "raw" {
		t.Fatalf("patched printer: %+v", p)
	}
	e.expect(e.do("DELETE", "/admin/v1/printers/lp", e.admin, nil), 204)
	e.expect(e.do("GET", "/admin/v1/printers/lp", e.admin, nil), 404)

	var workers api.List[api.Worker]
	e.expect(e.do("GET", "/admin/v1/workers", e.admin, nil), 200).decode(t, &workers)
	if len(workers.Items) != 2 || workers.Items[0].ID != "local" {
		t.Fatalf("workers: %+v", workers)
	}
	var c api.AppClient
	e.expect(e.do("PATCH", "/admin/v1/app-clients/erp", e.admin, map[string]any{"printers": []string{"p3"}}), 200).decode(t, &c)
	if len(c.Printers) != 1 || c.Printers[0] != "p3" || c.Token != "" {
		t.Fatalf("app client: %+v", c)
	}
	e.expect(e.do("DELETE", "/admin/v1/app-clients/erp", e.admin, nil), 204)
}

func TestCleanup(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.JobRetention = time.Hour })
	_, app := e.setup()
	var job api.Job
	e.expect(e.submit(app, "p1", []byte("a"), "text/plain"), 202).decode(t, &job)
	e.expect(e.do("DELETE", "/v1/jobs/"+job.ID, app, nil), 200)
	e.srv.Cleanup(context.Background())
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 200)
	e.clock.advance(2 * time.Hour)
	e.srv.Cleanup(context.Background())
	e.expect(e.do("GET", "/v1/jobs/"+job.ID, app, nil), 404)
	if entries, _ := e.srv.spool.List(); len(entries) != 0 {
		t.Fatalf("spool not cleaned: %+v", entries)
	}
}

func TestConcurrentLeaseOverHTTP(t *testing.T) {
	e := newEnv(t)
	cred, app := e.setup()
	e.expect(e.do("PATCH", "/admin/v1/app-clients/erp", e.admin, map[string]any{"all_printers": true}), 200)
	for i := 0; i < 3; i++ {
		e.expect(e.submit(app, fmt.Sprintf("p%d", i+1), []byte("x"), "application/pdf"), 202)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.do("POST", "/worker/v1/lease", cred, api.LeaseRequest{})
			if r.status != 200 {
				return
			}
			var l api.Lease
			r.decode(t, &l)
			mu.Lock()
			if seen[l.Job.ID] {
				t.Errorf("job %s leased twice", l.Job.ID)
			}
			seen[l.Job.ID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 3 {
		t.Fatalf("leased %d jobs, want 3", len(seen))
	}
}
