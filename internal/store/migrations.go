package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order. Never edit a released migration; append
// a new one instead. Timestamps are stored as Unix milliseconds.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE workers (
	id               TEXT PRIMARY KEY,
	name             TEXT NOT NULL,
	location         TEXT NOT NULL DEFAULT '',
	credential_hash  TEXT NOT NULL DEFAULT '',
	peer_id          TEXT NOT NULL DEFAULT '',
	peer_fqdn        TEXT NOT NULL DEFAULT '',
	version          TEXT NOT NULL DEFAULT '',
	last_heartbeat   INTEGER,
	enrolled_at      INTEGER,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
);
CREATE INDEX workers_credential ON workers(credential_hash) WHERE credential_hash <> '';

CREATE TABLE enrollment_tokens (
	id          TEXT PRIMARY KEY,
	worker_id   TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
	token_hash  TEXT NOT NULL UNIQUE,
	expires_at  INTEGER NOT NULL,
	used_at     INTEGER,
	created_at  INTEGER NOT NULL
);

CREATE TABLE printers (
	id                  TEXT PRIMARY KEY,
	name                TEXT NOT NULL,
	worker_id           TEXT NOT NULL REFERENCES workers(id),
	address             TEXT NOT NULL,
	backend             TEXT NOT NULL,
	paper_size          TEXT NOT NULL,
	resolution_dpi      INTEGER NOT NULL,
	options             TEXT NOT NULL DEFAULT '{}',
	status              TEXT NOT NULL,
	reachable           INTEGER,
	reachability_error  TEXT NOT NULL DEFAULT '',
	reachability_at     INTEGER,
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL
);
CREATE INDEX printers_worker ON printers(worker_id);

CREATE TABLE queues (
	id    TEXT PRIMARY KEY,
	name  TEXT NOT NULL
);

CREATE TABLE queue_printers (
	queue_id    TEXT NOT NULL REFERENCES queues(id) ON DELETE CASCADE,
	printer_id  TEXT NOT NULL REFERENCES printers(id) ON DELETE CASCADE,
	PRIMARY KEY (queue_id, printer_id)
);
CREATE INDEX queue_printers_printer ON queue_printers(printer_id);

CREATE TABLE app_clients (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	token_hash    TEXT NOT NULL UNIQUE,
	all_printers  INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);

CREATE TABLE app_client_printers (
	app_client_id  TEXT NOT NULL REFERENCES app_clients(id) ON DELETE CASCADE,
	printer_id     TEXT NOT NULL REFERENCES printers(id) ON DELETE CASCADE,
	PRIMARY KEY (app_client_id, printer_id)
);

CREATE TABLE jobs (
	id                TEXT PRIMARY KEY,
	seq               INTEGER NOT NULL,
	queue_id          TEXT NOT NULL,
	printer_id        TEXT NOT NULL DEFAULT '',
	app_client_id     TEXT NOT NULL DEFAULT '',
	status            TEXT NOT NULL,
	copies            INTEGER NOT NULL,
	title             TEXT NOT NULL DEFAULT '',
	content_type      TEXT NOT NULL,
	size              INTEGER NOT NULL,
	document_hash     TEXT NOT NULL,
	idempotency_key   TEXT NOT NULL DEFAULT '',
	lease_holder      TEXT NOT NULL DEFAULT '',
	lease_expires_at  INTEGER,
	attempts          INTEGER NOT NULL DEFAULT 0,
	max_attempts      INTEGER NOT NULL,
	error             TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL,
	leased_at         INTEGER,
	started_at        INTEGER,
	finished_at       INTEGER
);
CREATE UNIQUE INDEX jobs_idempotency ON jobs(app_client_id, idempotency_key) WHERE idempotency_key <> '';
CREATE INDEX jobs_queue_status ON jobs(queue_id, status, seq);
CREATE INDEX jobs_printer_status ON jobs(printer_id, status);
CREATE INDEX jobs_status_lease ON jobs(status, lease_expires_at);
CREATE INDEX jobs_finished ON jobs(finished_at) WHERE finished_at IS NOT NULL;
`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, v); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", v, err)
		}
	}
	return nil
}
