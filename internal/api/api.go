// Package api defines the JSON wire types of the platen HTTP interfaces.
// They are shared by the server, the worker and the admin CLI.
package api

import (
	"fmt"
	"net/http"
	"time"
)

// Error codes.
const (
	CodeBadRequest          = "bad_request"
	CodeUnauthorized        = "unauthorized"
	CodeForbidden           = "forbidden"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodeAlreadyExists       = "already_exists"
	CodeTooLarge            = "document_too_large"
	CodeUnsupportedMedia    = "unsupported_media_type"
	CodeIdempotencyMismatch = "idempotency_key_mismatch"
	CodeInvalidToken        = "invalid_token"
	CodeInternal            = "internal_error"
	CodeUnavailable         = "unavailable"
)

// Error is the uniform error of all endpoints. It is sent as
// {"error": {"code": "...", "message": "..."}}.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ErrorBody is the JSON envelope of an Error.
type ErrorBody struct {
	Error *Error `json:"error"`
}

// Errorf returns an *Error.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsStatus reports whether err is an *Error with the HTTP status.
func IsStatus(err error, status int) bool {
	e, ok := err.(*Error)
	return ok && e.Status == status
}

// IsPermanent reports whether a worker should stop retrying a request that
// failed with err: client errors are permanent, transport errors and server
// errors are not.
func IsPermanent(err error) bool {
	e, ok := err.(*Error)
	return ok && e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests
}

// ---- application API ----

// Printer is a printer as seen by applications.
type Printer struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
	Reachable      *bool      `json:"reachable"`
	ReachabilityAt *time.Time `json:"reachability_checked_at,omitempty"`
	PaperSize      string     `json:"paper_size"`
	ResolutionDPI  int        `json:"resolution_dpi"`
	ContentTypes   []string   `json:"content_types,omitempty"`
}

// PrinterList is the response of GET /v1/printers.
type PrinterList struct {
	Printers []Printer `json:"printers"`
}

// Job is a print job.
type Job struct {
	ID           string     `json:"id"`
	PrinterID    string     `json:"printer_id"`
	Status       string     `json:"status"`
	Copies       int        `json:"copies"`
	Title        string     `json:"title,omitempty"`
	ContentType  string     `json:"content_type"`
	Size         int64      `json:"size"`
	DocumentHash string     `json:"document_sha256"`
	Attempts     int        `json:"attempts"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// Health is the response of GET /v1/health.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// ---- worker API ----

// RegisterRequest exchanges an enrollment token for a credential.
type RegisterRequest struct {
	Token   string `json:"token"`
	Version string `json:"version,omitempty"`
}

// RegisterResponse carries the permanent worker credential. It is returned
// exactly once.
type RegisterResponse struct {
	WorkerID   string `json:"worker_id"`
	Credential string `json:"credential"`
}

// PrinterConfig is a printer as seen by its worker.
type PrinterConfig struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Address       string            `json:"address"`
	Backend       string            `json:"backend"`
	PaperSize     string            `json:"paper_size"`
	ResolutionDPI int               `json:"resolution_dpi"`
	Options       map[string]string `json:"options,omitempty"`
	Status        string            `json:"status"`
}

// WorkerConfig is the response of GET /worker/v1/config.
type WorkerConfig struct {
	WorkerID string          `json:"worker_id"`
	Name     string          `json:"name"`
	Printers []PrinterConfig `json:"printers"`
	// LeaseSeconds is the lease duration; workers report progress well
	// within it.
	LeaseSeconds int `json:"lease_seconds"`
}

// LeaseRequest is the body of POST /worker/v1/lease.
type LeaseRequest struct {
	// Printers restricts the lease to these (idle) printers. Empty means
	// all printers of the worker.
	Printers []string `json:"printers,omitempty"`
	// WaitSeconds is the long polling timeout, capped by the server.
	WaitSeconds int `json:"wait_seconds,omitempty"`
}

// LeasedJob is the job part of a lease.
type LeasedJob struct {
	ID           string `json:"id"`
	Copies       int    `json:"copies"`
	Title        string `json:"title,omitempty"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
	DocumentHash string `json:"document_sha256"`
	Attempt      int    `json:"attempt"`
}

// Lease is the response of POST /worker/v1/lease when a job is available.
// Without a job the server answers 204 No Content.
type Lease struct {
	Job       LeasedJob     `json:"job"`
	Printer   PrinterConfig `json:"printer"`
	ExpiresAt time.Time     `json:"expires_at"`
}

// ProgressResponse is the response of POST /worker/v1/jobs/{id}/progress.
type ProgressResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// FailRequest is the body of POST /worker/v1/jobs/{id}/fail.
type FailRequest struct {
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
	// Uncertain marks a job whose data may have reached the printer before
	// the worker crashed. It is never retried.
	Uncertain bool `json:"uncertain,omitempty"`
}

// Reachability is the reachability of one printer.
type Reachability struct {
	PrinterID string `json:"printer_id"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// HeartbeatRequest is the body of POST /worker/v1/heartbeat.
type HeartbeatRequest struct {
	Version  string         `json:"version"`
	Printers []Reachability `json:"printers"`
}

// ---- admin API ----

// Worker is a worker as seen by administrators.
type Worker struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Location      string     `json:"location,omitempty"`
	Enrolled      bool       `json:"enrolled"`
	PeerID        string     `json:"peer_id,omitempty"`
	PeerFQDN      string     `json:"peer_fqdn,omitempty"`
	Version       string     `json:"version,omitempty"`
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	EnrolledAt    *time.Time `json:"enrolled_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// WorkerRequest creates or updates a worker. On update, nil fields are
// left unchanged.
type WorkerRequest struct {
	ID       string  `json:"id,omitempty"`
	Name     *string `json:"name,omitempty"`
	Location *string `json:"location,omitempty"`
}

// EnrollmentTokenRequest creates an enrollment token.
type EnrollmentTokenRequest struct {
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// EnrollmentToken is returned once when created.
type EnrollmentToken struct {
	Token     string    `json:"token"`
	WorkerID  string    `json:"worker_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AdminPrinter is a printer as seen by administrators.
type AdminPrinter struct {
	PrinterConfig
	WorkerID          string     `json:"worker_id"`
	Reachable         *bool      `json:"reachable"`
	ReachabilityError string     `json:"reachability_error,omitempty"`
	ReachabilityAt    *time.Time `json:"reachability_checked_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// PrinterRequest creates or updates a printer. On update, nil fields are
// left unchanged.
type PrinterRequest struct {
	ID            string             `json:"id,omitempty"`
	Name          *string            `json:"name,omitempty"`
	WorkerID      *string            `json:"worker_id,omitempty"`
	Address       *string            `json:"address,omitempty"`
	Backend       *string            `json:"backend,omitempty"`
	PaperSize     *string            `json:"paper_size,omitempty"`
	ResolutionDPI *int               `json:"resolution_dpi,omitempty"`
	Options       *map[string]string `json:"options,omitempty"`
	Status        *string            `json:"status,omitempty"`
}

// AppClient is an application client as seen by administrators.
type AppClient struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	AllPrinters bool      `json:"all_printers"`
	Printers    []string  `json:"printers"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Token is only set in the response that created or rotated it.
	Token string `json:"token,omitempty"`
}

// AppClientRequest creates or updates an app client. On update, nil fields
// are left unchanged.
type AppClientRequest struct {
	ID          string    `json:"id,omitempty"`
	Name        *string   `json:"name,omitempty"`
	AllPrinters *bool     `json:"all_printers,omitempty"`
	Printers    *[]string `json:"printers,omitempty"`
}

// AdminJob is a job as seen by administrators.
type AdminJob struct {
	Job
	AppClientID    string     `json:"app_client_id,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	LeaseHolder    string     `json:"lease_holder,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	MaxAttempts    int        `json:"max_attempts"`
}

// List wraps list responses of the admin API.
type List[T any] struct {
	Items []T `json:"items"`
}

// ---- web panel ----

// PanelUser is a user of the web panel, identified by NetBird IPv4 address
// and bound to a NetBird peer.
type PanelUser struct {
	IP       string `json:"ip"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role"`
	PeerID   string `json:"peer_id,omitempty"`
	PeerFQDN string `json:"peer_fqdn,omitempty"`
	// Bound is false until the user's first visit binds the peer.
	Bound     bool       `json:"bound"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
}

// PanelUserRequest creates or updates a panel user. On update, nil fields
// are left unchanged.
type PanelUserRequest struct {
	IP   string  `json:"ip,omitempty"`
	Name *string `json:"name,omitempty"`
	Role *string `json:"role,omitempty"`
}

// PanelMe describes the panel user making the request.
type PanelMe struct {
	PanelUser
	// FirstAdmin is set when this request made the user the panel's
	// first admin.
	FirstAdmin bool   `json:"first_admin,omitempty"`
	Version    string `json:"version"`
}

// Backend describes a printer backend.
type Backend struct {
	Name         string   `json:"name"`
	ContentTypes []string `json:"content_types"`
}
