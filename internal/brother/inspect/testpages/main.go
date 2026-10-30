// Command testpages writes the PDF test pages used to capture further
// Brother print data streams (see testdata/capture-requests/README.md).
// Every page is drawn with exact geometry, so positions in a captured
// stream can be measured against known coordinates.
//
//	go run ./internal/brother/inspect/testpages testdata/capture-requests
package main

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

const ptPerMM = 72 / 25.4

type paper struct {
	name string
	w, h float64 // mm
}

var (
	a4     = paper{"A4", 210, 297}
	a4Land = paper{"A4 landscape", 297, 210}
	letter = paper{"Letter", 215.9, 279.4}
	a5     = paper{"A5", 148, 210}
)

// page collects PDF drawing operators in millimeters with the origin at
// the top left corner, y pointing down.
type page struct {
	p      paper
	ops    strings.Builder
	images []bitImage
}

// bitImage is a 1-bit image; a set bit is black.
type bitImage struct {
	w, h int
	data []byte // rows padded to whole bytes
}

// image draws a 1-bit image into the box given in mm.
func (pg *page) image(img bitImage, x, y, w, h float64) {
	pg.images = append(pg.images, img)
	fmt.Fprintf(&pg.ops, "q %.4f 0 0 %.4f %.4f %.4f cm /Im%d Do Q\n", w*ptPerMM, h*ptPerMM, pg.x(x), pg.y(y+h), len(pg.images))
}

func newPage(p paper) *page { return &page{p: p} }

func (pg *page) x(mm float64) float64 { return mm * ptPerMM }
func (pg *page) y(mm float64) float64 { return (pg.p.h - mm) * ptPerMM }

func (pg *page) gray(g float64) { fmt.Fprintf(&pg.ops, "%.3f g\n", g) }

// rect fills a rectangle given by its top left corner and size in mm.
func (pg *page) rect(x, y, w, h float64) {
	fmt.Fprintf(&pg.ops, "%.4f %.4f %.4f %.4f re f\n", pg.x(x), pg.y(y+h), w*ptPerMM, h*ptPerMM)
}

// frame draws a rectangle outline of the given line width, inside the box.
func (pg *page) frame(x, y, w, h, lw float64) {
	pg.rect(x, y, w, lw)
	pg.rect(x, y+h-lw, w, lw)
	pg.rect(x, y, lw, h)
	pg.rect(x+w-lw, y, lw, h)
}

// text writes Helvetica text with its baseline at (x, y) in mm.
func (pg *page) text(x, y, size float64, s string) {
	s = strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(s)
	fmt.Fprintf(&pg.ops, "BT /F1 %.1f Tf %.3f %.3f Td (%s) Tj ET\n", size, pg.x(x), pg.y(y), s)
}

func writePDF(path string, pages []*page) error {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) int {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
		return len(offsets)
	}
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, len(pages))
	next := 4
	for i, pg := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", next)
		next += 2 + len(pg.images)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	for _, pg := range pages {
		content := pg.ops.String()
		page := len(offsets) + 1
		xobjects := ""
		for i := range pg.images {
			xobjects += fmt.Sprintf(" /Im%d %d 0 R", i+1, page+2+i)
		}
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.3f %.3f] /Resources << /Font << /F1 3 0 R >> /XObject <<%s >> >> /Contents %d 0 R >>",
			pg.p.w*ptPerMM, pg.p.h*ptPerMM, xobjects, page+1))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
		for _, img := range pg.images {
			// Decode [1 0]: a set bit is black.
			obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 1 /Decode [1 0] /Interpolate false /Length %d >>\nstream\n%s\nendstream",
				img.w, img.h, len(img.data), img.data))
		}
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// calibration draws frames at known distances from every edge, rulers with
// 1 mm ticks from each edge inwards, and a center cross.
func calibration(p paper, title string) *page {
	pg := newPage(p)
	pg.gray(0)
	// Frames, 0.5 mm wide, outer edge at 5, 10 and 20 mm from the paper edge.
	for _, d := range []float64{5, 10, 20} {
		pg.frame(d, d, p.w-2*d, p.h-2*d, 0.5)
	}
	// Rulers starting at the paper edge, 1 mm ticks for 30 mm, every 5th
	// tick longer, on all four edges at their middle.
	for i := 0; i <= 30; i++ {
		l := 3.0
		if i%5 == 0 {
			l = 6
		}
		d := float64(i)
		pg.rect(d, p.h/2-l/2, 0.2, l)            // left edge, vertical ticks
		pg.rect(p.w-d-0.2, p.h/2-l/2, 0.2, l)    // right edge
		pg.rect(p.w/2-l/2, d, l, 0.2)            // top edge
		pg.rect(p.w/2-l/2, p.h-d-0.2, l, 0.2)    // bottom edge
		pg.rect(p.w/2+40-l/2, d, l, 0.2)         // second top ruler
		pg.rect(d, p.h/2+40-l/2, 0.2, l)         // second left ruler
		pg.rect(p.w-d-0.2, p.h/2+40-l/2, 0.2, l) // second right ruler
		pg.rect(p.w/2+40-l/2, p.h-d-0.2, l, 0.2) // second bottom ruler
	}
	// Corner squares touching the paper edges (5 x 5 mm).
	pg.rect(0, 0, 5, 5)
	pg.rect(p.w-5, 0, 5, 5)
	pg.rect(0, p.h-5, 5, 5)
	pg.rect(p.w-5, p.h-5, 5, 5)
	// Center cross, 0.3 mm lines, 40 mm long.
	pg.rect(p.w/2-20, p.h/2-0.15, 40, 0.3)
	pg.rect(p.w/2-0.15, p.h/2-20, 0.3, 40)
	// A 1 mm square exactly 30 mm right and 30 mm below the top left corner
	// (its top left corner at 30/30 mm).
	pg.rect(30, 30, 1, 1)
	pg.text(25, 45, 11, title)
	pg.text(25, 51, 8, fmt.Sprintf("%s, %.1f x %.1f mm. Frames: outer edge at 5, 10, 20 mm from the paper edge, 0.5 mm wide.", p.name, p.w, p.h))
	pg.text(25, 56, 8, "Rulers: 1 mm ticks from each paper edge. Corner squares 5 x 5 mm at the paper corners. 1 mm square at 30/30 mm.")
	return pg
}

func fullBlack(p paper) *page {
	pg := newPage(p)
	pg.gray(0)
	pg.rect(0, 0, p.w, p.h)
	return pg
}

// stripes has three bands: vertical hairlines spaced so that one row needs
// far more than 254 changes, a band of random dots (incompressible rows,
// large segments), and fine horizontal hairlines.
func stripes(p paper) *page {
	pg := newPage(p)
	pg.gray(0)
	pg.text(15, 12, 9, "04 stripes: band 1 vertical hairlines every 0.7 mm; band 2 random 0.17 mm dots; band 3 horizontal hairlines every 0.5 mm")
	// Band 1: 0.1 mm lines every 0.7 mm across the whole width.
	for x := 0.0; x < p.w; x += 0.7 {
		pg.rect(x, 20, 0.1, 60)
	}
	// Band 2: random dots of 0.17 mm (a 150 dpi image, 4 x 4 pixels at
	// 600 dpi each), 50 % filled, 80 mm high across the whole width.
	r := rand.New(rand.NewPCG(4, 2))
	bandMM := 80.0
	img := bitImage{w: int(p.w / 25.4 * 150), h: int(bandMM / 25.4 * 150)}
	stride := (img.w + 7) / 8
	img.data = make([]byte, stride*img.h)
	for i := range img.data {
		img.data[i] = byte(r.IntN(256))
	}
	pg.image(img, 0, 90, float64(img.w)*25.4/150, 80)
	// Band 3: horizontal hairlines.
	for y := 180.0; y < 280; y += 0.5 {
		pg.rect(0, y, p.w, 0.1)
	}
	return pg
}

// grays shows a gray wedge in 5 % steps and a continuous gradient.
func grays(p paper) *page {
	pg := newPage(p)
	pg.gray(0)
	pg.text(15, 15, 11, "05 gray levels: 0 % to 100 % in 5 % steps (left to right, top to bottom), then a gradient")
	for i := 0; i <= 20; i++ {
		col, row := i%7, i/7
		x, y := 15+float64(col)*26, 25+float64(row)*32
		pg.gray(1 - float64(i)/20)
		pg.rect(x, y, 24, 24)
		pg.gray(0)
		pg.text(x, y+28, 8, fmt.Sprintf("%d %%", i*5))
	}
	// Gradient in 200 steps of 0.9 mm.
	for i := 0; i < 200; i++ {
		pg.gray(1 - float64(i)/199)
		pg.rect(15+float64(i)*0.9, 130, 0.95, 30)
	}
	// Gray text and fine gray lines.
	for i, g := range []float64{0.25, 0.5, 0.75} {
		pg.gray(1 - g)
		pg.text(15, 180+float64(i)*10, 14, fmt.Sprintf("Text in %d %% gray, 14 pt", int(g*100)))
		pg.rect(110, 175+float64(i)*10, 80, 0.3)
	}
	return pg
}

func landscape() *page {
	pg := calibration(a4Land, "06 landscape: A4 landscape page, print with orientation landscape")
	pg.gray(0)
	// Arrow pointing to the top edge of the landscape page.
	pg.rect(148.5-2, 80, 4, 60)
	// Arrow head above the bar: narrow at the top, wide at the bar.
	for i := 0; i < 20; i++ {
		w := float64(i+1) * 1.2
		pg.rect(148.5-w/2, 64+float64(i)*0.8, w, 0.8)
	}
	pg.text(160, 120, 12, "TOP")
	return pg
}

func numbered(p paper, n, of int) *page {
	pg := newPage(p)
	pg.gray(0)
	pg.text(20, 40, 14, fmt.Sprintf("09 multi page: page %d of %d", n, of))
	// A large block of n bars so the page is recognizable in the raster.
	for i := 0; i < n; i++ {
		pg.rect(30+float64(i)*30, 80, 20, 120)
	}
	return pg
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: testpages <output directory>")
		os.Exit(2)
	}
	dir := os.Args[1]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	files := map[string][]*page{
		"01-calibration-a4.pdf":     {calibration(a4, "01 calibration A4")},
		"02-full-black-a4.pdf":      {fullBlack(a4)},
		"03-blank-a4.pdf":           {newPage(a4)},
		"04-stripes-a4.pdf":         {stripes(a4)},
		"05-grays-a4.pdf":           {grays(a4)},
		"06-landscape-a4.pdf":       {landscape()},
		"07-calibration-letter.pdf": {calibration(letter, "07 calibration Letter")},
		"08-calibration-a5.pdf":     {calibration(a5, "08 calibration A5")},
		"09-three-pages-a4.pdf":     {numbered(a4, 1, 3), numbered(a4, 2, 3), numbered(a4, 3, 3)},
	}
	for name, pages := range files {
		if err := writePDF(filepath.Join(dir, name), pages); err != nil {
			panic(err)
		}
	}
}
