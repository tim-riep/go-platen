// Package spool stores print documents as files. Only metadata lives in the
// database; the document of a job is stored under the job's ID.
package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ErrTooLarge is returned when a document exceeds the size limit.
var ErrTooLarge = errors.New("document too large")

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Spool is a directory of documents.
type Spool struct {
	dir string
}

// Open creates the spool directory if needed.
func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	return &Spool{dir: dir}, nil
}

func (s *Spool) path(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("invalid document id %q", id)
	}
	return filepath.Join(s.dir, id), nil
}

// Write stores the document read from r under id. It fails with ErrTooLarge
// if r yields more than limit bytes (limit <= 0 means no limit). It returns
// the size and the hex encoded SHA-256 of the document.
func (s *Spool) Write(id string, r io.Reader, limit int64) (size int64, hash string, err error) {
	p, err := s.path(id)
	if err != nil {
		return 0, "", err
	}
	tmp, err := os.CreateTemp(s.dir, ".incoming-*")
	if err != nil {
		return 0, "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	src := r
	if limit > 0 {
		src = io.LimitReader(r, limit+1)
	}
	size, err = io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return 0, "", err
	}
	if limit > 0 && size > limit {
		return 0, "", ErrTooLarge
	}
	if err = tmp.Sync(); err != nil {
		return 0, "", err
	}
	if err = tmp.Close(); err != nil {
		return 0, "", err
	}
	if err = os.Rename(tmp.Name(), p); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(h.Sum(nil)), nil
}

// Open opens the document stored under id.
func (s *Spool) Open(id string) (*os.File, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// Remove deletes the document stored under id. A missing document is not an
// error.
func (s *Spool) Remove(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Entry is a document in the spool.
type Entry struct {
	ID      string
	ModTime time.Time
}

// List returns all documents, including leftovers of interrupted uploads
// (with IDs starting with ".incoming-").
func (s *Spool) List() ([]Entry, error) {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		if !de.Type().IsRegular() {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{ID: de.Name(), ModTime: info.ModTime()})
	}
	return out, nil
}

// RemoveIncoming deletes a leftover of an interrupted upload.
func (s *Spool) RemoveIncoming(name string) error {
	if filepath.Base(name) != name || len(name) < 10 || name[:10] != ".incoming-" {
		return fmt.Errorf("not an incoming file: %q", name)
	}
	return os.Remove(filepath.Join(s.dir, name))
}
