// Package app wires the components together for the "server" and "worker"
// modes. The network is passed in as a mesh.Network: the binary always
// passes the NetBird client, tests pass a loopback implementation.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/backend"
	"github.com/tim-riep/go-platen/internal/backend/raster"
	"github.com/tim-riep/go-platen/internal/backend/raw"
	"github.com/tim-riep/go-platen/internal/config"
	"github.com/tim-riep/go-platen/internal/mesh"
	"github.com/tim-riep/go-platen/internal/server"
	"github.com/tim-riep/go-platen/internal/spool"
	"github.com/tim-riep/go-platen/internal/store"
	"github.com/tim-riep/go-platen/internal/worker"
)

// EnvEnrollmentToken overrides the enrollment token file of a worker.
const EnvEnrollmentToken = "PLATEN_ENROLLMENT_TOKEN"

// Backends returns the registry with all backends compiled into platen.
func Backends() *backend.Registry {
	return backend.NewRegistry(raw.Factory, raster.PDFToRaster)
}

type peerResolver struct{ n mesh.Network }

func (p peerResolver) PeerForAddr(addr string) (server.Peer, bool) {
	id, fqdn, ok := p.n.PeerForAddr(addr)
	return server.Peer{ID: id, FQDN: fqdn}, ok
}

// RunServer runs the management server until ctx is canceled. It listens
// only on the mesh network n.
func RunServer(ctx context.Context, cfg *config.Server, n mesh.Network, log *slog.Logger, version string) error {
	st, err := store.OpenSQLite(ctx, cfg.Server.Database)
	if err != nil {
		return err
	}
	defer st.Close()
	sp, err := spool.Open(cfg.Server.SpoolDir)
	if err != nil {
		return err
	}
	backends := Backends()
	defer backends.Close()

	srv, err := server.New(server.Options{
		Store: st, Spool: sp, Peers: peerResolver{n}, Logger: log, AdminTokenHash: cfg.Server.AdminTokenHash,
		Backends: backends.ContentTypes, LocalWorker: cfg.LocalWorker.Enabled, LocalWorkerName: cfg.LocalWorker.Name,
		MaxDocumentSize: cfg.Server.MaxDocumentSize, LeaseDuration: cfg.Server.LeaseDuration.Duration,
		MaxAttempts: cfg.Server.MaxAttempts, JobRetention: cfg.Server.JobRetention.Duration,
		CleanupInterval: cfg.Server.CleanupInterval.Duration, EnrollmentTTL: cfg.Server.EnrollmentTTL.Duration,
		Version: version,
	})
	if err != nil {
		return err
	}
	if err := srv.Init(ctx); err != nil {
		return err
	}
	if cfg.Server.AdminTokenHash == "" {
		log.Warn("no admin_token_hash configured; the admin API is disabled")
	}

	ln, err := n.Listen(cfg.NetBird.Port)
	if err != nil {
		return fmt.Errorf("listen in mesh: %w", err)
	}
	// Requests get a context that is canceled at shutdown, so long polling
	// lease requests return immediately.
	reqCtx, cancelReqs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelReqs()
	hs := &http.Server{
		Handler:           srv.Handler(),
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	bgCtx, cancelBg := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBg()
	bgDone := make(chan struct{})
	go func() {
		srv.Run(bgCtx)
		close(bgDone)
	}()

	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan error, 1)
	if cfg.LocalWorker.Enabled {
		j, err := worker.OpenJournal(cfg.LocalWorker.JournalDir)
		if err != nil {
			return err
		}
		w, err := worker.New(worker.Options{
			Transport: &worker.InProcess{Service: srv.Workers, WorkerID: store.LocalWorkerID},
			Journal:   j, Backends: backends, Logger: log.With("worker", store.LocalWorkerID),
			Version: version, TempDir: cfg.LocalWorker.TempDir,
		})
		if err != nil {
			return err
		}
		go func() { workerDone <- w.Run(workerCtx) }()
		log.Info("built-in worker started", "name", cfg.LocalWorker.Name)
	} else {
		workerDone <- nil
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	log.Info("server listening in the mesh", "addr", ln.Addr().String(), "version", version)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		runErr = fmt.Errorf("http server: %w", err)
	}

	log.Info("shutting down")
	cancelReqs()
	shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := hs.Shutdown(shCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("http shutdown", "err", err)
	}
	stopWorker()
	if err := <-workerDone; err != nil {
		log.Warn("built-in worker", "err", err)
	}
	cancelBg()
	<-bgDone
	return runErr
}

// RunWorker runs a standalone worker until ctx is canceled. It reaches the
// server only through the mesh network n and the printers through the
// local network.
func RunWorker(ctx context.Context, cfg *config.Worker, n mesh.Network, log *slog.Logger, version string) error {
	client := n.HTTPClient()
	cred, err := credential(ctx, cfg, client, log, version)
	if err != nil {
		return err
	}
	j, err := worker.OpenJournal(cfg.Worker.JournalDir)
	if err != nil {
		return err
	}
	backends := Backends()
	defer backends.Close()
	w, err := worker.New(worker.Options{
		Transport: &worker.HTTPTransport{BaseURL: cfg.Worker.ServerURL, Client: client, Credential: cred},
		Journal:   j, Backends: backends, Logger: log, Version: version, TempDir: cfg.Worker.TempDir,
		Dialer: &net.Dialer{},
	})
	if err != nil {
		return err
	}
	log.Info("worker started", "server", cfg.Worker.ServerURL, "version", version)
	return w.Run(ctx)
}

// credential returns the stored worker credential, enrolling the worker
// first if there is none yet.
func credential(ctx context.Context, cfg *config.Worker, client *http.Client, log *slog.Logger, version string) (string, error) {
	b, err := os.ReadFile(cfg.Worker.CredentialFile)
	if err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read credential: %w", err)
	}

	token := strings.TrimSpace(os.Getenv(EnvEnrollmentToken))
	if token == "" && cfg.Worker.EnrollmentTokenFile != "" {
		b, err := os.ReadFile(cfg.Worker.EnrollmentTokenFile)
		if err != nil {
			return "", fmt.Errorf("not enrolled yet and cannot read the enrollment token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return "", errors.New("not enrolled yet: provide an enrollment token (worker.enrollment_token_file or " + EnvEnrollmentToken + ")")
	}

	delay := time.Second
	for {
		resp, err := worker.Register(ctx, client, cfg.Worker.ServerURL, token, version)
		if err == nil {
			if err := writeSecret(cfg.Worker.CredentialFile, resp.Credential); err != nil {
				return "", fmt.Errorf("store credential: %w", err)
			}
			log.Info("worker enrolled; the enrollment token is used up and can be deleted", "worker", resp.WorkerID)
			return resp.Credential, nil
		}
		if api.IsPermanent(err) {
			return "", fmt.Errorf("enrollment rejected: %w", err)
		}
		log.Warn("enrollment failed, retrying", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
}

// writeSecret writes a file readable only by the owner, atomically.
func writeSecret(path, secret string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(secret + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
