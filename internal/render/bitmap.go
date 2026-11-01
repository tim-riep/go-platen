// Package render turns documents into 1-bit bitmaps at printer resolution.
package render

import (
	"fmt"
	"image"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Bitmap is a 1-bit image. Rows are packed most significant bit first and
// padded to whole bytes; a set bit is a black pixel.
type Bitmap struct {
	Width, Height int
	Stride        int
	DPI           int
	Data          []byte
}

// NewBitmap returns a white bitmap.
func NewBitmap(w, h, dpi int) *Bitmap {
	stride := (w + 7) / 8
	return &Bitmap{Width: w, Height: h, Stride: stride, DPI: dpi, Data: make([]byte, stride*h)}
}

// Row returns the packed bytes of row y.
func (b *Bitmap) Row(y int) []byte {
	return b.Data[y*b.Stride : (y+1)*b.Stride]
}

// Black reports whether the pixel is black.
func (b *Bitmap) Black(x, y int) bool {
	return b.Data[y*b.Stride+x/8]&(0x80>>(x%8)) != 0
}

// Set makes the pixel black.
func (b *Bitmap) Set(x, y int) {
	b.Data[y*b.Stride+x/8] |= 0x80 >> (x % 8)
}

// Dither is the method that reduces gray levels to black and white.
type Dither string

const (
	// Threshold maps every pixel darker than the threshold to black. It
	// keeps text and barcodes sharp.
	Threshold Dither = "threshold"
	// FloydSteinberg is error diffusion. It scatters single pixels, which
	// suits inkjet printers but often does not develop on laser printers.
	FloydSteinberg Dither = "floyd-steinberg"
	// Halftone is an ordered clustered-dot screen at 45 degrees: gray
	// becomes clumps of pixels that grow with darkness, which laser
	// printers reproduce reliably. Black and white areas stay solid.
	Halftone Dither = "halftone"
)

// ParseDither parses a dithering method name; empty means Threshold.
func ParseDither(s string) (Dither, error) {
	switch Dither(strings.ToLower(s)) {
	case "", Threshold:
		return Threshold, nil
	case FloydSteinberg, "error-diffusion", "diffusion":
		return FloydSteinberg, nil
	case Halftone, "clustered":
		return Halftone, nil
	}
	return "", fmt.Errorf("unknown dithering method %q (use %q, %q or %q)", s, Threshold, Halftone, FloydSteinberg)
}

// ToBitmap converts a grayscale image (0 = black, 255 = white) to a bitmap.
// Pixels with a value below threshold become black.
func ToBitmap(g *image.Gray, method Dither, threshold uint8, dpi int) *Bitmap {
	r := g.Bounds()
	w, h := r.Dx(), r.Dy()
	bm := NewBitmap(w, h, dpi)
	switch method {
	case FloydSteinberg:
		diffuse(g, bm, threshold)
		return bm
	case Halftone:
		screen(g, bm)
		return bm
	}
	for y := 0; y < h; y++ {
		row := g.Pix[(y)*g.Stride : y*g.Stride+w]
		out := bm.Row(y)
		for x, v := range row {
			if v < threshold {
				out[x>>3] |= 0x80 >> (x & 7)
			}
		}
	}
	return bm
}

// diffuse implements Floyd-Steinberg error diffusion with serpentine
// scanning. Errors are kept for the current and the next row only.
func diffuse(g *image.Gray, bm *Bitmap, threshold uint8) {
	w, h := bm.Width, bm.Height
	cur := make([]int32, w+2)
	next := make([]int32, w+2)
	t := int32(threshold)
	for y := 0; y < h; y++ {
		row := g.Pix[y*g.Stride : y*g.Stride+w]
		out := bm.Row(y)
		ltr := y%2 == 0
		for i := 0; i < w; i++ {
			x, dir := i, 1
			if !ltr {
				x, dir = w-1-i, -1
			}
			v := int32(row[x]) + cur[x+1]/16
			var e int32
			if v < t {
				out[x>>3] |= 0x80 >> (x & 7)
				e = v
			} else {
				e = v - 255
			}
			// Distribute 7/16 ahead, 3/16 behind below, 5/16 below,
			// 1/16 ahead below. Values are scaled by 16.
			cur[x+1+dir] += e * 7
			next[x+1-dir] += e * 3
			next[x+1] += e * 5
			next[x+1+dir] += e * 1
		}
		cur, next = next, cur
		for i := range next {
			next[i] = 0
		}
	}
}

// The halftone screen repeats every screenSize pixels in both directions.
// Its dots sit on a 45 degree lattice spanned by (4, 4) and (4, -4), which
// at 600 dpi is a screen of about 106 lines per inch; each 32-pixel cell
// renders 33 gray levels.
const screenSize = 8

// screenOrder holds, for each position in the screen tile, the rank at
// which it turns black as the gray darkens (0 first).
var screenOrder = func() [screenSize][screenSize]int {
	type cell struct {
		x, y int
		spot float64
	}
	cells := make([]cell, 0, screenSize*screenSize)
	for y := 0; y < screenSize; y++ {
		for x := 0; x < screenSize; x++ {
			// Lattice coordinates of the pixel center.
			u := (float64(x) + 0.5 + float64(y) + 0.5) / 8
			v := (float64(x) + 0.5 - float64(y) - 0.5) / 8
			fu, fv := u-math.Floor(u), v-math.Floor(v)
			// Round dot spot function: highest at the dot center.
			spot := math.Cos(2*math.Pi*fu) + math.Cos(2*math.Pi*fv)
			cells = append(cells, cell{x, y, spot})
		}
	}
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].spot > cells[j].spot })
	var order [screenSize][screenSize]int
	for rank, c := range cells {
		order[c.y][c.x] = rank
	}
	return order
}()

// screen applies the clustered-dot halftone screen.
func screen(g *image.Gray, bm *Bitmap) {
	const n = screenSize * screenSize
	var limits [screenSize][screenSize]int
	for y := range limits {
		for x := range limits[y] {
			// A pixel turns black when its darkness exceeds the limit of
			// its rank; rank 0 turns black first.
			limits[y][x] = (2*screenOrder[y][x] + 1) * 255 / (2 * n)
		}
	}
	for y := 0; y < bm.Height; y++ {
		row := g.Pix[y*g.Stride : y*g.Stride+bm.Width]
		out := bm.Row(y)
		lim := &limits[y%screenSize]
		for x, v := range row {
			if 255-int(v) > lim[x%screenSize] {
				out[x>>3] |= 0x80 >> (x & 7)
			}
		}
	}
}

// Paper is a paper size in points (1/72 inch), portrait.
type Paper struct {
	Name          string
	Width, Height float64
}

var papers = map[string]Paper{
	"a4":     {"A4", 595.276, 841.890},
	"a5":     {"A5", 419.528, 595.276},
	"a6":     {"A6", 297.638, 419.528},
	"letter": {"Letter", 612, 792},
	"legal":  {"Legal", 612, 1008},
}

// ParsePaper returns the paper size with the name; empty means A4.
// Besides the names above, a size can be given as <width>x<height> in mm
// or in, e.g. "100x150mm" or "4x6in" for labels; width is the side across
// the print head.
func ParsePaper(name string) (Paper, error) {
	if name == "" {
		name = "a4"
	}
	if p, ok := papers[strings.ToLower(name)]; ok {
		return p, nil
	}
	if p, ok := customPaper(name); ok {
		return p, nil
	}
	return Paper{}, fmt.Errorf("unknown paper size %q; use A4, A5, A6, Letter, Legal or a size such as 100x150mm or 4x6in", name)
}

func customPaper(name string) (Paper, bool) {
	s := strings.ToLower(strings.TrimSpace(name))
	var unit float64
	switch {
	case strings.HasSuffix(s, "mm"):
		unit, s = 72/25.4, strings.TrimSuffix(s, "mm")
	case strings.HasSuffix(s, "in"):
		unit, s = 72, strings.TrimSuffix(s, "in")
	default:
		return Paper{}, false
	}
	ws, hs, ok := strings.Cut(s, "x")
	if !ok {
		return Paper{}, false
	}
	w, err1 := strconv.ParseFloat(ws, 64)
	h, err2 := strconv.ParseFloat(hs, 64)
	// From 10 mm up to 1.2 m, the longest label roll lengths.
	if err1 != nil || err2 != nil || w*unit < 28 || h*unit < 28 || w*unit > 3400 || h*unit > 3400 {
		return Paper{}, false
	}
	return Paper{Name: name, Width: w * unit, Height: h * unit}, true
}

// Pixels returns the paper size in pixels at the resolution.
func (p Paper) Pixels(dpi int) (w, h int) {
	return int(p.Width*float64(dpi)/72 + 0.5), int(p.Height*float64(dpi)/72 + 0.5)
}
