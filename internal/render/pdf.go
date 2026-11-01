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
	if o.Threshold == 0 {
		o.Threshold = 128
	}
	return r.RenderImages(ctx, data, o, false, func(index int, img image.Image) error {
		return page(index, ToBitmap(img.(*image.Gray), o.Dither, o.Threshold, o.DPI))
	})
}

// RenderImages is Render without the reduction to 1 bit: page receives an
// *image.Gray, or with color an *image.RGBA, of the paper size at o.DPI
// on a white background. Dither and Threshold are ignored.
func (r *PDFRenderer) RenderImages(ctx context.Context, data []byte, o PageOptions, color bool, page func(index int, img image.Image) error) error {
	if o.DPI <= 0 {
		return errors.New("render: dpi must be positive")
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
		img, err := renderPage(inst, doc.Document, i, o, color)
		if err != nil {
			return fmt.Errorf("page %d: %w", i+1, err)
		}
		if err := page(i, img); err != nil {
			return err
		}
	}
	return nil
}

// renderPage renders a page into an *image.Gray, or with color an
// *image.RGBA, of the paper size.
func renderPage(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, index int, o PageOptions, color bool) (image.Image, error) {
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
	format := requests.RenderImageFormatGrayscale
	if color {
		format = requests.RenderImageFormatRGBA
	}
	// The page is scaled to fit the paper, keeping its aspect ratio.
	res, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page: page, Width: boxW, Height: boxH,
		RenderFlags: enums.FPDF_RENDER_FLAG_PRINTING | enums.FPDF_RENDER_FLAG_ANNOT,
		ImageFormat: format,
	})
	if err != nil {
		return nil, err
	}
	defer res.Cleanup()
	var src *pixels
	switch img := res.Result.RenderedImage.(type) {
	case *image.Gray:
		src = &pixels{pix: img.Pix, stride: img.Stride, bpp: 1, w: img.Bounds().Dx(), h: img.Bounds().Dy()}
	case *image.RGBA:
		onWhite(img)
		src = &pixels{pix: img.Pix, stride: img.Stride, bpp: 4, w: img.Bounds().Dx(), h: img.Bounds().Dy()}
	default:
		return nil, fmt.Errorf("unexpected image type %T", res.Result.RenderedImage)
	}
	canvas := &pixels{bpp: src.bpp, w: boxW, h: boxH, stride: boxW * src.bpp}
	canvas.pix = make([]byte, canvas.stride*boxH)
	for i := range canvas.pix {
		canvas.pix[i] = 0xff
	}
	offX, offY := (boxW-src.w)/2, (boxH-src.h)/2
	for y := 0; y < src.h && y+offY < boxH; y++ {
		if y+offY < 0 {
			continue
		}
		srow := src.pix[y*src.stride : y*src.stride+src.w*src.bpp]
		dst := canvas.pix[(y+offY)*canvas.stride+max(offX, 0)*src.bpp:]
		copy(dst[:min(len(srow), (boxW-max(offX, 0))*src.bpp)], srow)
	}
	if rotate {
		canvas = canvas.rotateClockwise()
	}
	return canvas.image(), nil
}

// onWhite composes the premultiplied RGBA image onto white paper.
func onWhite(img *image.RGBA) {
	for i := 0; i+3 < len(img.Pix); i += 4 {
		a := img.Pix[i+3]
		if a == 0xff {
			continue
		}
		w := 0xff - a
		img.Pix[i] += w
		img.Pix[i+1] += w
		img.Pix[i+2] += w
		img.Pix[i+3] = 0xff
	}
}

// pixels is an image with 1 (gray) or 4 (RGBA) bytes per pixel.
type pixels struct {
	pix         []byte
	stride, bpp int
	w, h        int
}

func (p *pixels) image() image.Image {
	r := image.Rect(0, 0, p.w, p.h)
	if p.bpp == 1 {
		return &image.Gray{Pix: p.pix, Stride: p.stride, Rect: r}
	}
	return &image.RGBA{Pix: p.pix, Stride: p.stride, Rect: r}
}

// rotateClockwise turns a landscape image into portrait orientation.
func (p *pixels) rotateClockwise() *pixels {
	out := &pixels{bpp: p.bpp, w: p.h, h: p.w, stride: p.h * p.bpp}
	out.pix = make([]byte, out.stride*out.h)
	for y := 0; y < p.h; y++ {
		row := p.pix[y*p.stride : y*p.stride+p.w*p.bpp]
		if p.bpp == 1 {
			for x, v := range row {
				out.pix[x*out.stride+p.h-1-y] = v
			}
			continue
		}
		for x := 0; x < p.w; x++ {
			// (x, y) -> (h-1-y, x)
			copy(out.pix[x*out.stride+(p.h-1-y)*p.bpp:][:p.bpp], row[x*p.bpp:(x+1)*p.bpp])
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
