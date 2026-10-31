package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Jobs received from the local queues wait in the spool until the server
// accepted them, so printing works while the server is briefly unreachable.

// SpoolJob is a job in the spool.
type SpoolJob struct {
	ID          string    `json:"id"`
	PrinterID   string    `json:"printer_id"`
	Title       string    `json:"title"`
	Copies      int       `json:"copies"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
	// Failed is set once the job will not be sent again.
	Failed bool `json:"failed"`
}

// Spool is a directory of jobs: <id>.json and <id>.doc.
type Spool struct {
	dir string
	mu  sync.Mutex
}

func OpenSpool(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Spool{dir: dir}, nil
}

func (s *Spool) Dir() string                   { return s.dir }
func (s *Spool) DocumentPath(id string) string { return filepath.Join(s.dir, id+".doc") }
func (s *Spool) metaPath(id string) string     { return filepath.Join(s.dir, id+".json") }

// Add moves the document into the spool and records the job.
func (s *Spool) Add(printerID, title string, copies int, document string, now time.Time) (SpoolJob, error) {
	b := make([]byte, 12)
	rand.Read(b)
	job := SpoolJob{ID: hex.EncodeToString(b), PrinterID: printerID, Title: title, Copies: max(copies, 1),
		ContentType: ContentType(document), CreatedAt: now, NextAttempt: now}
	if err := os.Rename(document, s.DocumentPath(job.ID)); err != nil {
		return SpoolJob{}, err
	}
	return job, s.Save(job)
}

func (s *Spool) Save(job SpoolJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := s.metaPath(job.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(job.ID))
}

func (s *Spool) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(s.metaPath(id))
	os.Remove(s.DocumentPath(id))
}

// Jobs returns all jobs, oldest first.
func (s *Spool) Jobs() []SpoolJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _ := os.ReadDir(s.dir)
	var jobs []SpoolJob
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		var j SpoolJob
		if err == nil && json.Unmarshal(data, &j) == nil {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].CreatedAt.Before(jobs[b].CreatedAt) })
	return jobs
}

// ForwardEvent tells what happened to a job.
type ForwardEvent struct {
	Job      SpoolJob
	Kind     string // "sent", "waiting" (first failed attempt), "failed"
	RemoteID string
}

// Forwarder sends spooled jobs to the server.
type Forwarder struct {
	Spool   *Spool
	OnEvent func(ForwardEvent)
	// MaxAge is how long a job is retried; default 24 hours.
	MaxAge time.Duration

	mu      sync.Mutex
	client  *Client
	running sync.Mutex
}

func (f *Forwarder) SetClient(c *Client) {
	f.mu.Lock()
	f.client = c
	f.mu.Unlock()
}

// RetryNow makes all waiting jobs due.
func (f *Forwarder) RetryNow(now time.Time) {
	for _, j := range f.Spool.Jobs() {
		if !j.Failed {
			j.NextAttempt = now
			f.Spool.Save(j)
		}
	}
}

// Run sends the due jobs. Concurrent calls return at once.
func (f *Forwarder) Run(ctx context.Context, now time.Time) {
	if !f.running.TryLock() {
		return
	}
	defer f.running.Unlock()
	f.mu.Lock()
	c := f.client
	f.mu.Unlock()
	if c == nil {
		return
	}
	maxAge := f.MaxAge
	if maxAge == 0 {
		maxAge = 24 * time.Hour
	}
	for _, j := range f.Spool.Jobs() {
		if j.Failed || j.NextAttempt.After(now) {
			continue
		}
		remote, err := c.Submit(ctx, j.PrinterID, j.Title, j.Copies, f.Spool.DocumentPath(j.ID), j.ContentType, "desktop-"+j.ID)
		if err == nil {
			f.Spool.Remove(j.ID)
			f.emit(ForwardEvent{Job: j, Kind: "sent", RemoteID: remote.ID})
			continue
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		j.Attempts++
		j.LastError = err.Error()
		switch {
		case !Retryable(err) || now.Sub(j.CreatedAt) > maxAge:
			j.Failed = true
			f.emit(ForwardEvent{Job: j, Kind: "failed"})
		default:
			j.NextAttempt = now.Add(Backoff(j.Attempts))
			if j.Attempts == 1 {
				f.emit(ForwardEvent{Job: j, Kind: "waiting"})
			}
		}
		f.Spool.Save(j)
	}
}

func (f *Forwarder) emit(e ForwardEvent) {
	if f.OnEvent != nil {
		f.OnEvent(e)
	}
}

// Backoff is the wait before the next attempt: 5 s, doubling, at most 5 min.
func Backoff(attempts int) time.Duration {
	d := 5 * time.Second * time.Duration(math.Pow(2, float64(min(max(attempts, 1), 10)-1)))
	return min(d, 5*time.Minute)
}
