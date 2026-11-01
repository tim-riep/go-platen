package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// SQLite is the SQLite implementation of Store.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// OpenSQLite opens (and creates if needed) the database at path and applies
// all pending migrations. Use ":memory:" only in tests.
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// Write transactions take the write lock up front, so concurrent lease
	// transactions serialize instead of failing on lock upgrade.
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if path == ":memory:" {
		// Every connection would get its own in-memory database.
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// Close closes the database.
func (s *SQLite) Close() error { return s.db.Close() }

// queryer is implemented by *sql.DB and *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface {
	Scan(dest ...any) error
}

func (s *SQLite) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// time helpers: timestamps are stored as Unix milliseconds.

func ms(t time.Time) int64 { return t.UnixMilli() }

func msPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromNullMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isFKViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ---- workers ----

const workerCols = `id, name, location, credential_hash, peer_id, peer_fqdn, version, last_heartbeat, enrolled_at, created_at, updated_at`

func scanWorker(sc scanner) (*Worker, error) {
	var w Worker
	var hb, enrolled sql.NullInt64
	var created, updated int64
	err := sc.Scan(&w.ID, &w.Name, &w.Location, &w.CredentialHash, &w.PeerID, &w.PeerFQDN, &w.Version, &hb, &enrolled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.LastHeartbeat = fromNullMS(hb)
	w.EnrolledAt = fromNullMS(enrolled)
	w.CreatedAt, w.UpdatedAt = fromMS(created), fromMS(updated)
	return &w, nil
}

func (s *SQLite) CreateWorker(ctx context.Context, w *Worker) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workers (`+workerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Name, w.Location, w.CredentialHash, w.PeerID, w.PeerFQDN, w.Version,
		msPtr(w.LastHeartbeat), msPtr(w.EnrolledAt), ms(w.CreatedAt), ms(w.UpdatedAt))
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (s *SQLite) GetWorker(ctx context.Context, id string) (*Worker, error) {
	return scanWorker(s.db.QueryRowContext(ctx, `SELECT `+workerCols+` FROM workers WHERE id = ?`, id))
}

func (s *SQLite) GetWorkerByCredentialHash(ctx context.Context, hash string) (*Worker, error) {
	if hash == "" {
		return nil, ErrNotFound
	}
	return scanWorker(s.db.QueryRowContext(ctx, `SELECT `+workerCols+` FROM workers WHERE credential_hash = ?`, hash))
}

func (s *SQLite) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+workerCols+` FROM workers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func (s *SQLite) UpdateWorker(ctx context.Context, w *Worker) error {
	return affected(s.db.ExecContext(ctx, `UPDATE workers SET name = ?, location = ?, updated_at = ? WHERE id = ?`,
		w.Name, w.Location, ms(w.UpdatedAt), w.ID))
}

func (s *SQLite) DeleteWorker(ctx context.Context, id string) error {
	err := affected(s.db.ExecContext(ctx, `DELETE FROM workers WHERE id = ?`, id))
	if isFKViolation(err) {
		return fmt.Errorf("%w: worker still has printers", ErrConflict)
	}
	return err
}

func (s *SQLite) RecordHeartbeat(ctx context.Context, hb Heartbeat) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, `UPDATE workers SET last_heartbeat = ?, version = ? WHERE id = ?`,
			ms(hb.At), hb.Version, hb.WorkerID)); err != nil {
			return err
		}
		for _, r := range hb.Printers {
			// Only printers assigned to this worker are updated; reports
			// for other printers are ignored.
			if _, err := tx.ExecContext(ctx, `UPDATE printers SET reachable = ?, reachability_error = ?, reachability_at = ? WHERE id = ? AND worker_id = ?`,
				r.Reachable, r.Error, ms(hb.At), r.PrinterID, hb.WorkerID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- enrollment ----

func (s *SQLite) CreateEnrollmentToken(ctx context.Context, t *EnrollmentToken) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO enrollment_tokens (id, worker_id, token_hash, expires_at, used_at, created_at) VALUES (?,?,?,?,?,?)`,
		t.ID, t.WorkerID, t.TokenHash, ms(t.ExpiresAt), msPtr(t.UsedAt), ms(t.CreatedAt))
	if isFKViolation(err) {
		return ErrNotFound
	}
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (s *SQLite) RedeemEnrollmentToken(ctx context.Context, e Enrollment) (*Worker, error) {
	var w *Worker
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id, workerID string
		var expires int64
		var used sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT id, worker_id, expires_at, used_at FROM enrollment_tokens WHERE token_hash = ?`, e.TokenHash).
			Scan(&id, &workerID, &expires, &used)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTokenInvalid
		}
		if err != nil {
			return err
		}
		if used.Valid || ms(e.Now) >= expires {
			return ErrTokenInvalid
		}
		if _, err := tx.ExecContext(ctx, `UPDATE enrollment_tokens SET used_at = ? WHERE id = ?`, ms(e.Now), id); err != nil {
			return err
		}
		if err := affected(tx.ExecContext(ctx, `UPDATE workers SET credential_hash = ?, peer_id = ?, peer_fqdn = ?, enrolled_at = ?, updated_at = ? WHERE id = ?`,
			e.CredentialHash, e.PeerID, e.PeerFQDN, ms(e.Now), ms(e.Now), workerID)); err != nil {
			return err
		}
		w, err = scanWorker(tx.QueryRowContext(ctx, `SELECT `+workerCols+` FROM workers WHERE id = ?`, workerID))
		return err
	})
	return w, err
}

// ---- printers ----

const printerCols = `id, name, worker_id, address, backend, paper_size, resolution_dpi, options, status, reachable, reachability_error, reachability_at, created_at, updated_at`

func scanPrinter(sc scanner) (*Printer, error) {
	var p Printer
	var opts string
	var reachable sql.NullBool
	var reachAt sql.NullInt64
	var created, updated int64
	err := sc.Scan(&p.ID, &p.Name, &p.WorkerID, &p.Address, &p.Backend, &p.PaperSize, &p.ResolutionDPI, &opts, &p.Status,
		&reachable, &p.ReachabilityError, &reachAt, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if opts != "" {
		if err := json.Unmarshal([]byte(opts), &p.Options); err != nil {
			return nil, fmt.Errorf("printer %s options: %w", p.ID, err)
		}
	}
	if reachable.Valid {
		v := reachable.Bool
		p.Reachable = &v
	}
	p.ReachabilityAt = fromNullMS(reachAt)
	p.CreatedAt, p.UpdatedAt = fromMS(created), fromMS(updated)
	return &p, nil
}

func encodeOptions(o map[string]string) (string, error) {
	if o == nil {
		return "{}", nil
	}
	b, err := json.Marshal(o)
	return string(b), err
}

func (s *SQLite) CreatePrinter(ctx context.Context, p *Printer) error {
	opts, err := encodeOptions(p.Options)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO printers (`+printerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Name, p.WorkerID, p.Address, p.Backend, p.PaperSize, p.ResolutionDPI, opts, p.Status,
			nil, "", nil, ms(p.CreatedAt), ms(p.UpdatedAt))
		switch {
		case isUniqueViolation(err):
			return ErrExists
		case isFKViolation(err):
			return fmt.Errorf("%w: worker %q does not exist", ErrNotFound, p.WorkerID)
		case err != nil:
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO queues (id, name) VALUES (?, ?)`, p.ID, p.Name); err != nil {
			if isUniqueViolation(err) {
				return ErrExists
			}
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO queue_printers (queue_id, printer_id) VALUES (?, ?)`, p.ID, p.ID)
		return err
	})
}

func (s *SQLite) GetPrinter(ctx context.Context, id string) (*Printer, error) {
	return scanPrinter(s.db.QueryRowContext(ctx, `SELECT `+printerCols+` FROM printers WHERE id = ?`, id))
}

func (s *SQLite) ListPrinters(ctx context.Context, f PrinterFilter) ([]Printer, error) {
	q := `SELECT ` + printerCols + ` FROM printers`
	var args []any
	if f.WorkerID != "" {
		q += ` WHERE worker_id = ?`
		args = append(args, f.WorkerID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Printer
	for rows.Next() {
		p, err := scanPrinter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *SQLite) UpdatePrinter(ctx context.Context, p *Printer) error {
	opts, err := encodeOptions(p.Options)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		err := affected(tx.ExecContext(ctx, `UPDATE printers SET name = ?, worker_id = ?, address = ?, backend = ?, paper_size = ?, resolution_dpi = ?, options = ?, status = ?, updated_at = ? WHERE id = ?`,
			p.Name, p.WorkerID, p.Address, p.Backend, p.PaperSize, p.ResolutionDPI, opts, p.Status, ms(p.UpdatedAt), p.ID))
		if isFKViolation(err) {
			return fmt.Errorf("%w: worker %q does not exist", ErrNotFound, p.WorkerID)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE queues SET name = ? WHERE id = ?`, p.Name, p.ID)
		return err
	})
}

func (s *SQLite) DeletePrinter(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE queue_id = ? AND status IN ('queued','leased','printing')`, id).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return fmt.Errorf("%w: printer has %d unfinished jobs", ErrConflict, active)
		}
		if err := affected(tx.ExecContext(ctx, `DELETE FROM printers WHERE id = ?`, id)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM queues WHERE id = ?`, id)
		return err
	})
}

func (s *SQLite) GetQueue(ctx context.Context, id string) (*Queue, error) {
	q := Queue{}
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM queues WHERE id = ?`, id).Scan(&q.ID, &q.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT printer_id FROM queue_printers WHERE queue_id = ? ORDER BY printer_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		q.PrinterIDs = append(q.PrinterIDs, pid)
	}
	return &q, rows.Err()
}

// ---- app clients ----

func (s *SQLite) loadClientPrinters(ctx context.Context, q queryer, c *AppClient) error {
	rows, err := q.QueryContext(ctx, `SELECT printer_id FROM app_client_printers WHERE app_client_id = ? ORDER BY printer_id`, c.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	c.PrinterIDs = nil
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		c.PrinterIDs = append(c.PrinterIDs, id)
	}
	return rows.Err()
}

func (s *SQLite) setClientPrinters(ctx context.Context, tx *sql.Tx, c *AppClient) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_client_printers WHERE app_client_id = ?`, c.ID); err != nil {
		return err
	}
	for _, pid := range c.PrinterIDs {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO app_client_printers (app_client_id, printer_id) VALUES (?, ?)`, c.ID, pid)
		if isFKViolation(err) {
			return fmt.Errorf("%w: printer %q does not exist", ErrNotFound, pid)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLite) getAppClient(ctx context.Context, where string, arg any) (*AppClient, error) {
	var c AppClient
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, all_printers, created_at, updated_at FROM app_clients WHERE `+where, arg).
		Scan(&c.ID, &c.Name, &c.TokenHash, &c.AllPrinters, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = fromMS(created), fromMS(updated)
	if err := s.loadClientPrinters(ctx, s.db, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *SQLite) CreateAppClient(ctx context.Context, c *AppClient) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO app_clients (id, name, token_hash, all_printers, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
			c.ID, c.Name, c.TokenHash, c.AllPrinters, ms(c.CreatedAt), ms(c.UpdatedAt))
		if isUniqueViolation(err) {
			return ErrExists
		}
		if err != nil {
			return err
		}
		return s.setClientPrinters(ctx, tx, c)
	})
}

func (s *SQLite) GetAppClient(ctx context.Context, id string) (*AppClient, error) {
	return s.getAppClient(ctx, `id = ?`, id)
}

func (s *SQLite) GetAppClientByTokenHash(ctx context.Context, hash string) (*AppClient, error) {
	if hash == "" {
		return nil, ErrNotFound
	}
	return s.getAppClient(ctx, `token_hash = ?`, hash)
}

func (s *SQLite) ListAppClients(ctx context.Context) ([]AppClient, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM app_clients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AppClient, 0, len(ids))
	for _, id := range ids {
		c, err := s.GetAppClient(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *SQLite) UpdateAppClient(ctx context.Context, c *AppClient) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, `UPDATE app_clients SET name = ?, token_hash = ?, all_printers = ?, updated_at = ? WHERE id = ?`,
			c.Name, c.TokenHash, c.AllPrinters, ms(c.UpdatedAt), c.ID)); err != nil {
			return err
		}
		return s.setClientPrinters(ctx, tx, c)
	})
}

func (s *SQLite) DeleteAppClient(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM app_clients WHERE id = ?`, id))
}

// ---- jobs ----

const jobCols = `id, queue_id, printer_id, app_client_id, status, copies, title, content_type, size, document_hash, idempotency_key,
	lease_holder, lease_expires_at, attempts, max_attempts, error, created_at, updated_at, leased_at, started_at, finished_at`

func scanJob(sc scanner) (*Job, error) {
	var j Job
	var leaseExp, leased, started, finished sql.NullInt64
	var created, updated int64
	err := sc.Scan(&j.ID, &j.QueueID, &j.PrinterID, &j.AppClientID, &j.Status, &j.Copies, &j.Title, &j.ContentType, &j.Size,
		&j.DocumentHash, &j.IdempotencyKey, &j.LeaseHolder, &leaseExp, &j.Attempts, &j.MaxAttempts, &j.Error,
		&created, &updated, &leased, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.LeaseExpiresAt = fromNullMS(leaseExp)
	j.LeasedAt, j.StartedAt, j.FinishedAt = fromNullMS(leased), fromNullMS(started), fromNullMS(finished)
	j.CreatedAt, j.UpdatedAt = fromMS(created), fromMS(updated)
	return &j, nil
}

func getJob(ctx context.Context, q queryer, id string) (*Job, error) {
	return scanJob(q.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

func (s *SQLite) CreateJob(ctx context.Context, j *Job) (*Job, bool, error) {
	var out *Job
	created := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if j.IdempotencyKey != "" {
			existing, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE app_client_id = ? AND idempotency_key = ?`,
				j.AppClientID, j.IdempotencyKey))
			if err == nil {
				if existing.QueueID != j.QueueID || existing.DocumentHash != j.DocumentHash || existing.Copies != j.Copies {
					return ErrIdempotencyMismatch
				}
				out = existing
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		var seq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM jobs`).Scan(&seq); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO jobs (seq, `+jobCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			seq, j.ID, j.QueueID, j.PrinterID, j.AppClientID, j.Status, j.Copies, j.Title, j.ContentType, j.Size, j.DocumentHash,
			j.IdempotencyKey, j.LeaseHolder, msPtr(j.LeaseExpiresAt), j.Attempts, j.MaxAttempts, j.Error,
			ms(j.CreatedAt), ms(j.UpdatedAt), msPtr(j.LeasedAt), msPtr(j.StartedAt), msPtr(j.FinishedAt))
		if isUniqueViolation(err) {
			return ErrExists
		}
		if err != nil {
			return err
		}
		created = true
		out, err = getJob(ctx, tx, j.ID)
		return err
	})
	return out, created, err
}

func (s *SQLite) GetJob(ctx context.Context, id string) (*Job, error) {
	return getJob(ctx, s.db, id)
}

func (s *SQLite) JobExists(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

func (s *SQLite) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, `status = ?`)
		args = append(args, f.Status)
	}
	if f.PrinterID != "" {
		where = append(where, `(printer_id = ? OR queue_id = ?)`)
		args = append(args, f.PrinterID, f.PrinterID)
	}
	if f.AppClientID != "" {
		where = append(where, `app_client_id = ?`)
		args = append(args, f.AppClientID)
	}
	if f.WorkerID != "" {
		where = append(where, `lease_holder = ?`)
		args = append(args, f.WorkerID)
	}
	q := `SELECT ` + jobCols + ` FROM jobs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *SQLite) CancelJob(ctx context.Context, id string, now time.Time) (*Job, error) {
	var out *Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.Status != JobQueued {
			return fmt.Errorf("%w: job is %s", ErrConflict, j.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'canceled', lease_expires_at = NULL, updated_at = ?, finished_at = ? WHERE id = ?`,
			ms(now), ms(now), id); err != nil {
			return err
		}
		out, err = getJob(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLite) LeaseJob(ctx context.Context, r LeaseRequest) (*Job, error) {
	var out *Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		q := `
SELECT j.id, p.id FROM jobs j
JOIN queue_printers qp ON qp.queue_id = j.queue_id
JOIN printers p ON p.id = qp.printer_id
WHERE j.status = 'queued' AND p.worker_id = ? AND p.status = 'ready'
  AND NOT EXISTS (SELECT 1 FROM jobs a WHERE a.printer_id = p.id AND a.status IN ('leased','printing'))`
		args := []any{r.WorkerID}
		if len(r.PrinterIDs) > 0 {
			q += ` AND p.id IN (` + placeholders(len(r.PrinterIDs)) + `)`
			for _, id := range r.PrinterIDs {
				args = append(args, id)
			}
		}
		q += ` ORDER BY j.seq LIMIT 1`
		var jobID, printerID string
		err := tx.QueryRowContext(ctx, q, args...).Scan(&jobID, &printerID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		now := ms(r.Now)
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'leased', printer_id = ?, lease_holder = ?, lease_expires_at = ?,
			attempts = attempts + 1, leased_at = ?, updated_at = ? WHERE id = ? AND status = 'queued'`,
			printerID, r.WorkerID, ms(r.Now.Add(r.Duration)), now, now, jobID); err != nil {
			return err
		}
		out, err = getJob(ctx, tx, jobID)
		return err
	})
	return out, err
}

// heldJob loads a job and checks that the worker may report on it.
func heldJob(ctx context.Context, tx *sql.Tx, jobID, workerID string, allowExpired bool) (*Job, error) {
	j, err := getJob(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	if j.LeaseHolder != workerID {
		return nil, fmt.Errorf("%w: job is not leased to this worker", ErrConflict)
	}
	switch j.Status {
	case JobLeased, JobPrinting:
	case JobQueued:
		// The lease expired and the job was requeued. A late report from
		// the last holder is still accepted to avoid printing twice.
		if !allowExpired {
			return nil, fmt.Errorf("%w: lease expired", ErrConflict)
		}
	default:
		return nil, fmt.Errorf("%w: job is %s", ErrConflict, j.Status)
	}
	return j, nil
}

func (s *SQLite) ExtendLease(ctx context.Context, jobID, workerID string, now, until time.Time) (*Job, error) {
	var out *Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := heldJob(ctx, tx, jobID, workerID, false); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'printing', lease_expires_at = ?, started_at = COALESCE(started_at, ?), updated_at = ? WHERE id = ?`,
			ms(until), ms(now), ms(now), jobID); err != nil {
			return err
		}
		var err error
		out, err = getJob(ctx, tx, jobID)
		return err
	})
	return out, err
}

func (s *SQLite) CompleteJob(ctx context.Context, jobID, workerID string, now time.Time) (*Job, error) {
	var out *Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := heldJob(ctx, tx, jobID, workerID, true); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'done', lease_expires_at = NULL, error = '', updated_at = ?, finished_at = ? WHERE id = ?`,
			ms(now), ms(now), jobID); err != nil {
			return err
		}
		var err error
		out, err = getJob(ctx, tx, jobID)
		return err
	})
	return out, err
}

func (s *SQLite) FailJob(ctx context.Context, r FailRequest) (*Job, error) {
	var out *Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, err := heldJob(ctx, tx, r.JobID, r.WorkerID, true)
		if err != nil {
			return err
		}
		now := ms(r.Now)
		if r.Retryable && j.Attempts < j.MaxAttempts {
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', lease_expires_at = NULL, error = ?, updated_at = ? WHERE id = ?`,
				r.Error, now, r.JobID)
		} else {
			msg := r.Error
			if r.Retryable {
				msg = fmt.Sprintf("%s (giving up after %d attempts)", msg, j.Attempts)
			}
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET status = 'failed', lease_expires_at = NULL, error = ?, updated_at = ?, finished_at = ? WHERE id = ?`,
				msg, now, now, r.JobID)
		}
		if err != nil {
			return err
		}
		out, err = getJob(ctx, tx, r.JobID)
		return err
	})
	return out, err
}

func (s *SQLite) ExpireLeases(ctx context.Context, now time.Time) ([]Job, error) {
	var out []Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE status IN ('leased','printing') AND lease_expires_at <= ?`, ms(now))
		if err != nil {
			return err
		}
		var expired []*Job
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, j)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		t := ms(now)
		for _, j := range expired {
			if j.Attempts < j.MaxAttempts {
				_, err = tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', lease_expires_at = NULL, error = 'lease expired', updated_at = ? WHERE id = ?`, t, j.ID)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE jobs SET status = 'failed', lease_expires_at = NULL, error = ?, updated_at = ?, finished_at = ? WHERE id = ?`,
					fmt.Sprintf("lease expired, giving up after %d attempts", j.Attempts), t, t, j.ID)
			}
			if err != nil {
				return err
			}
			updated, err := getJob(ctx, tx, j.ID)
			if err != nil {
				return err
			}
			out = append(out, *updated)
		}
		return nil
	})
	return out, err
}

func (s *SQLite) DeleteFinishedJobs(ctx context.Context, before time.Time) ([]string, error) {
	var ids []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM jobs WHERE status IN ('done','failed','canceled') AND finished_at < ?`, ms(before))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	return ids, err
}

// ---- panel users ----

const panelUserCols = `ip, peer_id, peer_fqdn, name, role, created_at, updated_at, last_seen`

func scanPanelUser(sc scanner) (*PanelUser, error) {
	var u PanelUser
	var created, updated int64
	var seen sql.NullInt64
	err := sc.Scan(&u.IP, &u.PeerID, &u.PeerFQDN, &u.Name, &u.Role, &created, &updated, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt, u.LastSeen = fromMS(created), fromMS(updated), fromNullMS(seen)
	return &u, nil
}

func (s *SQLite) ListPanelUsers(ctx context.Context) ([]PanelUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+panelUserCols+` FROM panel_users ORDER BY ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PanelUser
	for rows.Next() {
		u, err := scanPanelUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *SQLite) GetPanelUser(ctx context.Context, ip string) (*PanelUser, error) {
	return scanPanelUser(s.db.QueryRowContext(ctx, `SELECT `+panelUserCols+` FROM panel_users WHERE ip = ?`, ip))
}

func insertPanelUser(ctx context.Context, q queryer, u *PanelUser) error {
	_, err := q.ExecContext(ctx, `INSERT INTO panel_users (`+panelUserCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		u.IP, u.PeerID, u.PeerFQDN, u.Name, u.Role, ms(u.CreatedAt), ms(u.UpdatedAt), msPtr(u.LastSeen))
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (s *SQLite) CreatePanelUser(ctx context.Context, u *PanelUser) error {
	return insertPanelUser(ctx, s.db, u)
}

func (s *SQLite) ClaimFirstPanelAdmin(ctx context.Context, u *PanelUser) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM panel_users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: the panel already has users", ErrConflict)
		}
		return insertPanelUser(ctx, tx, u)
	})
}

// requireAdmin fails with ErrConflict if the panel has no admin left.
func requireAdmin(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM panel_users WHERE role = 'admin'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: the panel needs at least one admin", ErrConflict)
	}
	return nil
}

func (s *SQLite) UpdatePanelUser(ctx context.Context, u *PanelUser) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, `UPDATE panel_users SET peer_id = ?, peer_fqdn = ?, name = ?, role = ?, updated_at = ? WHERE ip = ?`,
			u.PeerID, u.PeerFQDN, u.Name, u.Role, ms(u.UpdatedAt), u.IP)); err != nil {
			return err
		}
		return requireAdmin(ctx, tx)
	})
}

func (s *SQLite) DeletePanelUser(ctx context.Context, ip string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, `DELETE FROM panel_users WHERE ip = ?`, ip)); err != nil {
			return err
		}
		return requireAdmin(ctx, tx)
	})
}

func (s *SQLite) TouchPanelUser(ctx context.Context, ip string, at time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE panel_users SET last_seen = ? WHERE ip = ?`, ms(at), ip))
}

// ---- proxy keys ----

const proxyKeyCols = `id, name, hash, created_at, last_used, last_peer`

func scanProxyKey(sc scanner) (*ProxyKey, error) {
	var k ProxyKey
	var created int64
	var used sql.NullInt64
	err := sc.Scan(&k.ID, &k.Name, &k.Hash, &created, &used, &k.LastPeer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.CreatedAt, k.LastUsed = fromMS(created), fromNullMS(used)
	return &k, nil
}

func (s *SQLite) ListProxyKeys(ctx context.Context) ([]ProxyKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+proxyKeyCols+` FROM proxy_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProxyKey
	for rows.Next() {
		k, err := scanProxyKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *SQLite) CreateProxyKey(ctx context.Context, k *ProxyKey) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO proxy_keys (`+proxyKeyCols+`) VALUES (?,?,?,?,?,?)`,
		k.ID, k.Name, k.Hash, ms(k.CreatedAt), msPtr(k.LastUsed), k.LastPeer)
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (s *SQLite) GetProxyKeyByHash(ctx context.Context, hash string) (*ProxyKey, error) {
	return scanProxyKey(s.db.QueryRowContext(ctx, `SELECT `+proxyKeyCols+` FROM proxy_keys WHERE hash = ?`, hash))
}

func (s *SQLite) DeleteProxyKey(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM proxy_keys WHERE id = ?`, id))
}

func (s *SQLite) TouchProxyKey(ctx context.Context, id string, at time.Time, peer string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE proxy_keys SET last_used = ?, last_peer = ? WHERE id = ?`, ms(at), peer, id))
}

func (s *SQLite) RetryJob(ctx context.Context, id string, now time.Time) (*Job, error) {
	var out *Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.Status != JobFailed && j.Status != JobCanceled {
			return fmt.Errorf("%w: only failed or canceled jobs can be retried, the job is %s", ErrConflict, j.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', attempts = 0, error = '', lease_expires_at = NULL,
			leased_at = NULL, started_at = NULL, finished_at = NULL, updated_at = ? WHERE id = ?`, ms(now), id); err != nil {
			return err
		}
		out, err = getJob(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLite) DeleteJob(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.Status == JobLeased || j.Status == JobPrinting {
			return fmt.Errorf("%w: the job is %s", ErrConflict, j.Status)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
		return err
	})
}

// ---- settings ----

func (s *SQLite) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *SQLite) SetSetting(ctx context.Context, key, value string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, ms(at))
	return err
}

func (s *SQLite) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}
