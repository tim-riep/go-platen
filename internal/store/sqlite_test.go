package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func openTest(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "platen.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seed(t *testing.T, s *SQLite, workerID string, printers ...string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateWorker(ctx, &Worker{ID: workerID, Name: workerID, CreatedAt: t0, UpdatedAt: t0}); err != nil && !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	for _, p := range printers {
		err := s.CreatePrinter(ctx, &Printer{ID: p, Name: p, WorkerID: workerID, Address: "10.0.0.1:9100", Backend: "raw",
			PaperSize: "A4", ResolutionDPI: 600, Status: PrinterReady, CreatedAt: t0, UpdatedAt: t0})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func newJob(id, printer string) *Job {
	return &Job{ID: id, QueueID: printer, Status: JobQueued, Copies: 1, ContentType: "application/octet-stream",
		Size: 3, DocumentHash: "h-" + id, MaxAttempts: 3, CreatedAt: t0, UpdatedAt: t0}
}

func mustCreate(t *testing.T, s *SQLite, j *Job) *Job {
	t.Helper()
	out, created, err := s.CreateJob(context.Background(), j)
	if err != nil || !created {
		t.Fatalf("create job: created=%v err=%v", created, err)
	}
	return out
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	for i := 0; i < 2; i++ {
		s, err := OpenSQLite(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestWorkerCRUD(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	if err := s.CreateWorker(ctx, &Worker{ID: "w1", Name: "x", CreatedAt: t0, UpdatedAt: t0}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate worker: %v", err)
	}
	if err := s.DeleteWorker(ctx, "w1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete worker with printers: %v", err)
	}
	if err := s.DeletePrinter(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorker(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted worker: %v", err)
	}
}

func TestEnrollment(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1")
	tok := &EnrollmentToken{ID: "t1", WorkerID: "w1", TokenHash: "th", ExpiresAt: t0.Add(time.Hour), CreatedAt: t0}
	if err := s.CreateEnrollmentToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemEnrollmentToken(ctx, Enrollment{TokenHash: "th", CredentialHash: "ch", PeerID: "peer", Now: t0.Add(2 * time.Hour)}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expired token: %v", err)
	}
	w, err := s.RedeemEnrollmentToken(ctx, Enrollment{TokenHash: "th", CredentialHash: "ch", PeerID: "peer", Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if w.CredentialHash != "ch" || w.PeerID != "peer" || w.EnrolledAt == nil {
		t.Fatalf("worker not bound: %+v", w)
	}
	if _, err := s.RedeemEnrollmentToken(ctx, Enrollment{TokenHash: "th", CredentialHash: "x", Now: t0}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("reused token: %v", err)
	}
	got, err := s.GetWorkerByCredentialHash(ctx, "ch")
	if err != nil || got.ID != "w1" {
		t.Fatalf("by credential: %v %v", got, err)
	}
}

func TestPrinterOptionsAndHeartbeat(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	seed(t, s, "w2", "p2")
	p, _ := s.GetPrinter(ctx, "p1")
	p.Options = map[string]string{"dither": "threshold"}
	p.Status = PrinterPaused
	if err := s.UpdatePrinter(ctx, p); err != nil {
		t.Fatal(err)
	}
	err := s.RecordHeartbeat(ctx, Heartbeat{WorkerID: "w1", Version: "1.0", At: t0, Printers: []Reachability{
		{PrinterID: "p1", Reachable: false, Error: "refused"},
		{PrinterID: "p2", Reachable: true}, // not w1's printer, ignored
	}})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetPrinter(ctx, "p1")
	if p.Options["dither"] != "threshold" || p.Status != PrinterPaused || p.Reachable == nil || *p.Reachable || p.ReachabilityError != "refused" {
		t.Fatalf("unexpected printer: %+v", p)
	}
	p2, _ := s.GetPrinter(ctx, "p2")
	if p2.Reachable != nil {
		t.Fatal("foreign heartbeat updated printer")
	}
	q, err := s.GetQueue(ctx, "p1")
	if err != nil || len(q.PrinterIDs) != 1 || q.PrinterIDs[0] != "p1" {
		t.Fatalf("queue: %+v %v", q, err)
	}
	w, _ := s.GetWorker(ctx, "w1")
	if w.Version != "1.0" || w.LastHeartbeat == nil {
		t.Fatalf("worker heartbeat: %+v", w)
	}
}

func TestAppClients(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1", "p2")
	c := &AppClient{ID: "erp", Name: "ERP", TokenHash: "tok", PrinterIDs: []string{"p1"}, CreatedAt: t0, UpdatedAt: t0}
	if err := s.CreateAppClient(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAppClient(ctx, &AppClient{ID: "x", TokenHash: "t2", PrinterIDs: []string{"nope"}, CreatedAt: t0, UpdatedAt: t0}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown printer: %v", err)
	}
	got, err := s.GetAppClientByTokenHash(ctx, "tok")
	if err != nil || !got.MayUse("p1") || got.MayUse("p2") {
		t.Fatalf("client: %+v %v", got, err)
	}
	got.PrinterIDs = []string{"p2"}
	if err := s.UpdateAppClient(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAppClient(ctx, "erp")
	if got.MayUse("p1") || !got.MayUse("p2") {
		t.Fatalf("permissions not updated: %+v", got)
	}
	if err := s.DeletePrinter(ctx, "p2"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAppClient(ctx, "erp")
	if len(got.PrinterIDs) != 0 {
		t.Fatalf("permission of deleted printer kept: %+v", got)
	}
	list, err := s.ListAppClients(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
}

func TestIdempotency(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1", "p2")
	j := newJob("j1", "p1")
	j.AppClientID, j.IdempotencyKey = "erp", "k1"
	mustCreate(t, s, j)

	dup := newJob("j2", "p1")
	dup.AppClientID, dup.IdempotencyKey, dup.DocumentHash = "erp", "k1", j.DocumentHash
	got, created, err := s.CreateJob(ctx, dup)
	if err != nil || created || got.ID != "j1" {
		t.Fatalf("duplicate: %v %v %v", got, created, err)
	}

	other := newJob("j3", "p2")
	other.AppClientID, other.IdempotencyKey = "erp", "k1"
	if _, _, err := s.CreateJob(ctx, other); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}

	// The same key from another client is independent.
	foreign := newJob("j4", "p1")
	foreign.AppClientID, foreign.IdempotencyKey = "other", "k1"
	mustCreate(t, s, foreign)
}

func TestLeaseOrderAndSerialPerPrinter(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1", "p2")
	seed(t, s, "w2", "p3")
	mustCreate(t, s, newJob("a", "p1"))
	mustCreate(t, s, newJob("b", "p1"))
	mustCreate(t, s, newJob("c", "p2"))
	mustCreate(t, s, newJob("d", "p3"))

	lease := func(worker string, printers ...string) *Job {
		t.Helper()
		j, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: worker, PrinterIDs: printers, Now: t0, Duration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	if j := lease("w1"); j == nil || j.ID != "a" || j.Status != JobLeased || j.PrinterID != "p1" || j.Attempts != 1 {
		t.Fatalf("first lease: %+v", j)
	}
	// p1 is busy, so b must wait; c on p2 is next.
	if j := lease("w1"); j == nil || j.ID != "c" {
		t.Fatalf("second lease: %+v", j)
	}
	if j := lease("w1"); j != nil {
		t.Fatalf("expected no job while printers busy, got %s", j.ID)
	}
	if _, err := s.CompleteJob(ctx, "a", "w1", t0); err != nil {
		t.Fatal(err)
	}
	if j := lease("w1", "p2"); j != nil {
		t.Fatalf("printer filter ignored: %s", j.ID)
	}
	if j := lease("w1", "p1"); j == nil || j.ID != "b" {
		t.Fatalf("lease after complete: %+v", j)
	}
	if j := lease("w2"); j == nil || j.ID != "d" {
		t.Fatalf("w2 lease: %+v", j)
	}
}

func TestPausedPrinterNotLeased(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	p, _ := s.GetPrinter(ctx, "p1")
	p.Status = PrinterPaused
	if err := s.UpdatePrinter(ctx, p); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, newJob("a", "p1"))
	j, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute})
	if err != nil || j != nil {
		t.Fatalf("paused printer leased: %v %v", j, err)
	}
}

func TestConcurrentLeaseExactlyOnce(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const n = 20
	var printers []string
	for i := 0; i < n; i++ {
		printers = append(printers, fmt.Sprintf("p%d", i))
	}
	seed(t, s, "w1", printers...)
	for i := 0; i < n; i++ {
		mustCreate(t, s, newJob(fmt.Sprintf("j%d", i), printers[i]))
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute})
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("leased %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("job %s leased %d times", id, c)
		}
	}
}

func TestLeaseExpiryAndAttempts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	j := newJob("a", "p1")
	j.MaxAttempts = 2
	mustCreate(t, s, j)

	now := t0
	for attempt := 1; attempt <= 2; attempt++ {
		l, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: now, Duration: time.Minute})
		if err != nil || l == nil {
			t.Fatalf("attempt %d: lease %v %v", attempt, l, err)
		}
		if exp, _ := s.ExpireLeases(ctx, now.Add(30*time.Second)); len(exp) != 0 {
			t.Fatal("lease expired early")
		}
		now = now.Add(2 * time.Minute)
		exp, err := s.ExpireLeases(ctx, now)
		if err != nil || len(exp) != 1 {
			t.Fatalf("expire: %v %v", exp, err)
		}
		want := JobQueued
		if attempt == 2 {
			want = JobFailed
		}
		if exp[0].Status != want {
			t.Fatalf("attempt %d: status %s, want %s", attempt, exp[0].Status, want)
		}
	}
}

func TestProgressExtendsLease(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	seed(t, s, "w2")
	mustCreate(t, s, newJob("a", "p1"))
	if _, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExtendLease(ctx, "a", "w2", t0, t0.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign worker extended lease: %v", err)
	}
	j, err := s.ExtendLease(ctx, "a", "w1", t0.Add(30*time.Second), t0.Add(5*time.Minute))
	if err != nil || j.Status != JobPrinting || j.StartedAt == nil {
		t.Fatalf("extend: %+v %v", j, err)
	}
	if exp, _ := s.ExpireLeases(ctx, t0.Add(2*time.Minute)); len(exp) != 0 {
		t.Fatal("extended lease expired")
	}
}

func TestLateCompleteAfterExpiry(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	mustCreate(t, s, newJob("a", "p1"))
	if _, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpireLeases(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExtendLease(ctx, "a", "w1", t0, t0.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("extend after expiry: %v", err)
	}
	j, err := s.CompleteJob(ctx, "a", "w1", t0.Add(time.Hour))
	if err != nil || j.Status != JobDone {
		t.Fatalf("late complete: %+v %v", j, err)
	}
	if _, err := s.CompleteJob(ctx, "a", "w1", t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete twice: %v", err)
	}
}

func TestFailRetry(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	j := newJob("a", "p1")
	j.MaxAttempts = 2
	mustCreate(t, s, j)
	lease := func() {
		if l, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute}); err != nil || l == nil {
			t.Fatalf("lease: %v %v", l, err)
		}
	}
	lease()
	got, err := s.FailJob(ctx, FailRequest{JobID: "a", WorkerID: "w1", Error: "timeout", Retryable: true, Now: t0})
	if err != nil || got.Status != JobQueued || got.Error != "timeout" {
		t.Fatalf("retryable fail: %+v %v", got, err)
	}
	lease()
	got, err = s.FailJob(ctx, FailRequest{JobID: "a", WorkerID: "w1", Error: "timeout", Retryable: true, Now: t0})
	if err != nil || got.Status != JobFailed || got.FinishedAt == nil {
		t.Fatalf("final fail: %+v %v", got, err)
	}

	mustCreate(t, s, newJob("b", "p1"))
	lease()
	got, err = s.FailJob(ctx, FailRequest{JobID: "b", WorkerID: "w1", Error: "bad pdf", Now: t0})
	if err != nil || got.Status != JobFailed {
		t.Fatalf("non-retryable fail: %+v %v", got, err)
	}
}

func TestCancelAndCleanup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	mustCreate(t, s, newJob("a", "p1"))
	mustCreate(t, s, newJob("b", "p1"))
	if _, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelJob(ctx, "a", t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel leased job: %v", err)
	}
	if j, err := s.CancelJob(ctx, "b", t0); err != nil || j.Status != JobCanceled {
		t.Fatalf("cancel: %+v %v", j, err)
	}
	ids, err := s.DeleteFinishedJobs(ctx, t0.Add(time.Second))
	if err != nil || len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("cleanup: %v %v", ids, err)
	}
	if ok, _ := s.JobExists(ctx, "b"); ok {
		t.Fatal("job not deleted")
	}
	if err := s.DeletePrinter(ctx, "p1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete printer with active job: %v", err)
	}
	jobs, err := s.ListJobs(ctx, JobFilter{PrinterID: "p1"})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list: %v %v", jobs, err)
	}
}

func TestPanelUsers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	first := &PanelUser{IP: "100.64.0.1", PeerID: "key-a", Role: PanelAdmin, CreatedAt: t0, UpdatedAt: t0}
	if err := s.ClaimFirstPanelAdmin(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &PanelUser{IP: "100.64.0.2", PeerID: "key-b", Role: PanelAdmin, CreatedAt: t0, UpdatedAt: t0}
	if err := s.ClaimFirstPanelAdmin(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("second claim: %v", err)
	}
	viewer := &PanelUser{IP: "100.64.0.3", Role: PanelViewer, Name: "Ops", CreatedAt: t0, UpdatedAt: t0}
	if err := s.CreatePanelUser(ctx, viewer); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePanelUser(ctx, viewer); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	// The only admin can neither be demoted nor deleted.
	demoted := *first
	demoted.Role = PanelViewer
	if err := s.UpdatePanelUser(ctx, &demoted); !errors.Is(err, ErrConflict) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := s.DeletePanelUser(ctx, first.IP); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete last admin: %v", err)
	}
	got, _ := s.GetPanelUser(ctx, first.IP)
	if got.Role != PanelAdmin {
		t.Fatal("rolled back change was applied")
	}
	viewer.Role, viewer.PeerID = PanelAdmin, "key-c"
	if err := s.UpdatePanelUser(ctx, viewer); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePanelUser(ctx, first.IP); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchPanelUser(ctx, viewer.IP, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPanelUsers(ctx)
	if err != nil || len(list) != 1 || list[0].PeerID != "key-c" || list[0].LastSeen == nil || list[0].Name != "Ops" {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestRetryAndDeleteJob(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "w1", "p1")
	j := newJob("a", "p1")
	j.MaxAttempts = 1
	mustCreate(t, s, j)
	if _, err := s.RetryJob(ctx, "a", t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry queued job: %v", err)
	}
	if _, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteJob(ctx, "a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete leased job: %v", err)
	}
	if _, err := s.FailJob(ctx, FailRequest{JobID: "a", WorkerID: "w1", Error: "paper jam", Now: t0}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RetryJob(ctx, "a", t0.Add(time.Minute))
	if err != nil || got.Status != JobQueued || got.Attempts != 0 || got.Error != "" || got.FinishedAt != nil {
		t.Fatalf("retry: %+v %v", got, err)
	}
	// The retried job can be leased again.
	if l, err := s.LeaseJob(ctx, LeaseRequest{WorkerID: "w1", Now: t0, Duration: time.Minute}); err != nil || l == nil || l.ID != "a" || l.Attempts != 1 {
		t.Fatalf("lease after retry: %+v %v", l, err)
	}
	if _, err := s.CompleteJob(ctx, "a", "w1", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryJob(ctx, "a", t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry done job: %v", err)
	}
	if err := s.DeleteJob(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteJob(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestSettings(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.GetSetting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	for _, v := range []string{"a", "b"} {
		if err := s.SetSetting(ctx, "k", v, t0); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := s.GetSetting(ctx, "k"); err != nil || v != "b" {
		t.Fatalf("get: %q %v", v, err)
	}
	if err := s.DeleteSetting(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSetting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}
