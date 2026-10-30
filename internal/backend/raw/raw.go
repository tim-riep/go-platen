// Package raw implements the pass-through backend: the document is sent to
// the printer unchanged, e.g. ZPL to label printers.
package raw

import (
	"context"
	"fmt"
	"io"

	"github.com/tim-riep/go-platen/internal/backend"
)

// Name is the registry name of the backend.
const Name = "raw"

// Factory registers the raw backend.
var Factory = backend.Factory{
	Name:         Name,
	ContentTypes: []string{"*/*"},
	New:          func() (backend.Backend, error) { return Backend{}, nil },
}

// Backend passes documents through.
type Backend struct{}

// Accepts accepts every content type.
func (Backend) Accepts(string) bool { return true }

// Print copies doc to out once per copy. For more than one copy doc must be
// an io.Seeker (the worker passes an *os.File); the printer language
// usually cannot express copies on its own for raw data.
func (Backend) Print(ctx context.Context, doc io.Reader, opts backend.JobOptions, out io.Writer) error {
	copies := max(opts.Copies, 1)
	for i := 0; i < copies; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 {
			s, ok := doc.(io.Seeker)
			if !ok {
				return fmt.Errorf("raw: %d copies need a seekable document", copies)
			}
			if _, err := s.Seek(0, io.SeekStart); err != nil {
				return err
			}
		}
		if _, err := io.Copy(out, doc); err != nil {
			return fmt.Errorf("raw: %w", err)
		}
	}
	return nil
}
