// Package worker implements the print worker: it leases jobs from the
// server, renders them with a backend and sends them to the printers. The
// same code runs in remote workers (HTTP transport) and in the worker
// built into the server (in-process transport).
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/backend"
	"github.com/tim-riep/go-platen/internal/printer"
)

// Options configures a Worker.
type Options struct {
	Transport Transport
	Journal   *Journal
	Backends  *backend.Registry
	// Dialer connects to printers in the local network. Default net.Dialer.
	Dialer  printer.Dialer
	Printer printer.Options
	Logger  *slog.Logger
	Version string
	// TempDir holds downloaded documents and rendered output while a job
	// is processed. Default: a directory below os.TempDir().
	TempDir string

	LeaseWait         time.Duration
	HeartbeatInterval time.Duration
	ProbeTimeout      time.Duration
	MinBackoff        time.Duration
	MaxBackoff        time.Duration
	// ShutdownTimeout bounds how long Run waits for prints in progress
	// after its context was canceled.
	ShutdownTimeout time.Duration
}

func (o *Options) setDefaults() {
	if o.Dialer == nil {
		o.Dialer = &net.Dialer{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.TempDir == "" {
		o.TempDir = os.TempDir()
	}
	if o.LeaseWait <= 0 {
		o.LeaseWait = 25 * time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 30 * time.Second
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 3 * time.Second
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Minute
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 5 * time.Minute
	}
}

// Worker processes print jobs.
type Worker struct {
	opts Options
	t    Transport
	j    *Journal
	log  *slog.Logger

	mu           sync.Mutex
	printers     map[string]api.PrinterConfig
	busy         map[string]bool
	leaseSeconds int

	idleWake   chan struct{}
	reportWake chan struct{}
}

// New creates a worker.
func New(opts Options) (*Worker, error) {
	if opts.Transport == nil || opts.Journal == nil || opts.Backends == nil {
		return nil, errors.New("worker: transport, journal and backends are required")
	}
	opts.setDefaults()
	if err := os.MkdirAll(opts.TempDir, 0o700); err != nil {
		return nil, fmt.Errorf("worker: temp dir: %w", err)
	}
	return &Worker{
		opts: opts, t: opts.Transport, j: opts.Journal, log: opts.Logger,
		printers: map[string]api.PrinterConfig{}, busy: map[string]bool{}, leaseSeconds: 60,
		idleWake: make(chan struct{}, 1), reportWake: make(chan struct{}, 1),
	}, nil
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run processes jobs until ctx is canceled. Prints in progress are then
// completed (up to ShutdownTimeout) and outstanding reports are sent if the
// server is reachable; reports that cannot be sent stay in the journal and
// are sent after the next start.
func (w *Worker) Run(ctx context.Context) error {
	// Processing and reporting outlive ctx so that a print in progress is
	// completed on shutdown.
	procCtx, procCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer procCancel()

	var bg sync.WaitGroup
	signal(w.reportWake)
	bg.Go(func() { w.reporter(procCtx) })

	if err := w.waitForConfig(ctx); err != nil {
		w.shutdown(procCancel, &bg, nil)
		return nil
	}
	bg.Go(func() { w.heartbeatLoop(ctx) })

	var jobs sync.WaitGroup
	w.leaseLoop(ctx, procCtx, &jobs)
	w.shutdown(procCancel, &bg, &jobs)
	return nil
}

func (w *Worker) shutdown(procCancel context.CancelFunc, bg, jobs *sync.WaitGroup) {
	if jobs != nil {
		done := make(chan struct{})
		go func() { jobs.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(w.opts.ShutdownTimeout):
			w.log.Error("prints still in progress at shutdown timeout; aborting them")
		}
	}
	// Give the reporter a moment to deliver the last reports.
	flush, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.reportPending(flush)
	procCancel()
	bg.Wait()
}

func (w *Worker) waitForConfig(ctx context.Context) error {
	bo := newBackoff(w.opts.MinBackoff, w.opts.MaxBackoff)
	for {
		err := w.refreshConfig(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d := bo.next()
		w.log.Warn("cannot fetch configuration from server", "err", err, "retry_in", d)
		if !sleep(ctx, d) {
			return ctx.Err()
		}
	}
}

func (w *Worker) refreshConfig(ctx context.Context) error {
	cfg, err := w.t.Config(ctx)
	if err != nil {
		return err
	}
	w.mu.Lock()
	changed := len(cfg.Printers) != len(w.printers)
	next := make(map[string]api.PrinterConfig, len(cfg.Printers))
	for _, p := range cfg.Printers {
		if old, ok := w.printers[p.ID]; !ok || old.Address != p.Address {
			changed = true
		}
		next[p.ID] = p
	}
	w.printers = next
	if cfg.LeaseSeconds > 0 {
		w.leaseSeconds = cfg.LeaseSeconds
	}
	w.mu.Unlock()
	if changed {
		w.log.Info("configuration updated", "worker", cfg.WorkerID, "printers", len(cfg.Printers))
		signal(w.idleWake)
	}
	return nil
}

func (w *Worker) idlePrinters() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var ids []string
	for id := range w.printers {
		if !w.busy[id] {
			ids = append(ids, id)
		}
	}
	return ids
}

func (w *Worker) setBusy(id string, busy bool) {
	w.mu.Lock()
	if busy {
		w.busy[id] = true
	} else {
		delete(w.busy, id)
	}
	w.mu.Unlock()
	if !busy {
		signal(w.idleWake)
	}
}

func (w *Worker) leaseLoop(ctx, procCtx context.Context, jobs *sync.WaitGroup) {
	bo := newBackoff(w.opts.MinBackoff, w.opts.MaxBackoff)
	unreachable := false
	for ctx.Err() == nil {
		idle := w.idlePrinters()
		if len(idle) == 0 {
			// Every printer is busy (or none is configured): wait until
			// one becomes idle or the configuration changes.
			select {
			case <-w.idleWake:
			case <-ctx.Done():
			}
			continue
		}
		lease, err := w.t.Lease(ctx, idle, w.opts.LeaseWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d := bo.next()
			if !unreachable {
				w.log.Warn("lease request failed, retrying with backoff", "err", err)
			} else {
				w.log.Debug("lease request failed", "err", err, "retry_in", d)
			}
			unreachable = true
			sleep(ctx, d)
			continue
		}
		if unreachable {
			w.log.Info("server reachable again")
			unreachable = false
		}
		bo.reset()
		if lease == nil {
			continue
		}
		pid := lease.Printer.ID
		w.setBusy(pid, true)
		jobs.Go(func() {
			defer w.setBusy(pid, false)
			w.process(procCtx, lease)
		})
	}
}

// process prints one leased job. The outcome is written to the journal and
// reported by the reporter, so it survives restarts and server outages.
func (w *Worker) process(ctx context.Context, l *api.Lease) {
	job, p := l.Job, l.Printer
	log := w.log.With("job", job.ID, "printer", p.ID, "attempt", job.Attempt)

	// A job we already handled comes back if our report was not received
	// before the lease expired. Never print it again.
	if e, err := w.j.Get(job.ID); err != nil {
		log.Error("read journal", "err", err)
		return
	} else if e != nil {
		log.Warn("job already handled by this worker, re-sending its report instead of printing", "state", e.State)
		if e.State == StateSent {
			e.State, e.Uncertain = StateUncertain, true
			e.Error = "the job was leased again while it was being sent"
			_ = w.j.Put(*e)
		}
		signal(w.reportWake)
		return
	}

	fail := func(retryable bool, err error) {
		log.Warn("job failed", "retryable", retryable, "err", err)
		w.record(Entry{JobID: job.ID, PrinterID: p.ID, State: StateFailed, Error: err.Error(), Retryable: retryable})
	}

	b, err := w.opts.Backends.Get(p.Backend)
	if err != nil {
		fail(false, err)
		return
	}
	if !b.Accepts(job.ContentType) {
		fail(false, fmt.Errorf("backend %s does not accept %s", p.Backend, job.ContentType))
		return
	}

	// Mark the job as printing and keep the lease alive while working.
	if _, err := w.t.Progress(ctx, job.ID); err != nil && api.IsPermanent(err) {
		log.Warn("lease lost before printing, dropping job", "err", err)
		return
	}
	keepCtx, stopKeep := context.WithCancel(ctx)
	defer stopKeep()
	var lost atomic.Bool
	go w.keepalive(keepCtx, job.ID, &lost, log)

	doc, err := w.download(ctx, job)
	if err != nil {
		fail(true, fmt.Errorf("download document: %w", err))
		return
	}
	defer os.Remove(doc.Name())
	defer doc.Close()

	out, err := os.CreateTemp(w.opts.TempDir, "render-*")
	if err != nil {
		fail(true, err)
		return
	}
	defer os.Remove(out.Name())
	defer out.Close()
	opts := backend.JobOptions{
		JobID: job.ID, Title: job.Title, ContentType: job.ContentType, Copies: job.Copies,
		PaperSize: p.PaperSize, ResolutionDPI: p.ResolutionDPI, Options: p.Options,
		Address: p.Address, Dialer: w.opts.Dialer,
	}
	start := time.Now()
	if err := b.Print(ctx, doc, opts, out); err != nil {
		fail(backend.IsRetryable(err), fmt.Errorf("render: %w", err))
		return
	}
	size, err := out.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = out.Seek(0, io.SeekStart)
	}
	if err != nil {
		fail(true, err)
		return
	}
	log.Debug("rendered", "backend", p.Backend, "bytes", size, "took", time.Since(start))

	if lost.Load() {
		log.Warn("lease lost before sending, dropping job")
		return
	}
	// From here on the job may reach the printer. Record that first, so a
	// crash during sending is detected after the restart.
	if err := w.j.Put(Entry{JobID: job.ID, PrinterID: p.ID, State: StateSent}); err != nil {
		fail(true, fmt.Errorf("write journal: %w", err))
		return
	}
	var n int64
	if d, ok := b.(backend.Deliverer); ok {
		err, n = d.Deliver(ctx, out, size, opts), size
	} else {
		n, err = printer.Send(ctx, w.opts.Dialer, p.Address, out, w.opts.Printer)
	}
	stopKeep()
	var refused *printer.RefusedError
	switch {
	case err == nil:
		log.Info("job printed", "bytes", n, "took", time.Since(start))
		w.record(Entry{JobID: job.ID, PrinterID: p.ID, State: StatePrinted})
	case printer.IsSafeToRetry(err):
		fail(true, err)
	case errors.As(err, &refused):
		fail(refused.Retryable, err)
	default:
		log.Error("sending interrupted, the printer may have printed part of the job", "err", err)
		w.record(Entry{JobID: job.ID, PrinterID: p.ID, State: StateFailed, Error: err.Error(), Uncertain: true})
	}
}

// record journals the outcome of a job and wakes up the reporter.
func (w *Worker) record(e Entry) {
	if err := w.j.Put(e); err != nil {
		w.log.Error("write journal", "job", e.JobID, "err", err)
	}
	signal(w.reportWake)
}

func (w *Worker) keepalive(ctx context.Context, jobID string, lost *atomic.Bool, log *slog.Logger) {
	w.mu.Lock()
	interval := time.Duration(w.leaseSeconds) * time.Second / 3
	w.mu.Unlock()
	t := time.NewTicker(max(interval, time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.t.Progress(ctx, jobID); err != nil {
				if api.IsPermanent(err) {
					lost.Store(true)
					log.Warn("lease lost", "err", err)
					return
				}
				log.Debug("progress report failed", "err", err)
			}
		}
	}
}

// download stores the document in a temporary file and verifies it.
func (w *Worker) download(ctx context.Context, job api.LeasedJob) (*os.File, error) {
	rc, err := w.t.Document(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	f, err := os.CreateTemp(w.opts.TempDir, "doc-*")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), rc)
	if err == nil && n != job.Size {
		err = fmt.Errorf("got %d bytes, want %d", n, job.Size)
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != job.DocumentHash {
		err = errors.New("document hash mismatch")
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// reporter sends pending reports from the journal until ctx is canceled.
func (w *Worker) reporter(ctx context.Context) {
	bo := newBackoff(w.opts.MinBackoff, w.opts.MaxBackoff)
	for {
		var retry <-chan time.Time
		if w.reportPending(ctx) {
			bo.reset()
		} else {
			retry = time.After(bo.next())
		}
		select {
		case <-ctx.Done():
			return
		case <-w.reportWake:
		case <-retry:
		}
	}
}

// reportPending sends all pending reports. It returns false if a report
// failed for a transient reason.
func (w *Worker) reportPending(ctx context.Context) bool {
	entries, err := w.j.List()
	if err != nil {
		w.log.Error("read journal", "err", err)
		return false
	}
	for _, e := range entries {
		if !e.pendingReport() {
			continue
		}
		err := w.report(ctx, e)
		if err != nil && !reportSettled(err) {
			if ctx.Err() == nil {
				w.log.Warn("report failed, will retry", "job", e.JobID, "err", err)
			}
			return false
		}
		if err != nil {
			w.log.Warn("server rejected report, discarding it", "job", e.JobID, "state", e.State, "err", err)
		}
		if err := w.j.Remove(e.JobID); err != nil {
			w.log.Error("remove journal entry", "job", e.JobID, "err", err)
		}
	}
	return true
}

// reportSettled reports whether retrying a rejected report is pointless,
// e.g. because the job is already final or was deleted. Authentication
// errors are not settled: the report is kept until it can be delivered.
func reportSettled(err error) bool {
	var ae *api.Error
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

func (w *Worker) report(ctx context.Context, e Entry) error {
	switch e.State {
	case StatePrinted:
		return w.t.Complete(ctx, e.JobID)
	case StateUncertain:
		return w.t.Fail(ctx, e.JobID, api.FailRequest{Error: e.Error, Uncertain: true})
	default:
		return w.t.Fail(ctx, e.JobID, api.FailRequest{Error: e.Error, Retryable: e.Retryable, Uncertain: e.Uncertain})
	}
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(w.opts.HeartbeatInterval)
	defer t.Stop()
	w.heartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.refreshConfig(ctx); err != nil && ctx.Err() == nil {
				w.log.Debug("refresh configuration", "err", err)
			}
			w.heartbeat(ctx)
		}
	}
}

func (w *Worker) heartbeat(ctx context.Context) {
	w.mu.Lock()
	printers := make([]api.PrinterConfig, 0, len(w.printers))
	busy := map[string]bool{}
	for id, p := range w.printers {
		printers = append(printers, p)
		busy[id] = w.busy[id]
	}
	w.mu.Unlock()

	reports := make([]api.Reachability, len(printers))
	var wg sync.WaitGroup
	for i, p := range printers {
		reports[i] = api.Reachability{PrinterID: p.ID, Reachable: true}
		if busy[p.ID] {
			// Do not open a second connection to a printer we are
			// currently sending to.
			continue
		}
		probe := printer.Probe
		if b, err := w.opts.Backends.Get(p.Backend); err == nil {
			if d, ok := b.(backend.Deliverer); ok {
				probe = func(ctx context.Context, dl printer.Dialer, addr string, timeout time.Duration) error {
					return d.Probe(ctx, dl, addr, timeout)
				}
			}
		}
		wg.Go(func() {
			if err := probe(ctx, w.opts.Dialer, p.Address, w.opts.ProbeTimeout); err != nil {
				reports[i] = api.Reachability{PrinterID: p.ID, Reachable: false, Error: err.Error()}
			}
		})
	}
	wg.Wait()
	if err := w.t.Heartbeat(ctx, api.HeartbeatRequest{Version: w.opts.Version, Printers: reports}); err != nil && ctx.Err() == nil {
		w.log.Debug("heartbeat failed", "err", err)
	}
}

// ---- helpers ----

type backoff struct {
	min, max, cur time.Duration
}

func newBackoff(min, max time.Duration) *backoff {
	return &backoff{min: min, max: max}
}

// next returns the next delay: exponential growth with jitter.
func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur = min(b.cur*2, b.max)
	}
	jitter := time.Duration(rand.Int64N(int64(b.cur)/4 + 1))
	return b.cur - b.cur/8 + jitter
}

func (b *backoff) reset() { b.cur = 0 }

// sleep waits for d or until ctx is done; it reports whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
