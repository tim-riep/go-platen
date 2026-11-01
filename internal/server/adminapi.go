package server

import (
	"errors"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/store"
)

const (
	defaultPaperSize  = "A4"
	defaultResolution = 600
	maxEnrollmentTTL  = 30 * 24 * time.Hour
)

func (s *Server) adminAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.AdminTokenHash == "" {
			writeError(w, errForbidden("the admin API is disabled: no admin token hash configured"))
			return
		}
		if !auth.Verify(auth.BearerToken(r), s.opts.AdminTokenHash) {
			writeError(w, errUnauthorized())
			return
		}
		h(w, r)
	}
}

func (s *Server) adminRoutes(mux *http.ServeMux) {
	handle := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, s.adminAuth(h))
	}
	handle("GET /admin/v1/workers", s.adminListWorkers)
	handle("POST /admin/v1/workers", s.adminCreateWorker)
	handle("GET /admin/v1/workers/{id}", s.adminGetWorker)
	handle("PATCH /admin/v1/workers/{id}", s.adminUpdateWorker)
	handle("DELETE /admin/v1/workers/{id}", s.adminDeleteWorker)
	handle("POST /admin/v1/workers/{id}/enrollment-tokens", s.adminCreateEnrollmentToken)

	handle("GET /admin/v1/printers", s.adminListPrinters)
	handle("POST /admin/v1/printers", s.adminCreatePrinter)
	handle("GET /admin/v1/printers/{id}", s.adminGetPrinter)
	handle("PATCH /admin/v1/printers/{id}", s.adminUpdatePrinter)
	handle("DELETE /admin/v1/printers/{id}", s.adminDeletePrinter)

	handle("GET /admin/v1/app-clients", s.adminListAppClients)
	handle("POST /admin/v1/app-clients", s.adminCreateAppClient)
	handle("GET /admin/v1/app-clients/{id}", s.adminGetAppClient)
	handle("PATCH /admin/v1/app-clients/{id}", s.adminUpdateAppClient)
	handle("DELETE /admin/v1/app-clients/{id}", s.adminDeleteAppClient)
	handle("POST /admin/v1/app-clients/{id}/rotate-token", s.adminRotateAppClientToken)

	// Panel users; the way back in when no panel admin can log in.
	handle("GET /admin/v1/panel-users", s.listPanelUsers)
	handle("POST /admin/v1/panel-users", s.createPanelUser)
	handle("PATCH /admin/v1/panel-users/{ip}", s.updatePanelUser)
	handle("DELETE /admin/v1/panel-users/{ip}", s.deletePanelUser)
	handle("POST /admin/v1/panel-users/{ip}/reset-peer", s.resetPanelUserPeer)
	handle("GET /admin/v1/proxy-keys", s.listProxyKeys)
	handle("POST /admin/v1/proxy-keys", s.createProxyKey)
	handle("DELETE /admin/v1/proxy-keys/{id}", s.deleteProxyKey)
	handle("GET /admin/v1/netbird", s.getNetBirdSync)
	handle("PUT /admin/v1/netbird", s.putNetBirdSync)
	handle("DELETE /admin/v1/netbird", s.deleteNetBirdSync)
	handle("POST /admin/v1/netbird/sync", s.syncNetBirdNow)

	handle("POST /admin/v1/printers/{id}/jobs", s.adminSubmitJob)
	handle("GET /admin/v1/jobs", s.adminListJobs)
	handle("GET /admin/v1/jobs/{id}", s.adminGetJob)
	handle("POST /admin/v1/jobs/{id}/cancel", s.adminCancelJob)
	handle("POST /admin/v1/jobs/{id}/retry", s.adminRetryJob)
	handle("DELETE /admin/v1/jobs/{id}", s.adminDeleteJob)
}

// ---- workers ----

func apiWorker(w *store.Worker) api.Worker {
	return api.Worker{
		ID: w.ID, Name: w.Name, Location: w.Location, Enrolled: w.CredentialHash != "", PeerID: w.PeerID, PeerFQDN: w.PeerFQDN,
		Version: w.Version, LastHeartbeat: w.LastHeartbeat, EnrolledAt: w.EnrolledAt, CreatedAt: w.CreatedAt,
	}
}

func (s *Server) adminListWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := s.store.ListWorkers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.List[api.Worker]{Items: []api.Worker{}}
	for i := range workers {
		out.Items = append(out.Items, apiWorker(&workers[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminCreateWorker(w http.ResponseWriter, r *http.Request) {
	var req api.WorkerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := checkID("worker", req.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ID == store.LocalWorkerID {
		writeError(w, errBadRequest("worker id %q is reserved for the built-in worker", store.LocalWorkerID))
		return
	}
	now := s.opts.Now()
	wk := &store.Worker{ID: req.ID, Name: req.ID, CreatedAt: now, UpdatedAt: now}
	if req.Name != nil {
		wk.Name = *req.Name
	}
	if req.Location != nil {
		wk.Location = *req.Location
	}
	if err := s.store.CreateWorker(r.Context(), wk); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, apiWorker(wk))
}

func (s *Server) adminGetWorker(w http.ResponseWriter, r *http.Request) {
	wk, err := s.store.GetWorker(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiWorker(wk))
}

func (s *Server) adminUpdateWorker(w http.ResponseWriter, r *http.Request) {
	var req api.WorkerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	wk, err := s.store.GetWorker(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ID != "" && req.ID != wk.ID {
		writeError(w, errBadRequest("the id of a worker cannot be changed"))
		return
	}
	if req.Name != nil {
		wk.Name = *req.Name
	}
	if req.Location != nil {
		wk.Location = *req.Location
	}
	wk.UpdatedAt = s.opts.Now()
	if err := s.store.UpdateWorker(r.Context(), wk); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiWorker(wk))
}

func (s *Server) adminDeleteWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == store.LocalWorkerID {
		writeError(w, errBadRequest("the built-in worker cannot be deleted; disable it in the configuration"))
		return
	}
	if err := s.store.DeleteWorker(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminCreateEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollmentTokenRequest
	if err := decodeOptionalJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	if id == store.LocalWorkerID {
		writeError(w, errBadRequest("the built-in worker does not enroll"))
		return
	}
	ttl := s.opts.EnrollmentTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl > maxEnrollmentTTL {
		writeError(w, errBadRequest("ttl must not exceed %s", maxEnrollmentTTL))
		return
	}
	now := s.opts.Now()
	token := auth.NewToken(auth.PrefixEnrollment)
	t := &store.EnrollmentToken{ID: auth.NewID(), WorkerID: id, TokenHash: auth.Hash(token), ExpiresAt: now.Add(ttl), CreatedAt: now}
	if err := s.store.CreateEnrollmentToken(r.Context(), t); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("enrollment token created", "worker", id, "expires", t.ExpiresAt)
	writeJSON(w, http.StatusCreated, api.EnrollmentToken{Token: token, WorkerID: id, ExpiresAt: t.ExpiresAt})
}

// ---- printers ----

func apiAdminPrinter(p *store.Printer) api.AdminPrinter {
	return api.AdminPrinter{
		PrinterConfig: printerConfig(p), WorkerID: p.WorkerID, Reachable: p.Reachable,
		ReachabilityError: p.ReachabilityError, ReachabilityAt: p.ReachabilityAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func (s *Server) adminListPrinters(w http.ResponseWriter, r *http.Request) {
	printers, err := s.store.ListPrinters(r.Context(), store.PrinterFilter{WorkerID: r.URL.Query().Get("worker")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.List[api.AdminPrinter]{Items: []api.AdminPrinter{}}
	for i := range printers {
		out.Items = append(out.Items, apiAdminPrinter(&printers[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) applyPrinter(p *store.Printer, req *api.PrinterRequest) error {
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.WorkerID != nil {
		p.WorkerID = *req.WorkerID
	}
	if req.Address != nil {
		p.Address = *req.Address
	}
	if req.Backend != nil {
		p.Backend = *req.Backend
	}
	if req.PaperSize != nil {
		p.PaperSize = *req.PaperSize
	}
	if req.ResolutionDPI != nil {
		p.ResolutionDPI = *req.ResolutionDPI
	}
	if req.Options != nil {
		p.Options = *req.Options
	}
	if req.Status != nil {
		p.Status = store.PrinterStatus(*req.Status)
	}
	switch {
	case p.WorkerID == "":
		return errBadRequest("worker_id is required")
	case p.Address == "":
		return errBadRequest("address is required")
	case p.Backend == "":
		return errBadRequest("backend is required")
	case p.ResolutionDPI < 72 || p.ResolutionDPI > 2400:
		return errBadRequest("resolution_dpi must be between 72 and 2400")
	case p.Status != store.PrinterReady && p.Status != store.PrinterPaused:
		return errBadRequest("status must be %q or %q", store.PrinterReady, store.PrinterPaused)
	}
	if !strings.Contains(p.Address, ":") {
		// Raw TCP printers listen on 9100, IPP printers on 631.
		if p.Backend == "ipp" {
			p.Address += ":631"
		} else {
			p.Address += ":9100"
		}
	}
	if s.opts.Backends != nil {
		if _, ok := s.opts.Backends(p.Backend); !ok {
			return errBadRequest("unknown backend %q", p.Backend)
		}
	}
	return nil
}

func (s *Server) adminCreatePrinter(w http.ResponseWriter, r *http.Request) {
	var req api.PrinterRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := checkID("printer", req.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	now := s.opts.Now()
	p := &store.Printer{
		ID: req.ID, Name: req.ID, PaperSize: defaultPaperSize, ResolutionDPI: defaultResolution,
		Status: store.PrinterReady, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.applyPrinter(p, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.CreatePrinter(r.Context(), p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.notify.broadcast()
	writeJSON(w, http.StatusCreated, apiAdminPrinter(p))
}

func (s *Server) adminGetPrinter(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPrinter(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiAdminPrinter(p))
}

func (s *Server) adminUpdatePrinter(w http.ResponseWriter, r *http.Request) {
	var req api.PrinterRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.store.GetPrinter(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ID != "" && req.ID != p.ID {
		writeError(w, errBadRequest("the id of a printer cannot be changed"))
		return
	}
	if err := s.applyPrinter(p, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	p.UpdatedAt = s.opts.Now()
	if err := s.store.UpdatePrinter(r.Context(), p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.notify.broadcast()
	writeJSON(w, http.StatusOK, apiAdminPrinter(p))
}

func (s *Server) adminDeletePrinter(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePrinter(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- app clients ----

func apiAppClient(c *store.AppClient) api.AppClient {
	printers := c.PrinterIDs
	if printers == nil {
		printers = []string{}
	}
	return api.AppClient{ID: c.ID, Name: c.Name, AllPrinters: c.AllPrinters, Printers: printers, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func applyAppClient(c *store.AppClient, req *api.AppClientRequest) {
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.AllPrinters != nil {
		c.AllPrinters = *req.AllPrinters
	}
	if req.Printers != nil {
		c.PrinterIDs = slices.Clone(*req.Printers)
	}
}

func (s *Server) adminListAppClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.store.ListAppClients(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.List[api.AppClient]{Items: []api.AppClient{}}
	for i := range clients {
		out.Items = append(out.Items, apiAppClient(&clients[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminCreateAppClient(w http.ResponseWriter, r *http.Request) {
	var req api.AppClientRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := checkID("app client", req.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	now := s.opts.Now()
	token := auth.NewToken(auth.PrefixApp)
	c := &store.AppClient{ID: req.ID, Name: req.ID, TokenHash: auth.Hash(token), CreatedAt: now, UpdatedAt: now}
	applyAppClient(c, &req)
	if err := s.store.CreateAppClient(r.Context(), c); err != nil {
		s.fail(w, r, err)
		return
	}
	out := apiAppClient(c)
	out.Token = token
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) adminGetAppClient(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetAppClient(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiAppClient(c))
}

func (s *Server) adminUpdateAppClient(w http.ResponseWriter, r *http.Request) {
	var req api.AppClientRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.store.GetAppClient(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ID != "" && req.ID != c.ID {
		writeError(w, errBadRequest("the id of an app client cannot be changed"))
		return
	}
	applyAppClient(c, &req)
	c.UpdatedAt = s.opts.Now()
	if err := s.store.UpdateAppClient(r.Context(), c); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiAppClient(c))
}

func (s *Server) adminDeleteAppClient(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAppClient(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminRotateAppClientToken(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetAppClient(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	token := auth.NewToken(auth.PrefixApp)
	c.TokenHash, c.UpdatedAt = auth.Hash(token), s.opts.Now()
	if err := s.store.UpdateAppClient(r.Context(), c); err != nil {
		s.fail(w, r, err)
		return
	}
	out := apiAppClient(c)
	out.Token = token
	writeJSON(w, http.StatusOK, out)
}

// ---- jobs ----

func apiAdminJob(j *store.Job) api.AdminJob {
	return api.AdminJob{
		Job: apiJob(j), AppClientID: j.AppClientID, IdempotencyKey: j.IdempotencyKey, LeaseHolder: j.LeaseHolder,
		LeaseExpiresAt: j.LeaseExpiresAt, MaxAttempts: j.MaxAttempts,
	}
}

func (s *Server) adminListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.JobFilter{
		Status: store.JobStatus(q.Get("status")), PrinterID: q.Get("printer"),
		AppClientID: q.Get("app_client"), WorkerID: q.Get("worker"),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, errBadRequest("invalid limit"))
			return
		}
		f.Limit = n
	}
	jobs, err := s.store.ListJobs(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.List[api.AdminJob]{Items: []api.AdminJob{}}
	for i := range jobs {
		out.Items = append(out.Items, apiAdminJob(&jobs[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminGetJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiAdminJob(j))
}

func (s *Server) adminCancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.CancelJob(r.Context(), r.PathValue("id"), s.opts.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiAdminJob(j))
}

// adminRetryJob puts a failed or canceled job back into its queue.
func (s *Server) adminRetryJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, err := s.spool.Open(id)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, api.Errorf(http.StatusGone, api.CodeNotFound, "the document of job %s is no longer available", id))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f.Close()
	j, err := s.store.RetryJob(r.Context(), id, s.opts.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("job retried", "job", j.ID, "printer", j.QueueID, "by", panelActor(r))
	s.notify.broadcast()
	writeJSON(w, http.StatusOK, apiAdminJob(j))
}

// adminDeleteJob deletes a job and its document.
func (s *Server) adminDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteJob(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.spool.Remove(id); err != nil {
		s.log.Error("delete document", "job", id, "err", err)
	}
	s.log.Info("job deleted", "job", id, "by", panelActor(r))
	w.WriteHeader(http.StatusNoContent)
}
