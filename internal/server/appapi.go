package server

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/store"
)

const (
	maxCopies         = 999
	maxTitleLen       = 255
	maxIdempotencyLen = 255
)

func apiJob(j *store.Job) api.Job {
	printer := j.PrinterID
	if printer == "" {
		printer = j.QueueID
	}
	return api.Job{
		ID: j.ID, PrinterID: printer, Status: string(j.Status), Copies: j.Copies, Title: j.Title,
		ContentType: j.ContentType, Size: j.Size, DocumentHash: j.DocumentHash, Attempts: j.Attempts, Error: j.Error,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

type appHandler func(w http.ResponseWriter, r *http.Request, client *store.AppClient)

func (s *Server) appAuth(h appHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := auth.BearerToken(r)
		if token == "" {
			writeError(w, errUnauthorized())
			return
		}
		c, err := s.store.GetAppClientByTokenHash(r.Context(), auth.Hash(token))
		if errors.Is(err, store.ErrNotFound) || (err == nil && !auth.Verify(token, c.TokenHash)) {
			writeError(w, errUnauthorized())
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r, c)
	}
}

func (s *Server) appRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/printers", s.appAuth(s.listAppPrinters))
	mux.HandleFunc("POST /v1/printers/{id}/jobs", s.appAuth(s.submitJob))
	mux.HandleFunc("GET /v1/jobs/{id}", s.appAuth(s.getAppJob))
	mux.HandleFunc("DELETE /v1/jobs/{id}", s.appAuth(s.cancelAppJob))
}

func (s *Server) listAppPrinters(w http.ResponseWriter, r *http.Request, c *store.AppClient) {
	printers, err := s.store.ListPrinters(r.Context(), store.PrinterFilter{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.PrinterList{Printers: []api.Printer{}}
	for _, p := range printers {
		if !c.MayUse(p.ID) {
			continue
		}
		ap := api.Printer{
			ID: p.ID, Name: p.Name, Status: string(p.Status), Reachable: p.Reachable, ReachabilityAt: p.ReachabilityAt,
			PaperSize: p.PaperSize, ResolutionDPI: p.ResolutionDPI,
		}
		if s.opts.Backends != nil {
			ap.ContentTypes, _ = s.opts.Backends(p.Backend)
		}
		out.Printers = append(out.Printers, ap)
	}
	writeJSON(w, http.StatusOK, out)
}

// accepts reports whether the printer's backend accepts the content type.
func (s *Server) accepts(p *store.Printer, contentType string) ([]string, bool) {
	if s.opts.Backends == nil {
		return nil, true
	}
	types, ok := s.opts.Backends(p.Backend)
	if !ok {
		return nil, false
	}
	for _, t := range types {
		if t == "*/*" || t == contentType {
			return types, true
		}
	}
	return types, false
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request, c *store.AppClient) {
	ctx := r.Context()
	printerID := r.PathValue("id")
	if !c.MayUse(printerID) {
		// Do not reveal whether the printer exists.
		writeError(w, errNotFound("printer %q not found", printerID))
		return
	}
	p, err := s.store.GetPrinter(ctx, printerID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType == "" {
		writeError(w, api.Errorf(http.StatusUnsupportedMediaType, api.CodeUnsupportedMedia, "a valid Content-Type header is required"))
		return
	}
	if types, ok := s.accepts(p, contentType); !ok {
		writeError(w, api.Errorf(http.StatusUnsupportedMediaType, api.CodeUnsupportedMedia,
			"printer %q does not accept %s (accepted: %s)", p.ID, contentType, fmtContentTypes(types)))
		return
	}

	q := r.URL.Query()
	copies := 1
	if v := q.Get("copies"); v != "" {
		copies, err = strconv.Atoi(v)
		if err != nil || copies < 1 || copies > maxCopies {
			writeError(w, errBadRequest("copies must be between 1 and %d", maxCopies))
			return
		}
	}
	title := q.Get("title")
	if len(title) > maxTitleLen {
		writeError(w, errBadRequest("title must not exceed %d bytes", maxTitleLen))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > maxIdempotencyLen {
		writeError(w, errBadRequest("Idempotency-Key must not exceed %d bytes", maxIdempotencyLen))
		return
	}
	if r.ContentLength > s.opts.MaxDocumentSize {
		writeError(w, apiError(errTooLarge(s.opts.MaxDocumentSize)))
		return
	}

	id := auth.NewID()
	size, hash, err := s.spool.Write(id, r.Body, s.opts.MaxDocumentSize)
	if err != nil {
		if ae := apiError(err); ae != nil {
			writeError(w, ae)
			return
		}
		s.fail(w, r, err)
		return
	}
	if size == 0 {
		_ = s.spool.Remove(id)
		writeError(w, errBadRequest("document is empty"))
		return
	}
	now := s.opts.Now()
	j, created, err := s.store.CreateJob(ctx, &store.Job{
		ID: id, QueueID: p.ID, AppClientID: c.ID, Status: store.JobQueued, Copies: copies, Title: title,
		ContentType: contentType, Size: size, DocumentHash: hash, IdempotencyKey: key,
		MaxAttempts: s.opts.MaxAttempts, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || !created {
		_ = s.spool.Remove(id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if created {
		s.log.Info("job queued", "job", j.ID, "client", c.ID, "printer", p.ID, "content_type", contentType, "size", size)
		s.notify.broadcast()
	} else {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, apiJob(j))
}

func (s *Server) ownJob(ctx context.Context, c *store.AppClient, id string) (*store.Job, error) {
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.AppClientID != c.ID {
		return nil, errNotFound("job %q not found", id)
	}
	return j, nil
}

func (s *Server) getAppJob(w http.ResponseWriter, r *http.Request, c *store.AppClient) {
	j, err := s.ownJob(r.Context(), c, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiJob(j))
}

func (s *Server) cancelAppJob(w http.ResponseWriter, r *http.Request, c *store.AppClient) {
	if _, err := s.ownJob(r.Context(), c, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	j, err := s.store.CancelJob(r.Context(), r.PathValue("id"), s.opts.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("job canceled", "job", j.ID, "client", c.ID)
	writeJSON(w, http.StatusOK, apiJob(j))
}
