package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/store"
)

// WorkerService implements the worker side operations. The HTTP worker API
// calls it after authenticating the worker; the built-in worker calls it
// directly through its in-process transport. Both share this code path.
//
// Errors are returned as *api.Error where the cause is known, so both
// transports report them identically.
type WorkerService struct {
	s *Server
}

func toAPI(err error) error {
	if ae := apiError(err); ae != nil {
		return ae
	}
	return err
}

func printerConfig(p *store.Printer) api.PrinterConfig {
	return api.PrinterConfig{
		ID: p.ID, Name: p.Name, Address: p.Address, Backend: p.Backend, PaperSize: p.PaperSize,
		ResolutionDPI: p.ResolutionDPI, Options: p.Options, Status: string(p.Status),
	}
}

// Config returns the worker's own printers.
func (ws *WorkerService) Config(ctx context.Context, workerID string) (*api.WorkerConfig, error) {
	w, err := ws.s.store.GetWorker(ctx, workerID)
	if err != nil {
		return nil, toAPI(err)
	}
	printers, err := ws.s.store.ListPrinters(ctx, store.PrinterFilter{WorkerID: workerID})
	if err != nil {
		return nil, toAPI(err)
	}
	out := &api.WorkerConfig{
		WorkerID: w.ID, Name: w.Name, Printers: make([]api.PrinterConfig, 0, len(printers)),
		LeaseSeconds: int(ws.s.opts.LeaseDuration / time.Second),
	}
	for i := range printers {
		out.Printers = append(out.Printers, printerConfig(&printers[i]))
	}
	return out, nil
}

// Lease returns the next job for the worker, waiting up to wait (capped by
// the server) for one to become available. It returns nil if none did.
func (ws *WorkerService) Lease(ctx context.Context, workerID string, printers []string, wait time.Duration) (*api.Lease, error) {
	if wait < 0 {
		wait = 0
	}
	if wait > ws.s.opts.MaxLeaseWait {
		wait = ws.s.opts.MaxLeaseWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		woken := ws.s.notify.wait()
		j, err := ws.s.store.LeaseJob(ctx, store.LeaseRequest{
			WorkerID: workerID, PrinterIDs: printers, Now: ws.s.opts.Now(), Duration: ws.s.opts.LeaseDuration,
		})
		if err != nil {
			return nil, toAPI(err)
		}
		if j != nil {
			p, err := ws.s.store.GetPrinter(ctx, j.PrinterID)
			if err != nil {
				return nil, toAPI(err)
			}
			ws.s.log.Info("job leased", "job", j.ID, "worker", workerID, "printer", j.PrinterID, "attempt", j.Attempts)
			return &api.Lease{
				Job: api.LeasedJob{
					ID: j.ID, Copies: j.Copies, Title: j.Title, ContentType: j.ContentType, Size: j.Size,
					DocumentHash: j.DocumentHash, Attempt: j.Attempts,
				},
				Printer:   printerConfig(p),
				ExpiresAt: *j.LeaseExpiresAt,
			}, nil
		}
		select {
		case <-woken:
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	}
}

// Document opens the document of a job leased to the worker.
func (ws *WorkerService) Document(ctx context.Context, workerID, jobID string) (io.ReadCloser, *store.Job, error) {
	j, err := ws.s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, toAPI(err)
	}
	if j.LeaseHolder != workerID || (j.Status != store.JobLeased && j.Status != store.JobPrinting) {
		return nil, nil, api.Errorf(http.StatusConflict, api.CodeConflict, "job is not leased to this worker")
	}
	f, err := ws.s.spool.Open(jobID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, api.Errorf(http.StatusGone, api.CodeNotFound, "document of job %s is missing", jobID)
	}
	if err != nil {
		return nil, nil, err
	}
	return f, j, nil
}

// OpenDocument is Document without the job metadata.
func (ws *WorkerService) OpenDocument(ctx context.Context, workerID, jobID string) (io.ReadCloser, error) {
	rc, _, err := ws.Document(ctx, workerID, jobID)
	return rc, err
}

// Progress extends the lease and marks the job as printing.
func (ws *WorkerService) Progress(ctx context.Context, workerID, jobID string) (*api.ProgressResponse, error) {
	now := ws.s.opts.Now()
	j, err := ws.s.store.ExtendLease(ctx, jobID, workerID, now, now.Add(ws.s.opts.LeaseDuration))
	if err != nil {
		return nil, toAPI(err)
	}
	return &api.ProgressResponse{ExpiresAt: *j.LeaseExpiresAt}, nil
}

// Complete marks the job as done.
func (ws *WorkerService) Complete(ctx context.Context, workerID, jobID string) error {
	j, err := ws.s.store.CompleteJob(ctx, jobID, workerID, ws.s.opts.Now())
	if err != nil {
		return toAPI(err)
	}
	ws.s.log.Info("job done", "job", j.ID, "worker", workerID, "printer", j.PrinterID)
	ws.s.notify.broadcast()
	return nil
}

// Fail records a failed print attempt.
func (ws *WorkerService) Fail(ctx context.Context, workerID, jobID string, req api.FailRequest) error {
	msg := req.Error
	if msg == "" {
		msg = "unknown error"
	}
	retryable := req.Retryable
	if req.Uncertain {
		retryable = false
		msg = "uncertain: " + msg + "; the job may have been printed and is not retried to avoid a duplicate print"
	}
	j, err := ws.s.store.FailJob(ctx, store.FailRequest{
		JobID: jobID, WorkerID: workerID, Error: msg, Retryable: retryable, Now: ws.s.opts.Now(),
	})
	if err != nil {
		return toAPI(err)
	}
	ws.s.log.Warn("job failed", "job", j.ID, "worker", workerID, "printer", j.PrinterID, "status", j.Status, "error", msg)
	ws.s.notify.broadcast()
	return nil
}

// Heartbeat records liveness and printer reachability.
func (ws *WorkerService) Heartbeat(ctx context.Context, workerID string, req api.HeartbeatRequest) error {
	hb := store.Heartbeat{WorkerID: workerID, Version: req.Version, At: ws.s.opts.Now()}
	for _, p := range req.Printers {
		hb.Printers = append(hb.Printers, store.Reachability{PrinterID: p.PrinterID, Reachable: p.Reachable, Error: p.Error})
	}
	return toAPI(ws.s.store.RecordHeartbeat(ctx, hb))
}

// Register exchanges an enrollment token for a permanent credential and
// binds the worker to the mesh peer of the connection.
func (ws *WorkerService) Register(ctx context.Context, token string, peer Peer) (*api.RegisterResponse, error) {
	if token == "" {
		return nil, errBadRequest("token is required")
	}
	credential := auth.NewToken(auth.PrefixWorker)
	w, err := ws.s.store.RedeemEnrollmentToken(ctx, store.Enrollment{
		TokenHash: auth.Hash(token), CredentialHash: auth.Hash(credential),
		PeerID: peer.ID, PeerFQDN: peer.FQDN, Now: ws.s.opts.Now(),
	})
	if err != nil {
		return nil, toAPI(err)
	}
	ws.s.log.Info("worker enrolled", "worker", w.ID, "peer", peer.ID, "fqdn", peer.FQDN)
	return &api.RegisterResponse{WorkerID: w.ID, Credential: credential}, nil
}

// ---- HTTP ----

type workerHandler func(w http.ResponseWriter, r *http.Request, worker *store.Worker)

// workerAuth authenticates the worker credential and checks that the
// request comes from the mesh peer bound during enrollment.
func (s *Server) workerAuth(h workerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := auth.BearerToken(r)
		if token == "" {
			writeError(w, errUnauthorized())
			return
		}
		worker, err := s.store.GetWorkerByCredentialHash(r.Context(), auth.Hash(token))
		if errors.Is(err, store.ErrNotFound) || (err == nil && !auth.Verify(token, worker.CredentialHash)) {
			writeError(w, errUnauthorized())
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		peer, ok := s.opts.Peers.PeerForAddr(r.RemoteAddr)
		if !ok || worker.PeerID == "" || peer.ID != worker.PeerID {
			s.log.Warn("worker request from unexpected peer", "worker", worker.ID, "remote", r.RemoteAddr, "peer", peer.ID)
			writeError(w, errForbidden("request does not come from the peer this worker is bound to"))
			return
		}
		h(w, r, worker)
	}
}

func (s *Server) workerRoutes(mux *http.ServeMux) {
	ws := s.Workers
	mux.HandleFunc("POST /worker/v1/register", func(w http.ResponseWriter, r *http.Request) {
		var req api.RegisterRequest
		if err := decodeJSON(w, r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		peer, ok := s.opts.Peers.PeerForAddr(r.RemoteAddr)
		if !ok || peer.ID == "" {
			writeError(w, errForbidden("cannot determine the mesh peer of this connection"))
			return
		}
		resp, err := ws.Register(r.Context(), req.Token, peer)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /worker/v1/config", s.workerAuth(func(w http.ResponseWriter, r *http.Request, worker *store.Worker) {
		cfg, err := ws.Config(r.Context(), worker.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	}))
	mux.HandleFunc("POST /worker/v1/lease", s.workerAuth(func(w http.ResponseWriter, r *http.Request, worker *store.Worker) {
		var req api.LeaseRequest
		if err := decodeOptionalJSON(w, r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		lease, err := ws.Lease(r.Context(), worker.ID, req.Printers, time.Duration(req.WaitSeconds)*time.Second)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if lease == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, lease)
	}))
	mux.HandleFunc("GET /worker/v1/jobs/{id}/document", s.workerAuth(func(w http.ResponseWriter, r *http.Request, worker *store.Worker) {
		rc, j, err := ws.Document(r.Context(), worker.ID, r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", j.ContentType)
		w.Header().Set("Content-Length", strconv.FormatInt(j.Size, 10))
		w.Header().Set("X-Document-SHA256", j.DocumentHash)
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, rc); err != nil {
			s.log.Warn("sending document", "job", j.ID, "err", err)
		}
	}))
	mux.HandleFunc("POST /worker/v1/jobs/{id}/progress", s.workerAuth(func(w http.ResponseWriter, r *http.Request, worker *store.Worker) {
		resp, err := ws.Progress(r.Context(), worker.ID, r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}))
	mux.HandleFunc("POST /worker/v1/jobs/{id}/complete", s.workerAuth(func(w http.ResponseWriter, r *http.Request, worker *store.Worker) {
		if err := ws.Complete(r.Context(), worker.ID, r.PathValue("id")); err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /worker/v1/jobs/{id}/fail", s.workerAuth(func(w http.ResponseWriter, r *http.Request, worker *store.Worker) {
		var req api.FailRequest
		if err := decodeJSON(w, r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := ws.Fail(r.Context(), worker.ID, r.PathValue("id"), req); err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /worker/v1/heartbeat", s.workerAuth(func(w http.ResponseWriter, r *http.Request, worker *store.Worker) {
		var req api.HeartbeatRequest
		if err := decodeJSON(w, r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := ws.Heartbeat(r.Context(), worker.ID, req); err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
