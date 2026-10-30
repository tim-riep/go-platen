package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"sync"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// PageOptions control how PDF pages are rasterized.
type PageOptions struct {
	Paper     Paper
	DPI       int
	Dither    Dither
	Threshold uint8
	// AutoRotate turns landscape pages by 90 degrees to fit portrait
	// paper.
	AutoRotate bool
}

// PDFRenderer renders PDF documents with PDFium compiled to WebAssembly
// (run by wazero), so no cgo is required. PDFium runs in a sandbox without
// file system access; documents are passed in memory.
type PDFRenderer struct {
	mu   sync.Mutex
	pool pdfium.Pool
}

// NewPDFRenderer initializes PDFium. Initialization compiles the
// WebAssembly module and takes a moment, so renderers are long-lived.
func NewPDFRenderer() (*PDFRenderer, error) {
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle: 1, MaxIdle: 1, MaxTotal: 1,
		// No mounts: PDFium gets no access to the file system.
		FSConfig: wazero.NewFSConfig(),
	})
	if err != nil {
		return nil, fmt.Errorf("init pdfium: %w", err)
	}
	return &PDFRenderer{pool: pool}, nil
}

// Close releases PDFium.
func (r *PDFRenderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pool == nil {
		return nil
	}
	err := r.pool.Close()
	r.pool = nil
	return err
}

// Render rasterizes every page of the PDF in data and calls page for each
// one in order. Pages are rendered one at a time to bound memory use.
func (r *PDFRenderer) Render(ctx context.Context, data []byte, o PageOptions, page func(index int, bm *Bitmap) error) error {
	if o.DPI <= 0 {
		return errors.New("render: dpi must be positive")
	}
	if o.Threshold == 0 {
		o.Threshold = 128
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pool == nil {
		return errors.New("render: renderer closed")
	}
	inst, err := r.pool.GetInstanceWithContext(ctx)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	defer inst.Close()

	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return fmt.Errorf("open pdf: %w", err)
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})

	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return fmt.Errorf("pdf page count: %w", err)
	}
	if count.PageCount == 0 {
		return errors.New("pdf has no pages")
	}
	for i := 0; i < count.PageCount; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		bm, err := renderPage(inst, doc.Document, i, o)
		if err != nil {
			return fmt.Errorf("page %d: %w", i+1, err)
		}
		if err := page(i, bm); err != nil {
			return err
		}
	}
	return nil
}

func renderPage(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, index int, o PageOptions) (*Bitmap, error) {
	page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: index}}
	size, err := inst.GetPageSize(&requests.GetPageSize{Page: page})
	if err != nil {
		return nil, err
	}
	paperW, paperH := o.Paper.Pixels(o.DPI)
	rotate := o.AutoRotate && (size.Width > size.Height) != (o.Paper.Width > o.Paper.Height)
	boxW, boxH := paperW, paperH
	if rotate {
		boxW, boxH = paperH, paperW
	}
	// The page is scaled to fit the paper, keeping its aspect ratio.
	res, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page: page, Width: boxW, Height: boxH,
		RenderFlags: enums.FPDF_RENDER_FLAG_PRINTING | enums.FPDF_RENDER_FLAG_ANNOT,
		ImageFormat: requests.RenderImageFormatGrayscale,
	})
	if err != nil {
		return nil, err
	}
	defer res.Cleanup()
	src, ok := res.Result.RenderedImage.(*image.Gray)
	if !ok {
		return nil, fmt.Errorf("unexpected image type %T", res.Result.RenderedImage)
	}
	canvas := image.NewGray(image.Rect(0, 0, boxW, boxH))
	for i := range canvas.Pix {
		canvas.Pix[i] = 0xff
	}
	sb := src.Bounds()
	offX, offY := (boxW-sb.Dx())/2, (boxH-sb.Dy())/2
	for y := 0; y < sb.Dy() && y+offY < boxH; y++ {
		if y+offY < 0 {
			continue
		}
		srow := src.Pix[y*src.Stride : y*src.Stride+sb.Dx()]
		dst := canvas.Pix[(y+offY)*canvas.Stride+max(offX, 0):]
		copy(dst[:min(len(srow), boxW-max(offX, 0))], srow)
	}
	if rotate {
		canvas = rotateClockwise(canvas)
	}
	return ToBitmap(canvas, o.Dither, o.Threshold, o.DPI), nil
}

// rotateClockwise turns a landscape image into portrait orientation.
func rotateClockwise(g *image.Gray) *image.Gray {
	b := g.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewGray(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		row := g.Pix[y*g.Stride : y*g.Stride+w]
		for x, v := range row {
			// (x, y) -> (h-1-y, x)
			out.Pix[x*out.Stride+h-1-y] = v
		}
	}
	return out
}

// ReadAllLimited reads a document into memory, failing above limit bytes.
func ReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("document larger than %d bytes", limit)
	}
	return b, nil
}
