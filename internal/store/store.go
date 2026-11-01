// Package store defines the persistence interface of the platen server and
// its data model. The SQLite implementation lives in this package as well;
// other databases can be added by implementing Store.
package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned when a requested record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when an operation is not valid in the current
	// state of a record, e.g. canceling a job that is already printing.
	ErrConflict = errors.New("conflict")
	// ErrExists is returned when creating a record whose ID is taken.
	ErrExists = errors.New("already exists")
	// ErrIdempotencyMismatch is returned when an idempotency key is reused
	// for a request with different content.
	ErrIdempotencyMismatch = errors.New("idempotency key reused with different request")
	// ErrTokenInvalid is returned when an enrollment token is unknown,
	// expired or already used.
	ErrTokenInvalid = errors.New("enrollment token invalid")
)

// LocalWorkerID is the fixed ID of the worker built into the server.
const LocalWorkerID = "local"

// Worker is a print worker, either remote or the built-in one.
type Worker struct {
	ID       string
	Name     string
	Location string
	// CredentialHash is the hash of the permanent worker credential. Empty
	// until the worker has enrolled; always empty for the built-in worker.
	CredentialHash string
	// PeerID is the NetBird peer identity (WireGuard public key) bound
	// during enrollment.
	PeerID        string
	PeerFQDN      string
	Version       string
	LastHeartbeat *time.Time
	EnrolledAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PrinterStatus is the administrative status of a printer.
type PrinterStatus string

const (
	// PrinterReady printers receive jobs.
	PrinterReady PrinterStatus = "ready"
	// PrinterPaused printers keep their queue but are not leased jobs.
	PrinterPaused PrinterStatus = "paused"
)

// Printer is a printer reachable by exactly one worker.
type Printer struct {
	ID       string
	Name     string
	WorkerID string
	// Address is host:port in the worker's local network.
	Address       string
	Backend       string
	PaperSize     string
	ResolutionDPI int
	// Options holds backend specific settings, e.g. the dithering mode.
	Options map[string]string
	Status  PrinterStatus
	// Reachable is the last reachability reported by the worker; nil if
	// never reported.
	Reachable         *bool
	ReachabilityError string
	ReachabilityAt    *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Queue groups printers that can serve the same jobs. Currently every
// printer has exactly one queue with the printer's ID; the data model allows
// queues with several printers (pools) later.
type Queue struct {
	ID         string
	Name       string
	PrinterIDs []string
}

// JobStatus is the life cycle state of a job.
type JobStatus string

const (
	JobQueued   JobStatus = "queued"
	JobLeased   JobStatus = "leased"
	JobPrinting JobStatus = "printing"
	JobDone     JobStatus = "done"
	JobFailed   JobStatus = "failed"
	JobCanceled JobStatus = "canceled"
)

// Final reports whether the status is terminal.
func (s JobStatus) Final() bool {
	return s == JobDone || s == JobFailed || s == JobCanceled
}

// Job is a print job.
type Job struct {
	ID      string
	QueueID string
	// PrinterID is the printer the job is (or was last) assigned to. It is
	// set when the job is leased.
	PrinterID      string
	AppClientID    string
	Status         JobStatus
	Copies         int
	Title          string
	ContentType    string
	Size           int64
	DocumentHash   string
	IdempotencyKey string
	// LeaseHolder is the worker that holds or last held the lease.
	LeaseHolder    string
	LeaseExpiresAt *time.Time
	Attempts       int
	MaxAttempts    int
	Error          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LeasedAt       *time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

// AppClient is an application that submits jobs.
type AppClient struct {
	ID        string
	Name      string
	TokenHash string
	// AllPrinters grants access to every printer, including future ones.
	AllPrinters bool
	PrinterIDs  []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// MayUse reports whether the client may print on the printer.
func (c *AppClient) MayUse(printerID string) bool {
	if c.AllPrinters {
		return true
	}
	for _, id := range c.PrinterIDs {
		if id == printerID {
			return true
		}
	}
	return false
}

// EnrollmentToken is a single use token a worker exchanges for its
// permanent credential.
type EnrollmentToken struct {
	ID        string
	WorkerID  string
	TokenHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// Enrollment carries the result of redeeming an enrollment token.
type Enrollment struct {
	TokenHash      string
	CredentialHash string
	PeerID         string
	PeerFQDN       string
	Now            time.Time
}

// Reachability is a printer reachability report from a worker.
type Reachability struct {
	PrinterID string
	Reachable bool
	Error     string
}

// Heartbeat is a worker heartbeat.
type Heartbeat struct {
	WorkerID string
	Version  string
	At       time.Time
	Printers []Reachability
}

// PanelRole is the role of a web panel user.
type PanelRole string

const (
	// PanelAdmin may change everything, including panel users.
	PanelAdmin PanelRole = "admin"
	// PanelViewer may only look.
	PanelViewer PanelRole = "viewer"
)

// PanelUser is a user of the web panel. Users are identified by their
// NetBird IPv4 address; access also requires the NetBird peer (WireGuard
// public key) bound to that address. An empty PeerID is bound on the
// user's first visit.
//
// Users who open the panel through a NetBird reverse proxy are identified
// by their e-mail address instead, which is stored in IP in lower case;
// they have no peer binding.
type PanelUser struct {
	IP        string
	PeerID    string
	PeerFQDN  string
	Name      string
	Role      PanelRole
	CreatedAt time.Time
	UpdatedAt time.Time
	LastSeen  *time.Time
}

// IsEmail reports whether the user is identified by e-mail address.
func (u *PanelUser) IsEmail() bool { return strings.Contains(u.IP, "@") }

// ProxyKey lets a NetBird reverse proxy name the panel user it forwards a
// request for. Only the hash of the key is stored.
type ProxyKey struct {
	ID        string
	Name      string
	Hash      string
	CreatedAt time.Time
	LastUsed  *time.Time
	// LastPeer is the NetBird name of the peer that last used the key.
	LastPeer string
}

// PrinterFilter restricts ListPrinters. Zero values match everything.
type PrinterFilter struct {
	WorkerID string
}

// JobFilter restricts ListJobs. Zero values match everything.
type JobFilter struct {
	Status      JobStatus
	PrinterID   string
	AppClientID string
	WorkerID    string
	Limit       int
}

// LeaseRequest asks for the next job for a worker.
type LeaseRequest struct {
	WorkerID string
	// PrinterIDs restricts the lease to these printers of the worker, e.g.
	// the printers that are currently idle. Empty means all printers.
	PrinterIDs []string
	Now        time.Time
	Duration   time.Duration
}

// FailRequest reports a failed job.
type FailRequest struct {
	JobID     string
	WorkerID  string
	Error     string
	Retryable bool
	Now       time.Time
}

// Store is the persistence interface of the server.
type Store interface {
	Close() error

	CreateWorker(ctx context.Context, w *Worker) error
	GetWorker(ctx context.Context, id string) (*Worker, error)
	GetWorkerByCredentialHash(ctx context.Context, hash string) (*Worker, error)
	ListWorkers(ctx context.Context) ([]Worker, error)
	// UpdateWorker updates name and location.
	UpdateWorker(ctx context.Context, w *Worker) error
	DeleteWorker(ctx context.Context, id string) error
	RecordHeartbeat(ctx context.Context, hb Heartbeat) error

	CreateEnrollmentToken(ctx context.Context, t *EnrollmentToken) error
	// RedeemEnrollmentToken marks the token as used and binds the
	// credential and peer identity to the worker in one transaction.
	RedeemEnrollmentToken(ctx context.Context, e Enrollment) (*Worker, error)

	// CreatePrinter creates the printer and its queue.
	CreatePrinter(ctx context.Context, p *Printer) error
	GetPrinter(ctx context.Context, id string) (*Printer, error)
	ListPrinters(ctx context.Context, f PrinterFilter) ([]Printer, error)
	// UpdatePrinter updates all administrative fields.
	UpdatePrinter(ctx context.Context, p *Printer) error
	DeletePrinter(ctx context.Context, id string) error
	GetQueue(ctx context.Context, id string) (*Queue, error)

	CreateAppClient(ctx context.Context, c *AppClient) error
	GetAppClient(ctx context.Context, id string) (*AppClient, error)
	GetAppClientByTokenHash(ctx context.Context, hash string) (*AppClient, error)
	ListAppClients(ctx context.Context) ([]AppClient, error)
	// UpdateAppClient updates name, token hash and printer permissions.
	UpdateAppClient(ctx context.Context, c *AppClient) error
	DeleteAppClient(ctx context.Context, id string) error

	// CreateJob inserts a job. If the job carries an idempotency key that
	// the app client already used, the existing job is returned with
	// created == false. If the existing job differs in printer or
	// document, ErrIdempotencyMismatch is returned.
	CreateJob(ctx context.Context, j *Job) (job *Job, created bool, err error)
	GetJob(ctx context.Context, id string) (*Job, error)
	ListJobs(ctx context.Context, f JobFilter) ([]Job, error)
	// CancelJob cancels a queued job; other states yield ErrConflict.
	CancelJob(ctx context.Context, id string, now time.Time) (*Job, error)
	// RetryJob puts a failed or canceled job back into its queue with its
	// attempts reset; other states yield ErrConflict.
	RetryJob(ctx context.Context, id string, now time.Time) (*Job, error)
	// DeleteJob deletes a job that no worker is processing; a leased or
	// printing job yields ErrConflict.
	DeleteJob(ctx context.Context, id string) error

	// LeaseJob assigns the oldest eligible queued job to the worker in a
	// single transaction. A printer only receives a job while it has no
	// other leased or printing job. Returns nil if there is none.
	LeaseJob(ctx context.Context, r LeaseRequest) (*Job, error)
	// ExtendLease extends the worker's lease and sets the job to printing.
	ExtendLease(ctx context.Context, jobID, workerID string, now, until time.Time) (*Job, error)
	// CompleteJob marks the job as done. It is accepted from the last lease
	// holder even after its lease expired, as long as the job is not final.
	CompleteJob(ctx context.Context, jobID, workerID string, now time.Time) (*Job, error)
	// FailJob records a failure. Retryable failures requeue the job until
	// it reaches its maximum number of attempts.
	FailJob(ctx context.Context, r FailRequest) (*Job, error)
	// ExpireLeases requeues or fails jobs whose lease expired.
	ExpireLeases(ctx context.Context, now time.Time) ([]Job, error)
	// DeleteFinishedJobs deletes final jobs that finished before the given
	// time and returns their IDs.
	DeleteFinishedJobs(ctx context.Context, before time.Time) ([]string, error)
	// JobExists reports whether a job with the ID exists.
	JobExists(ctx context.Context, id string) (bool, error)

	ListPanelUsers(ctx context.Context) ([]PanelUser, error)
	GetPanelUser(ctx context.Context, ip string) (*PanelUser, error)
	CreatePanelUser(ctx context.Context, u *PanelUser) error
	// ClaimFirstPanelAdmin creates the user only if there are no panel
	// users yet; otherwise it returns ErrConflict.
	ClaimFirstPanelAdmin(ctx context.Context, u *PanelUser) error
	// UpdatePanelUser updates name, role and peer binding. It returns
	// ErrConflict if no admin would be left.
	UpdatePanelUser(ctx context.Context, u *PanelUser) error
	// DeletePanelUser returns ErrConflict if no admin would be left.
	DeletePanelUser(ctx context.Context, ip string) error
	TouchPanelUser(ctx context.Context, ip string, at time.Time) error

	ListProxyKeys(ctx context.Context) ([]ProxyKey, error)
	CreateProxyKey(ctx context.Context, k *ProxyKey) error
	GetProxyKeyByHash(ctx context.Context, hash string) (*ProxyKey, error)
	DeleteProxyKey(ctx context.Context, id string) error
	TouchProxyKey(ctx context.Context, id string, at time.Time, peer string) error

	// GetSetting returns ErrNotFound for a setting that is not set.
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string, at time.Time) error
	DeleteSetting(ctx context.Context, key string) error
}
