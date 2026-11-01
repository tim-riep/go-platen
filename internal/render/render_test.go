package render

import (
	"context"
	"image"
	"sync"
	"testing"
)

var (
	testRendererOnce sync.Once
	testRenderer     *PDFRenderer
	testRendererErr  error
)

func renderer(t *testing.T) *PDFRenderer {
	t.Helper()
	testRendererOnce.Do(func() { testRenderer, testRendererErr = NewPDFRenderer() })
	if testRendererErr != nil {
		t.Fatal(testRendererErr)
	}
	return testRenderer
}

func renderAll(t *testing.T, pdf []byte, o PageOptions) []*Bitmap {
	t.Helper()
	var pages []*Bitmap
	err := renderer(t).Render(context.Background(), pdf, o, func(_ int, bm *Bitmap) error {
		pages = append(pages, bm)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

func a4(t *testing.T) Paper {
	p, err := ParsePaper("A4")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRenderRectangle(t *testing.T) {
	// A4 page, black square from (72,72) to (216,216) in PDF coordinates
	// (origin bottom left).
	pdf := makePDF(595.276, 841.890, "0 0 0 rg 72 72 144 144 re f", "0 0 0 rg 0 0 10 10 re f")
	pages := renderAll(t, pdf, PageOptions{Paper: a4(t), DPI: 72, Dither: Threshold})
	if len(pages) != 2 {
		t.Fatalf("%d pages", len(pages))
	}
	bm := pages[0]
	if bm.Width != 595 || bm.Height != 842 || bm.DPI != 72 {
		t.Fatalf("size %dx%d", bm.Width, bm.Height)
	}
	if !bm.Black(144, 842-144) {
		t.Error("center of square is white")
	}
	for _, p := range [][2]int{{10, 10}, {300, 400}, {60, 842 - 144}, {230, 842 - 144}} {
		if bm.Black(p[0], p[1]) {
			t.Errorf("pixel %v is black", p)
		}
	}
	if !pages[1].Black(2, 840) || pages[1].Black(100, 100) {
		t.Error("second page wrong")
	}
}

func TestRenderResolutionAndFit(t *testing.T) {
	// A Letter page on A4 paper is scaled to fit and centered.
	pdf := makePDF(612, 792, "0 0 0 rg 0 0 612 792 re f")
	bm := renderAll(t, pdf, PageOptions{Paper: a4(t), DPI: 150})[0]
	w, h := a4(t).Pixels(150)
	if bm.Width != w || bm.Height != h {
		t.Fatalf("size %dx%d, want %dx%d", bm.Width, bm.Height, w, h)
	}
	if !bm.Black(w/2, h/2) {
		t.Error("page not rendered")
	}
	// Letter is shorter relative to its width than A4: white bands top and
	// bottom, none left and right.
	if bm.Black(w/2, 2) || bm.Black(w/2, h-3) {
		t.Error("expected white margin at top and bottom")
	}
	if !bm.Black(1, h/2) || !bm.Black(w-2, h/2) {
		t.Error("expected page to fill the width")
	}
}

func TestRenderAutoRotate(t *testing.T) {
	// Landscape A4 with a mark in the top left corner.
	pdf := makePDF(841.890, 595.276, "0 0 0 rg 0 545 50 50 re f")
	bm := renderAll(t, pdf, PageOptions{Paper: a4(t), DPI: 72, AutoRotate: true})[0]
	if bm.Width != 595 || bm.Height != 842 {
		t.Fatalf("size %dx%d", bm.Width, bm.Height)
	}
	// Rotated clockwise, the top left corner ends up top right.
	if !bm.Black(590, 5) || bm.Black(5, 5) {
		t.Error("mark not rotated to the top right")
	}
	// Without rotation the page is scaled to the paper width (factor
	// 595/842) and centered vertically, so the mark is at the left edge
	// about 211 pixels from the top.
	bm = renderAll(t, pdf, PageOptions{Paper: a4(t), DPI: 72})[0]
	if !bm.Black(5, 220) || bm.Black(590, 5) || bm.Black(5, 5) {
		t.Error("page rotated or misplaced although AutoRotate is off")
	}
}

func TestRenderInvalidPDF(t *testing.T) {
	err := renderer(t).Render(context.Background(), []byte("not a pdf"), PageOptions{Paper: a4(t), DPI: 72}, func(int, *Bitmap) error { return nil })
	if err == nil {
		t.Fatal("invalid pdf rendered")
	}
}

func TestThresholdAndDiffusion(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 64, 64))
	for i := range g.Pix {
		g.Pix[i] = 128 // 50% gray
	}
	th := ToBitmap(g, Threshold, 129, 600)
	if !th.Black(0, 0) || !th.Black(63, 63) {
		t.Error("threshold: gray below threshold must be black")
	}
	fs := ToBitmap(g, FloydSteinberg, 128, 600)
	black := 0
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if fs.Black(x, y) {
				black++
			}
		}
	}
	if black < 64*64*4/10 || black > 64*64*6/10 {
		t.Errorf("error diffusion of 50%% gray gave %d of %d black pixels", black, 64*64)
	}
	for i := range g.Pix {
		g.Pix[i] = 255
	}
	if ToBitmap(g, FloydSteinberg, 128, 600).Black(10, 10) {
		t.Error("white became black")
	}
}

func TestBitmapPacking(t *testing.T) {
	bm := NewBitmap(10, 2, 300)
	if bm.Stride != 2 {
		t.Fatal(bm.Stride)
	}
	bm.Set(0, 0)
	bm.Set(9, 1)
	if bm.Row(0)[0] != 0x80 || bm.Row(1)[1] != 0x40 {
		t.Fatalf("%x", bm.Data)
	}
}

func TestParse(t *testing.T) {
	if _, err := ParsePaper("tabloid"); err == nil {
		t.Error("unknown paper accepted")
	}
	if p, _ := ParsePaper(""); p.Name != "A4" {
		t.Error("default paper")
	}
	if w, h := a4(t).Pixels(600); w != 4961 || h != 7016 {
		t.Errorf("A4 at 600 dpi: %dx%d", w, h)
	}
	if d, _ := ParseDither("error-diffusion"); d != FloydSteinberg {
		t.Error(d)
	}
	if _, err := ParseDither("ordered"); err == nil {
		t.Error("unknown dither accepted")
	}
}

func TestHalftone(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 64, 64))
	coverage := func(v uint8) float64 {
		for i := range g.Pix {
			g.Pix[i] = v
		}
		bm := ToBitmap(g, Halftone, 128, 600)
		black := 0
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				if bm.Black(x, y) {
					black++
				}
			}
		}
		return float64(black) / (64 * 64)
	}
	if c := coverage(0); c != 1 {
		t.Errorf("black: coverage %.2f", c)
	}
	if c := coverage(255); c != 0 {
		t.Errorf("white: coverage %.2f", c)
	}
	prev := -1.0
	for v := 255; v >= 0; v -= 15 {
		c := coverage(uint8(v))
		if c < prev {
			t.Fatalf("coverage not monotonic at gray %d", v)
		}
		if d := c - (255-float64(v))/255; d > 0.06 || d < -0.06 {
			t.Errorf("gray %d: coverage %.2f", v, c)
		}
		prev = c
	}
	// Light gray must form clusters, not isolated pixels: at about 15 %
	// coverage most black pixels have a black neighbor.
	for i := range g.Pix {
		g.Pix[i] = 215
	}
	bm := ToBitmap(g, Halftone, 128, 600)
	lonely, black := 0, 0
	for y := 1; y < 63; y++ {
		for x := 1; x < 63; x++ {
			if !bm.Black(x, y) {
				continue
			}
			black++
			if !bm.Black(x-1, y) && !bm.Black(x+1, y) && !bm.Black(x, y-1) && !bm.Black(x, y+1) {
				lonely++
			}
		}
	}
	if black == 0 || lonely*10 > black {
		t.Errorf("light gray: %d of %d black pixels isolated", lonely, black)
	}
	if d, _ := ParseDither("clustered"); d != Halftone {
		t.Error(d)
	}
}

func TestRenderImagesColor(t *testing.T) {
	// A red square on an otherwise empty (transparent) A4 page, and a
	// landscape page with a blue square that is turned to portrait.
	pdf := makePDF(595.276, 841.890, "1 0 0 rg 50 600 100 100 re f")
	land := makePDF(841.890, 595.276, "0 0 1 rg 0 0 100 100 re f")
	paper, _ := ParsePaper("A4")
	o := PageOptions{Paper: paper, DPI: 72, AutoRotate: true}
	var imgs []image.Image
	for _, doc := range [][]byte{pdf, land} {
		err := renderer(t).RenderImages(context.Background(), doc, o, true, func(_ int, img image.Image) error {
			imgs = append(imgs, img)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	at := func(img image.Image, x, y int) [3]uint8 {
		m := img.(*image.RGBA)
		i := m.PixOffset(x, y)
		return [3]uint8{m.Pix[i], m.Pix[i+1], m.Pix[i+2]}
	}
	red := imgs[0]
	if b := red.Bounds(); b.Dx() != 595 || b.Dy() != 842 {
		t.Fatalf("size %v", b)
	}
	if c := at(red, 100, 190); c != [3]uint8{255, 0, 0} {
		t.Fatalf("square %v, want red", c)
	}
	if c := at(red, 400, 50); c != [3]uint8{255, 255, 255} {
		t.Fatalf("background %v, want white", c)
	}
	// The landscape page's bottom left corner ends up top left.
	blue := imgs[1]
	if b := blue.Bounds(); b.Dx() != 595 || b.Dy() != 842 {
		t.Fatalf("rotated size %v", b)
	}
	if c := at(blue, 50, 50); c != [3]uint8{0, 0, 255} {
		t.Fatalf("rotated square %v, want blue", c)
	}

	// Gray rendering returns 8-bit gray.
	err := renderer(t).RenderImages(context.Background(), pdf, o, false, func(_ int, img image.Image) error {
		g := img.(*image.Gray)
		if v := g.GrayAt(100, 190).Y; v == 0 || v == 255 {
			t.Fatalf("red as gray: %d", v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
