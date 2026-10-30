package raster

import (
	"fmt"
	"io"

	"github.com/tim-riep/go-platen/internal/render"
)

// PDFToRasterName is the registry name of the plain raster backend.
const PDFToRasterName = "pdf-to-raster"

// PDFToRaster renders PDFs into 1-bit bitmaps and writes them as a sequence
// of binary PBM images (Netpbm "P4", one per page). It is printer-neutral:
// useful for printers or tools that take PBM and for inspecting the
// rendering, and it is the reference output for the raster encoders.
var PDFToRaster = NewFactory(PDFToRasterName, func() Encoder { return PBM{} })

// PBM writes Netpbm P4 images. In P4 a set bit is black, like Bitmap.
type PBM struct{}

func (PBM) Begin(io.Writer, Job) error { return nil }

func (PBM) Page(out io.Writer, bm *render.Bitmap) error {
	if _, err := fmt.Fprintf(out, "P4\n# dpi=%d\n%d %d\n", bm.DPI, bm.Width, bm.Height); err != nil {
		return err
	}
	_, err := out.Write(bm.Data)
	return err
}

func (PBM) End(io.Writer) error { return nil }
