package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/spool"
	"github.com/tim-riep/go-platen/internal/store"
)

const maxJSONBody = 1 << 20

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func errBadRequest(format string, args ...any) *api.Error {
	return api.Errorf(http.StatusBadRequest, api.CodeBadRequest, format, args...)
}

func errNotFound(format string, args ...any) *api.Error {
	return api.Errorf(http.StatusNotFound, api.CodeNotFound, format, args...)
}

func errUnauthorized() *api.Error {
	return api.Errorf(http.StatusUnauthorized, api.CodeUnauthorized, "missing or invalid credentials")
}

func errForbidden(format string, args ...any) *api.Error {
	return api.Errorf(http.StatusForbidden, api.CodeForbidden, format, args...)
}

func errInternal() *api.Error {
	return api.Errorf(http.StatusInternalServerError, api.CodeInternal, "internal error")
}

// apiError converts store and spool errors to API errors. Unknown errors
// become internal errors; the caller logs them.
func apiError(err error) *api.Error {
	var ae *api.Error
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, store.ErrNotFound):
		return api.Errorf(http.StatusNotFound, api.CodeNotFound, "%s", err)
	case errors.Is(err, store.ErrConflict):
		return api.Errorf(http.StatusConflict, api.CodeConflict, "%s", err)
	case errors.Is(err, store.ErrExists):
		return api.Errorf(http.StatusConflict, api.CodeAlreadyExists, "%s", err)
	case errors.Is(err, store.ErrIdempotencyMismatch):
		return api.Errorf(http.StatusUnprocessableEntity, api.CodeIdempotencyMismatch, "%s", err)
	case errors.Is(err, store.ErrTokenInvalid):
		return api.Errorf(http.StatusUnauthorized, api.CodeInvalidToken, "%s", err)
	case errors.Is(err, spool.ErrTooLarge):
		return api.Errorf(http.StatusRequestEntityTooLarge, api.CodeTooLarge, "%s", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, e *api.Error) {
	writeJSON(w, e.Status, api.ErrorBody{Error: e})
}

// fail writes err as JSON error and logs unexpected errors.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if ae := apiError(err); ae != nil {
		writeError(w, ae)
		return
	}
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, errInternal())
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errBadRequest("request body is empty")
		}
		return errBadRequest("invalid JSON: %v", err)
	}
	if dec.More() {
		return errBadRequest("trailing data after JSON body")
	}
	return nil
}

// decodeOptionalJSON accepts an empty body.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) error {
	err := decodeJSON(w, r, v)
	var ae *api.Error
	if errors.As(err, &ae) && ae.Message == "request body is empty" {
		return nil
	}
	return err
}

func checkID(kind, id string) error {
	if !validID.MatchString(id) {
		return errBadRequest("invalid %s id %q: use 1-64 characters a-z, 0-9, '-' and '_'", kind, id)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: "ok", Version: s.opts.Version})
}

func fmtContentTypes(ct []string) string {
	return fmt.Sprintf("%q", ct)
}

func errTooLarge(limit int64) error {
	return fmt.Errorf("%w: the limit is %d bytes", spool.ErrTooLarge, limit)
}
