// Package server implements the platen management server: the application,
// worker and admin HTTP APIs, authentication, lease handling and cleanup.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/spool"
	"github.com/tim-riep/go-platen/internal/store"
)

// Peer is the mesh identity of the remote end of a connection.
type Peer struct {
	// ID is the NetBird peer identity (WireGuard public key).
	ID   string
	FQDN string
}

// PeerResolver maps the remote address of an incoming connection to the
// mesh peer it belongs to.
type PeerResolver interface {
	PeerForAddr(remoteAddr string) (Peer, bool)
}

// ContentTypeFunc reports the content types a backend accepts. It returns
// false if the backend is unknown.
type ContentTypeFunc func(backend string) (contentTypes []string, ok bool)

// Options configures a Server.
type Options struct {
	Store  store.Store
	Spool  *spool.Spool
	Peers  PeerResolver
	Logger *slog.Logger
	// AdminTokenHash is the auth.Hash of the admin token. Empty disables
	// the admin API.
	AdminTokenHash string
	// Backends reports the content types per backend. If nil, backends
	// and content types are not validated.
	Backends ContentTypeFunc
	// LocalWorker enables the built-in worker; its name is LocalWorkerName.
	LocalWorker     bool
	LocalWorkerName string

	MaxDocumentSize int64
	LeaseDuration   time.Duration
	MaxLeaseWait    time.Duration
	MaxAttempts     int
	JobRetention    time.Duration
	CleanupInterval time.Duration
	ExpiryInterval  time.Duration
	EnrollmentTTL   time.Duration
	Version         string

	// Panel enables the web panel on the server's port. PanelHosts returns
	// the host names and addresses the panel may be opened by (the
	// server's NetBird name and IP); requests for other hosts are refused.
	Panel      bool
	PanelHosts func() []string
	// BackendNames lists the available backends for the panel.
	BackendNames []string

	// Now returns the current time; tests may override it.
	Now func() time.Time
}

func (o *Options) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxDocumentSize <= 0 {
		o.MaxDocumentSize = 64 << 20
	}
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = 60 * time.Second
	}
	if o.MaxLeaseWait <= 0 {
		o.MaxLeaseWait = 30 * time.Second
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.JobRetention <= 0 {
		o.JobRetention = 7 * 24 * time.Hour
	}
	if o.CleanupInterval <= 0 {
		o.CleanupInterval = 10 * time.Minute
	}
	if o.ExpiryInterval <= 0 {
		o.ExpiryInterval = 5 * time.Second
	}
	if o.EnrollmentTTL <= 0 {
		o.EnrollmentTTL = 24 * time.Hour
	}
	if o.LocalWorkerName == "" {
		o.LocalWorkerName = "built-in worker"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Server is the platen management server.
type Server struct {
	opts   Options
	store  store.Store
	spool  *spool.Spool
	log    *slog.Logger
	notify *notifier
	// Workers is the worker side service, shared by the HTTP worker API and
	// the in-process transport of the built-in worker.
	Workers *WorkerService
}

// New creates a server. It does not start background loops; call Run.
func New(opts Options) (*Server, error) {
	if opts.Store == nil || opts.Spool == nil {
		return nil, errors.New("server: store and spool are required")
	}
	if opts.Peers == nil {
		return nil, errors.New("server: peer resolver is required")
	}
	if opts.AdminTokenHash != "" && !auth.ValidHash(opts.AdminTokenHash) {
		return nil, errors.New("server: admin token hash must have the form sha256:<hex>")
	}
	opts.setDefaults()
	s := &Server{
		opts:   opts,
		store:  opts.Store,
		spool:  opts.Spool,
		log:    opts.Logger,
		notify: newNotifier(),
	}
	s.Workers = &WorkerService{s: s}
	return s, nil
}

// Init prepares the database, e.g. the record of the built-in worker.
func (s *Server) Init(ctx context.Context) error {
	if !s.opts.LocalWorker {
		return nil
	}
	now := s.opts.Now()
	w, err := s.store.GetWorker(ctx, store.LocalWorkerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		err = s.store.CreateWorker(ctx, &store.Worker{
			ID: store.LocalWorkerID, Name: s.opts.LocalWorkerName, Location: "server", CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("create built-in worker: %w", err)
		}
	case err != nil:
		return err
	case w.Name != s.opts.LocalWorkerName:
		w.Name, w.UpdatedAt = s.opts.LocalWorkerName, now
		if err := s.store.UpdateWorker(ctx, w); err != nil {
			return fmt.Errorf("update built-in worker: %w", err)
		}
	}
	return nil
}

// Run runs the background loops (lease expiry, cleanup) until ctx is done.
func (s *Server) Run(ctx context.Context) {
	expiry := time.NewTicker(s.opts.ExpiryInterval)
	defer expiry.Stop()
	cleanup := time.NewTicker(s.opts.CleanupInterval)
	defer cleanup.Stop()
	s.Cleanup(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			s.ExpireLeases(ctx)
		case <-cleanup.C:
			s.Cleanup(ctx)
		}
	}
}

// ExpireLeases returns jobs with expired leases to their queue.
func (s *Server) ExpireLeases(ctx context.Context) {
	jobs, err := s.store.ExpireLeases(ctx, s.opts.Now())
	if err != nil {
		s.log.Error("expire leases", "err", err)
		return
	}
	for _, j := range jobs {
		s.log.Warn("lease expired", "job", j.ID, "worker", j.LeaseHolder, "status", j.Status, "attempts", j.Attempts)
	}
	if len(jobs) > 0 {
		s.notify.broadcast()
	}
}

// orphanAge is how old a spool file without job must be before it is
// removed; younger files may belong to an upload in progress.
const orphanAge = time.Hour

// Cleanup deletes finished jobs older than the retention period together
// with their documents, and removes orphaned spool files.
func (s *Server) Cleanup(ctx context.Context) {
	now := s.opts.Now()
	ids, err := s.store.DeleteFinishedJobs(ctx, now.Add(-s.opts.JobRetention))
	if err != nil {
		s.log.Error("cleanup jobs", "err", err)
		return
	}
	for _, id := range ids {
		if err := s.spool.Remove(id); err != nil {
			s.log.Error("cleanup document", "job", id, "err", err)
		}
	}
	if len(ids) > 0 {
		s.log.Info("cleaned up finished jobs", "count", len(ids))
	}
	entries, err := s.spool.List()
	if err != nil {
		s.log.Error("list spool", "err", err)
		return
	}
	for _, e := range entries {
		if now.Sub(e.ModTime) < orphanAge {
			continue
		}
		if strings.HasPrefix(e.ID, ".incoming-") {
			_ = s.spool.RemoveIncoming(e.ID)
			continue
		}
		exists, err := s.store.JobExists(ctx, e.ID)
		if err != nil {
			s.log.Error("cleanup orphan check", "err", err)
			return
		}
		if !exists {
			s.log.Info("removing orphaned document", "id", e.ID)
			_ = s.spool.Remove(e.ID)
		}
	}
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)

	s.appRoutes(mux)
	s.workerRoutes(mux)
	s.adminRoutes(mux)
	if s.opts.Panel {
		s.panelRoutes(mux)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, errNotFound("no such endpoint"))
	})
	return s.recoverer(mux)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeError(w, errInternal())
			}
		}()
		next.ServeHTTP(w, r)
	})
}
