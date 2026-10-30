package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is the local state of a job in the journal.
type State string

const (
	// StateSent is written before any data goes to the printer. Finding
	// it after a restart means the job may or may not have printed.
	StateSent State = "sent"
	// StateUncertain is a StateSent entry found after a restart.
	StateUncertain State = "uncertain"
	// StatePrinted means the printer accepted all data; the completion
	// report is pending.
	StatePrinted State = "printed"
	// StateFailed means the job failed; the failure report is pending.
	StateFailed State = "failed"
)

// Entry is one journaled job.
type Entry struct {
	JobID     string    `json:"job_id"`
	PrinterID string    `json:"printer_id"`
	State     State     `json:"state"`
	Error     string    `json:"error,omitempty"`
	Retryable bool      `json:"retryable,omitempty"`
	Uncertain bool      `json:"uncertain,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// pendingReport reports whether the entry waits for a report to the server.
func (e *Entry) pendingReport() bool {
	return e.State == StateUncertain || e.State == StatePrinted || e.State == StateFailed
}

// Journal is a small crash-safe store of job states, one JSON file per job.
// Entries are removed once the server acknowledged the job's outcome.
type Journal struct {
	dir string
	mu  sync.Mutex
}

var journalID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// OpenJournal opens the journal directory and turns entries left in
// StateSent by a previous process into StateUncertain.
func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	j := &Journal{dir: dir}
	entries, err := j.List()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.State == StateSent {
			e.State = StateUncertain
			e.Uncertain = true
			e.Error = "the worker stopped while sending the job to the printer"
			if err := j.Put(e); err != nil {
				return nil, err
			}
		}
	}
	return j, nil
}

func (j *Journal) path(id string) (string, error) {
	if !journalID.MatchString(id) {
		return "", fmt.Errorf("invalid job id %q", id)
	}
	return filepath.Join(j.dir, id+".json"), nil
}

// Put writes the entry durably (write to temporary file, fsync, rename,
// fsync directory).
func (j *Journal) Put(e Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	p, err := j.path(e.JobID)
	if err != nil {
		return err
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(j.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
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
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	return syncDir(j.dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Not all platforms support syncing directories.
	_ = d.Sync()
	return nil
}

// Get returns the entry of a job, or nil.
func (j *Journal) Get(id string) (*Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	p, err := j.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("journal entry %s: %w", id, err)
	}
	return &e, nil
}

// Remove deletes the entry of a job.
func (j *Journal) Remove(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	p, err := j.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(j.dir)
}

// List returns all entries, oldest first.
func (j *Journal) List() ([]Entry, error) {
	des, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		name := de.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		e, err := j.Get(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].UpdatedAt.Before(out[b].UpdatedAt) })
	return out, nil
}
