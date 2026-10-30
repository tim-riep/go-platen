package raster

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/tim-riep/go-platen/internal/backend"
	"github.com/tim-riep/go-platen/internal/render"
)

// readPBM parses a sequence of P4 images.
func readPBM(t *testing.T, data []byte) []*render.Bitmap {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(data))
	var out []*render.Bitmap
	for {
		var magic string
		if _, err := fmt.Fscan(r, &magic); err == io.EOF {
			return out
		} else if err != nil || magic != "P4" {
			t.Fatalf("bad magic %q: %v", magic, err)
		}
		r.ReadString('\n')
		var dpi, w, h int
		if _, err := fmt.Fscanf(r, "# dpi=%d\n%d %d\n", &dpi, &w, &h); err != nil {
			t.Fatal(err)
		}
		bm := render.NewBitmap(w, h, dpi)
		if _, err := io.ReadFull(r, bm.Data); err != nil {
			t.Fatal(err)
		}
		out = append(out, bm)
	}
}

func newBackend(t *testing.T) backend.Backend {
	t.Helper()
	b, err := PDFToRaster.New()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPDFToRaster(t *testing.T) {
	b := newBackend(t)
	if !b.Accepts("application/pdf") || b.Accepts("text/plain") {
		t.Fatal("content types")
	}
	pdf := makePDF(595.276, 841.890, "0 0 0 rg 0 0 100 100 re f", "")
	var out bytes.Buffer
	err := b.Print(context.Background(), bytes.NewReader(pdf), backend.JobOptions{
		Copies: 2, PaperSize: "A4", ResolutionDPI: 100, Options: map[string]string{"dither": "floyd-steinberg"},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	pages := readPBM(t, out.Bytes())
	if len(pages) != 4 {
		t.Fatalf("%d pages, want 4 (2 pages x 2 copies)", len(pages))
	}
	w, h := render.Paper{Width: 595.276, Height: 841.890}.Pixels(100)
	if pages[0].Width != w || pages[0].Height != h || pages[0].DPI != 100 {
		t.Fatalf("page size %dx%d", pages[0].Width, pages[0].Height)
	}
	if !pages[0].Black(10, h-10) || pages[1].Black(10, h-10) || !pages[2].Black(10, h-10) {
		t.Fatal("page content or collation wrong")
	}
}

func TestPageOptions(t *testing.T) {
	o, err := PageOptions(backend.JobOptions{})
	if err != nil || o.DPI != 600 || o.Paper.Name != "A4" || o.Dither != render.Threshold || o.Threshold != 128 || !o.AutoRotate {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	for _, bad := range []map[string]string{{"dither": "x"}, {"threshold": "0"}, {"threshold": "x"}, {"rotate": "left"}} {
		if _, err := PageOptions(backend.JobOptions{Options: bad}); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := PageOptions(backend.JobOptions{PaperSize: "B9"}); err == nil {
		t.Error("unknown paper accepted")
	}
}

func TestInvalidPDFIsPermanentError(t *testing.T) {
	err := newBackend(t).Print(context.Background(), bytes.NewReader([]byte("nope")), backend.JobOptions{}, io.Discard)
	if err == nil || backend.IsRetryable(err) {
		t.Fatalf("err %v", err)
	}
}
