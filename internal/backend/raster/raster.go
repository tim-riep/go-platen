// Package raster provides backends that rasterize PDF documents into 1-bit
// bitmaps at printer resolution and pass the pages to a page encoder that
// produces the printer's data stream. Printer languages that print bitmaps
// (Brother raster, later PCL raster) only implement an Encoder.
package raster

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/tim-riep/go-platen/internal/backend"
	"github.com/tim-riep/go-platen/internal/render"
)

// ContentTypePDF is the only input format of raster backends.
const ContentTypePDF = "application/pdf"

// maxDocument bounds the PDF size held in memory while rendering.
const maxDocument = 512 << 20

// Encoder writes bitmaps in a printer language.
type Encoder interface {
	// Begin starts a job; it is called once before the first page.
	Begin(out io.Writer, job Job) error
	// Page writes one page.
	Page(out io.Writer, bm *render.Bitmap) error
	// End finishes the job.
	End(out io.Writer) error
}

// Job describes a raster job for the encoder.
type Job struct {
	backend.JobOptions
	Paper render.Paper
	// Copies is handled by the encoder if it reports CopiesInStream,
	// otherwise the backend repeats the pages.
	Copies int
}

// CopiesInStream is implemented by encoders whose printer language can
// express the number of copies itself.
type CopiesInStream interface {
	EncodesCopies() bool
}

// renderer is shared by all raster backends: PDFium is initialized once.
var (
	rendererOnce sync.Once
	rendererInst *render.PDFRenderer
	rendererErr  error
)

// SharedRenderer returns the PDF renderer shared by all backends that
// render PDFs.
func SharedRenderer() (*render.PDFRenderer, error) { return sharedRenderer() }

func sharedRenderer() (*render.PDFRenderer, error) {
	rendererOnce.Do(func() { rendererInst, rendererErr = render.NewPDFRenderer() })
	return rendererInst, rendererErr
}

// Backend is a raster backend.
type Backend struct {
	newEncoder func() Encoder
	defaults   map[string]string
	renderer   *render.PDFRenderer
}

// NewFactory returns a backend factory that renders PDFs and encodes them
// with the encoder returned by newEncoder (one encoder per job).
func NewFactory(name string, newEncoder func() Encoder) backend.Factory {
	return NewFactoryWithDefaults(name, newEncoder, nil)
}

// NewFactoryWithDefaults is NewFactory with default printer options, used
// where a printer option is not set, e.g. the dithering method that suits
// the printer.
func NewFactoryWithDefaults(name string, newEncoder func() Encoder, defaults map[string]string) backend.Factory {
	return backend.Factory{
		Name:         name,
		ContentTypes: []string{ContentTypePDF},
		New: func() (backend.Backend, error) {
			r, err := sharedRenderer()
			if err != nil {
				return nil, err
			}
			return &Backend{newEncoder: newEncoder, defaults: defaults, renderer: r}, nil
		},
	}
}

// Accepts accepts PDF documents.
func (b *Backend) Accepts(contentType string) bool { return contentType == ContentTypePDF }

// PageOptions derives the rendering options from the job options. Printer
// options: "dither" (threshold, halftone, floyd-steinberg), "threshold" (1-255) and
// "rotate" (auto, none).
func PageOptions(opts backend.JobOptions) (render.PageOptions, error) {
	paper, err := render.ParsePaper(opts.PaperSize)
	if err != nil {
		return render.PageOptions{}, err
	}
	dpi := opts.ResolutionDPI
	if dpi <= 0 {
		dpi = 600
	}
	dither, err := render.ParseDither(opts.Options["dither"])
	if err != nil {
		return render.PageOptions{}, err
	}
	threshold := 128
	if v := opts.Options["threshold"]; v != "" {
		threshold, err = strconv.Atoi(v)
		if err != nil || threshold < 1 || threshold > 255 {
			return render.PageOptions{}, fmt.Errorf("threshold must be between 1 and 255")
		}
	}
	rotate := true
	switch opts.Options["rotate"] {
	case "", "auto":
	case "none":
		rotate = false
	default:
		return render.PageOptions{}, fmt.Errorf("rotate must be auto or none")
	}
	return render.PageOptions{Paper: paper, DPI: dpi, Dither: dither, Threshold: uint8(threshold), AutoRotate: rotate}, nil
}

// Print renders the PDF and encodes every page.
func (b *Backend) Print(ctx context.Context, doc io.Reader, opts backend.JobOptions, out io.Writer) error {
	if len(b.defaults) > 0 {
		merged := make(map[string]string, len(b.defaults)+len(opts.Options))
		for k, v := range b.defaults {
			merged[k] = v
		}
		for k, v := range opts.Options {
			merged[k] = v
		}
		opts.Options = merged
	}
	po, err := PageOptions(opts)
	if err != nil {
		return err
	}
	data, err := render.ReadAllLimited(doc, maxDocument)
	if err != nil {
		return err
	}
	enc := b.newEncoder()
	copies := max(opts.Copies, 1)
	inStream := false
	if c, ok := enc.(CopiesInStream); ok && c.EncodesCopies() {
		inStream = true
	}
	job := Job{JobOptions: opts, Paper: po.Paper, Copies: copies}
	if err := enc.Begin(out, job); err != nil {
		return err
	}
	passes := copies
	if inStream {
		passes = 1
	}
	// Copies are collated: the whole document is rendered once per copy
	// rather than keeping all pages in memory.
	for c := 0; c < passes; c++ {
		err := b.renderer.Render(ctx, data, po, func(_ int, bm *render.Bitmap) error {
			return enc.Page(out, bm)
		})
		if err != nil {
			return err
		}
	}
	return enc.End(out)
}

// Close releases nothing; the shared renderer lives for the process.
func (b *Backend) Close() error { return nil }
