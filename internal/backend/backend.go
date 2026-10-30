// Package backend defines how workers turn a document into the data stream
// a printer understands, and a registry of the available backends.
//
// A backend writes to an io.Writer and never talks to the printer itself,
// so every backend can be tested without hardware. New backends (e.g. PCL
// or PostScript) are added by implementing Backend and registering a
// Factory; nothing else needs to change.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// JobOptions are the parameters of one print job.
type JobOptions struct {
	JobID       string
	Title       string
	ContentType string
	Copies      int
	// PaperSize is a name such as "A4" or "Letter".
	PaperSize     string
	ResolutionDPI int
	// Options are backend specific printer options from the server.
	Options map[string]string
}

// Backend renders documents for a type of printer.
type Backend interface {
	// Accepts reports whether the backend can print the content type.
	Accepts(contentType string) bool
	// Print renders doc and writes the printer data stream to out.
	Print(ctx context.Context, doc io.Reader, opts JobOptions, out io.Writer) error
}

// Factory describes a backend.
type Factory struct {
	Name string
	// ContentTypes lists the accepted content types; "*/*" accepts all.
	ContentTypes []string
	// New creates the backend. It is called at most once per registry.
	New func() (Backend, error)
}

// Registry holds the available backends.
type Registry struct {
	mu        sync.Mutex
	factories map[string]Factory
	instances map[string]Backend
}

// NewRegistry returns a registry with the given backends.
func NewRegistry(factories ...Factory) *Registry {
	r := &Registry{factories: map[string]Factory{}, instances: map[string]Backend{}}
	for _, f := range factories {
		r.Register(f)
	}
	return r
}

// Register adds a backend. It panics on duplicate names, which is a
// programming error.
func (r *Registry) Register(f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[f.Name]; ok {
		panic("backend: duplicate backend " + f.Name)
	}
	r.factories[f.Name] = f
}

// Names returns the names of all backends.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ContentTypes returns the content types the backend accepts.
func (r *Registry) ContentTypes(name string) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.factories[name]
	if !ok {
		return nil, false
	}
	return append([]string(nil), f.ContentTypes...), true
}

// Get returns the backend, creating it on first use.
func (r *Registry) Get(name string) (Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.instances[name]; ok {
		return b, nil
	}
	f, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", name)
	}
	b, err := f.New()
	if err != nil {
		return nil, fmt.Errorf("init backend %s: %w", name, err)
	}
	r.instances[name] = b
	return b, nil
}

// Close closes backends that implement io.Closer.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for n, b := range r.instances {
		if c, ok := b.(io.Closer); ok {
			errs = append(errs, c.Close())
		}
		delete(r.instances, n)
	}
	return errors.Join(errs...)
}

// retryable marks errors worth retrying with the same document.
type retryable struct{ err error }

func (e retryable) Error() string { return e.err.Error() }
func (e retryable) Unwrap() error { return e.err }

// Retryable marks err as transient. Errors returned by Print are treated as
// permanent (the document cannot be printed) unless marked.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryable{err}
}

// IsRetryable reports whether err was marked with Retryable.
func IsRetryable(err error) bool {
	var r retryable
	return errors.As(err, &r)
}
