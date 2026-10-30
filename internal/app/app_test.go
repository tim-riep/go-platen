package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/config"
)

// loopback is a mesh.Network over the loopback interface. It exists only
// in test code; production code always uses NetBird.
type loopback struct {
	mu   sync.Mutex
	addr string
	peer string
}

func (l *loopback) Listen(int) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		l.mu.Lock()
		l.addr = ln.Addr().String()
		l.mu.Unlock()
	}
	return ln, err
}

func (l *loopback) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func (l *loopback) HTTPClient() *http.Client { return &http.Client{} }

func (l *loopback) PeerForAddr(string) (string, string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peer, l.peer + ".mesh.test", l.peer != ""
}

func (l *loopback) Stop(context.Context) error { return nil }

func (l *loopback) url() string {
	for i := 0; i < 200; i++ {
		l.mu.Lock()
		a := l.addr
		l.mu.Unlock()
		if a != "" {
			return "http://" + a
		}
		time.Sleep(10 * time.Millisecond)
	}
	panic("server did not listen")
}

func call(t *testing.T, method, url, token string, in, out any, header ...string) int {
	t.Helper()
	var body io.Reader
	switch b := in.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		body = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestServerAndWorkerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	admin := auth.NewToken(auth.PrefixAdmin)
	scfg := &config.Server{
		NetBird: config.NetBird{Port: 8080},
		Server: config.ServerOpts{
			Database: filepath.Join(dir, "db"), SpoolDir: filepath.Join(dir, "spool"),
			AdminTokenHash: auth.Hash(admin), MaxDocumentSize: 1 << 20,
		},
		LocalWorker: config.LocalWorker{Enabled: true, Name: "local", JournalDir: filepath.Join(dir, "local-journal")},
	}
	net1 := &loopback{peer: "worker-peer"}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- RunServer(ctx, scfg, net1, log, "test") }()
	base := net1.url()

	// A printer on the "local network" of the worker.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	printed := make(chan []byte, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(c)
			c.Close()
			if len(b) > 0 {
				printed <- b
			}
		}
	}()

	if s := call(t, "POST", base+"/admin/v1/workers", admin, map[string]any{"id": "site1"}, nil); s != 201 {
		t.Fatalf("create worker: %d", s)
	}
	if s := call(t, "POST", base+"/admin/v1/printers", admin, map[string]any{"id": "zebra", "worker_id": "site1", "address": ln.Addr().String(), "backend": "raw"}, nil); s != 201 {
		t.Fatalf("create printer: %d", s)
	}
	var client api.AppClient
	call(t, "POST", base+"/admin/v1/app-clients", admin, map[string]any{"id": "erp", "printers": []string{"zebra"}}, &client)
	var tok api.EnrollmentToken
	call(t, "POST", base+"/admin/v1/workers/site1/enrollment-tokens", admin, nil, &tok)

	tokenFile := filepath.Join(dir, "enroll")
	os.WriteFile(tokenFile, []byte(tok.Token+"\n"), 0o600)
	wcfg := &config.Worker{Worker: config.WorkerOpts{
		ServerURL: base, CredentialFile: filepath.Join(dir, "cred"), JournalDir: filepath.Join(dir, "journal"),
		EnrollmentTokenFile: tokenFile,
	}}
	wctx, wcancel := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunWorker(wctx, wcfg, &loopback{}, log, "test") }()

	var job api.Job
	if s := call(t, "POST", base+"/v1/printers/zebra/jobs?title=label", client.Token, []byte("^XA^XZ"), &job, "Content-Type", "application/vnd.zebra-zpl"); s != 202 {
		t.Fatalf("submit: %d", s)
	}
	select {
	case b := <-printed:
		if string(b) != "^XA^XZ" {
			t.Fatalf("printed %q", b)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("nothing printed")
	}
	deadline := time.Now().Add(10 * time.Second)
	for job.Status != "done" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		call(t, "GET", base+"/v1/jobs/"+job.ID, client.Token, nil, &job)
	}
	if job.Status != "done" {
		t.Fatalf("job %s", job.Status)
	}
	if fi, err := os.Stat(wcfg.Worker.CredentialFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", fi, err)
	}

	// The worker restarts with its stored credential.
	wcancel()
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	os.Remove(tokenFile)
	wctx, wcancel = context.WithCancel(ctx)
	go func() { workerDone <- RunWorker(wctx, wcfg, &loopback{}, log, "test") }()
	call(t, "POST", base+"/v1/printers/zebra/jobs", client.Token, []byte("second"), &job, "Content-Type", "text/plain")
	select {
	case b := <-printed:
		if string(b) != "second" {
			t.Fatalf("printed %q", b)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("nothing printed after restart")
	}
	wcancel()
	<-workerDone

	cancel()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not shut down")
	}
}
