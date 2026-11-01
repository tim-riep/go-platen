// Package integration tests server and worker together without NetBird.
// Connections run over regular loopback listeners, which exist only in
// this test code; the platen binary has no such mode.
package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/backend"
	ippbackend "github.com/tim-riep/go-platen/internal/backend/ipp"
	"github.com/tim-riep/go-platen/internal/backend/raw"
	"github.com/tim-riep/go-platen/internal/server"
	"github.com/tim-riep/go-platen/internal/spool"
	"github.com/tim-riep/go-platen/internal/store"
	"github.com/tim-riep/go-platen/internal/worker"
)

func testLogger(t *testing.T) *slog.Logger {
	if os.Getenv("PLATEN_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePrinter is a raw TCP printer that records every connection.
type fakePrinter struct {
	ln   net.Listener
	mu   sync.Mutex
	jobs [][]byte
	recv chan []byte
}

func newFakePrinter(t *testing.T) *fakePrinter {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePrinter{ln: ln, recv: make(chan []byte, 16)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				if len(b) == 0 {
					return // reachability probe
				}
				p.mu.Lock()
				p.jobs = append(p.jobs, b)
				p.mu.Unlock()
				p.recv <- b
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return p
}

func (p *fakePrinter) addr() string { return p.ln.Addr().String() }

func (p *fakePrinter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.jobs)
}

// closedAddr returns an address nobody listens on.
func closedAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

type fixedPeer struct{ id string }

func (f fixedPeer) PeerForAddr(string) (server.Peer, bool) { return server.Peer{ID: f.id}, true }

type env struct {
	t        *testing.T
	ctx      context.Context
	srv      *server.Server
	http     *httptest.Server
	store    store.Store
	appToken string
	backends *backend.Registry
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	st, err := store.OpenSQLite(ctx, filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sp, err := spool.Open(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	reg := backend.NewRegistry(raw.Factory, ippbackend.Factory)
	srv, err := server.New(server.Options{
		Store: st, Spool: sp, Peers: fixedPeer{"peer-a"}, Logger: testLogger(t), LocalWorker: true,
		Backends: reg.ContentTypes, MaxAttempts: 2, LeaseDuration: 3 * time.Second, ExpiryInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Init(ctx); err != nil {
		t.Fatal(err)
	}
	go srv.Run(ctx)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	e := &env{t: t, ctx: ctx, srv: srv, http: hs, store: st, backends: reg}
	e.appToken = auth.NewToken(auth.PrefixApp)
	now := time.Now()
	if err := st.CreateAppClient(ctx, &store.AppClient{ID: "erp", Name: "ERP", TokenHash: auth.Hash(e.appToken), AllPrinters: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) addWorker(id string) {
	now := time.Now()
	if err := e.store.CreateWorker(e.ctx, &store.Worker{ID: id, Name: id, CreatedAt: now, UpdatedAt: now}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) addPrinter(id, workerID, addr string) {
	now := time.Now()
	err := e.store.CreatePrinter(e.ctx, &store.Printer{ID: id, Name: id, WorkerID: workerID, Address: addr, Backend: "raw",
		PaperSize: "A4", ResolutionDPI: 600, Status: store.PrinterReady, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		e.t.Fatal(err)
	}
}

// enroll enrolls a remote worker over HTTP and returns its transport.
func (e *env) enroll(id string) *worker.HTTPTransport {
	token := auth.NewToken(auth.PrefixEnrollment)
	now := time.Now()
	if err := e.store.CreateEnrollmentToken(e.ctx, &store.EnrollmentToken{ID: auth.NewID(), WorkerID: id, TokenHash: auth.Hash(token), ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
		e.t.Fatal(err)
	}
	reg, err := worker.Register(e.ctx, http.DefaultClient, e.http.URL, token, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	return &worker.HTTPTransport{BaseURL: e.http.URL, Client: http.DefaultClient, Credential: reg.Credential}
}

func (e *env) startWorker(t worker.Transport, journalDir string) context.CancelFunc {
	e.t.Helper()
	j, err := worker.OpenJournal(journalDir)
	if err != nil {
		e.t.Fatal(err)
	}
	w, err := worker.New(worker.Options{
		Transport: t, Journal: j, Backends: e.backends, Logger: testLogger(e.t), Version: "test",
		TempDir: e.t.TempDir(), LeaseWait: time.Second, HeartbeatInterval: 200 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		<-done
	}
	e.t.Cleanup(stop)
	return stop
}

func (e *env) submit(printer, doc string, copies int) string {
	e.t.Helper()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/printers/%s/jobs?copies=%d", e.http.URL, printer, copies), strings.NewReader(doc))
	req.Header.Set("Authorization", "Bearer "+e.appToken)
	req.Header.Set("Content-Type", "application/vnd.zebra-zpl")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("submit: %d %s", resp.StatusCode, b)
	}
	var j api.Job
	if err := jsonDecode(resp.Body, &j); err != nil {
		e.t.Fatal(err)
	}
	return j.ID
}

func (e *env) waitStatus(jobID string, want store.JobStatus) *store.Job {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := e.store.GetJob(e.ctx, jobID)
		if err != nil {
			e.t.Fatal(err)
		}
		if j.Status == want {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %s is %s (%s), want %s", jobID, j.Status, j.Error, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func receive(t *testing.T, p *fakePrinter) []byte {
	t.Helper()
	select {
	case b := <-p.recv:
		return b
	case <-time.After(10 * time.Second):
		t.Fatal("printer received nothing")
		return nil
	}
}

func TestRemoteWorkerPrints(t *testing.T) {
	e := newEnv(t)
	p := newFakePrinter(t)
	e.addWorker("w1")
	e.addPrinter("label", "w1", p.addr())
	tr := e.enroll("w1")
	e.startWorker(tr, t.TempDir())

	id := e.submit("label", "^XA^FDhello^FS^XZ", 2)
	got := receive(t, p)
	if want := strings.Repeat("^XA^FDhello^FS^XZ", 2); string(got) != want {
		t.Fatalf("printer got %q, want %q", got, want)
	}
	e.waitStatus(id, store.JobDone)

	// Heartbeats report reachability.
	deadline := time.Now().Add(5 * time.Second)
	for {
		pr, _ := e.store.GetPrinter(e.ctx, "label")
		if pr.Reachable != nil && *pr.Reachable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no reachability reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBuiltinWorkerPrints(t *testing.T) {
	e := newEnv(t)
	p := newFakePrinter(t)
	e.addPrinter("office", store.LocalWorkerID, p.addr())
	e.startWorker(&worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}, t.TempDir())

	for i := 0; i < 3; i++ {
		id := e.submit("office", fmt.Sprintf("doc %d", i), 1)
		if got := receive(t, p); string(got) != fmt.Sprintf("doc %d", i) {
			t.Fatalf("got %q", got)
		}
		e.waitStatus(id, store.JobDone)
	}
}

func TestSerialPerPrinter(t *testing.T) {
	e := newEnv(t)
	// A printer that accepts slowly; jobs must never overlap.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var active, maxActive, printed atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				first := make([]byte, 1)
				if n, _ := c.Read(first); n == 0 {
					return // reachability probe
				}
				n := active.Add(1)
				for {
					m := maxActive.Load()
					if n <= m || maxActive.CompareAndSwap(m, n) {
						break
					}
				}
				io.Copy(io.Discard, c)
				time.Sleep(50 * time.Millisecond)
				printed.Add(1)
				active.Add(-1)
			}()
		}
	}()
	e.addPrinter("slow", store.LocalWorkerID, ln.Addr().String())
	e.startWorker(&worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}, t.TempDir())
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, e.submit("slow", "x", 1))
	}
	for _, id := range ids {
		e.waitStatus(id, store.JobDone)
	}
	// Connections carrying data must have been strictly serial.
	if printed.Load() != 4 {
		t.Fatalf("printed %d jobs", printed.Load())
	}
	if maxActive.Load() > 1 {
		t.Fatalf("%d concurrent connections to one printer", maxActive.Load())
	}
}

func TestCrashDuringSendIsReportedUncertain(t *testing.T) {
	e := newEnv(t)
	p := newFakePrinter(t)
	e.addPrinter("office", store.LocalWorkerID, p.addr())
	id := e.submit("office", "invoice", 1)

	// Simulate a worker that leased the job, journaled "sent" and crashed.
	lease, err := e.srv.Workers.Lease(e.ctx, store.LocalWorkerID, nil, 0)
	if err != nil || lease == nil || lease.Job.ID != id {
		t.Fatalf("lease: %+v %v", lease, err)
	}
	dir := t.TempDir()
	j, _ := worker.OpenJournal(dir)
	if err := j.Put(worker.Entry{JobID: id, PrinterID: "office", State: worker.StateSent}); err != nil {
		t.Fatal(err)
	}

	e.startWorker(&worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}, dir)
	job := e.waitStatus(id, store.JobFailed)
	if !strings.Contains(job.Error, "uncertain") {
		t.Fatalf("error %q", job.Error)
	}
	time.Sleep(200 * time.Millisecond)
	if p.count() != 0 {
		t.Fatal("uncertain job was printed again")
	}
	if entries, _ := j.List(); len(entries) != 0 {
		t.Fatalf("journal not cleaned: %+v", entries)
	}
}

func TestJournaledJobIsNotReprinted(t *testing.T) {
	e := newEnv(t)
	p := newFakePrinter(t)
	e.addPrinter("office", store.LocalWorkerID, p.addr())
	id := e.submit("office", "invoice", 1)
	// The job was printed but the lease expired before the report arrived,
	// so the server queued it again.
	if _, err := e.srv.Workers.Lease(e.ctx, store.LocalWorkerID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ExpireLeases(e.ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	j, _ := worker.OpenJournal(dir)
	if err := j.Put(worker.Entry{JobID: id, PrinterID: "office", State: worker.StatePrinted}); err != nil {
		t.Fatal(err)
	}
	e.startWorker(&worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}, dir)
	e.waitStatus(id, store.JobDone)
	time.Sleep(200 * time.Millisecond)
	if p.count() != 0 {
		t.Fatal("job printed twice")
	}
}

// flakyTransport fails selected calls while down is set.
type flakyTransport struct {
	worker.Transport
	down      atomic.Bool
	completes atomic.Int32
}

var errDown = errors.New("server unreachable")

func (f *flakyTransport) Complete(ctx context.Context, id string) error {
	f.completes.Add(1)
	if f.down.Load() {
		return errDown
	}
	return f.Transport.Complete(ctx, id)
}

func (f *flakyTransport) Lease(ctx context.Context, printers []string, wait time.Duration) (*api.Lease, error) {
	if f.down.Load() {
		return nil, errDown
	}
	return f.Transport.Lease(ctx, printers, wait)
}

func TestReportSentAfterServerOutage(t *testing.T) {
	e := newEnv(t)
	p := newFakePrinter(t)
	e.addPrinter("office", store.LocalWorkerID, p.addr())
	ft := &flakyTransport{Transport: &worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}}
	e.startWorker(ft, t.TempDir())

	id := e.submit("office", "invoice", 1)
	ft.down.Store(true)
	receive(t, p)
	for ft.completes.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if j, _ := e.store.GetJob(e.ctx, id); j.Status.Final() {
		t.Fatalf("job final while server down: %s", j.Status)
	}
	ft.down.Store(false)
	e.waitStatus(id, store.JobDone)
	if p.count() != 1 {
		t.Fatalf("printed %d times", p.count())
	}
}

func TestUnreachablePrinterRetriesThenFails(t *testing.T) {
	e := newEnv(t)
	e.addPrinter("gone", store.LocalWorkerID, closedAddr(t))
	e.startWorker(&worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}, t.TempDir())
	id := e.submit("gone", "x", 1)
	j := e.waitStatus(id, store.JobFailed)
	if j.Attempts != 2 || !strings.Contains(j.Error, "giving up") {
		t.Fatalf("job: attempts %d error %q", j.Attempts, j.Error)
	}
}

func TestGracefulShutdownCompletesPrint(t *testing.T) {
	e := newEnv(t)
	// A printer that reads slowly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	started := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var buf bytes.Buffer
				one := make([]byte, 1)
				n, _ := c.Read(one)
				if n == 0 {
					return
				}
				started <- struct{}{}
				buf.Write(one[:n])
				time.Sleep(300 * time.Millisecond)
				io.Copy(&buf, c)
				got <- buf.Bytes()
			}()
		}
	}()
	e.addPrinter("slow", store.LocalWorkerID, ln.Addr().String())
	stop := e.startWorker(&worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}, t.TempDir())
	id := e.submit("slow", "complete document", 1)
	<-started
	stop()
	if b := <-got; string(b) != "complete document" {
		t.Fatalf("got %q", b)
	}
	e.waitStatus(id, store.JobDone)
}
