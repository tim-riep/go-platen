// Package render turns documents into 1-bit bitmaps at printer resolution.
package render

import (
	"fmt"
	"image"
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
	// FloydSteinberg is error diffusion; it reproduces gray areas and
	// photos better.
	FloydSteinberg Dither = "floyd-steinberg"
)

// ParseDither parses a dithering method name; empty means Threshold.
func ParseDither(s string) (Dither, error) {
	switch Dither(strings.ToLower(s)) {
	case "", Threshold:
		return Threshold, nil
	case FloydSteinberg, "error-diffusion", "diffusion":
		return FloydSteinberg, nil
	}
	return "", fmt.Errorf("unknown dithering method %q (use %q or %q)", s, Threshold, FloydSteinberg)
}

// ToBitmap converts a grayscale image (0 = black, 255 = white) to a bitmap.
// Pixels with a value below threshold become black.
func ToBitmap(g *image.Gray, method Dither, threshold uint8, dpi int) *Bitmap {
	r := g.Bounds()
	w, h := r.Dx(), r.Dy()
	bm := NewBitmap(w, h, dpi)
	if method == FloydSteinberg {
		diffuse(g, bm, threshold)
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
func ParsePaper(name string) (Paper, error) {
	if name == "" {
		name = "a4"
	}
	p, ok := papers[strings.ToLower(name)]
	if !ok {
		return Paper{}, fmt.Errorf("unknown paper size %q", name)
	}
	return p, nil
}

// Pixels returns the paper size in pixels at the resolution.
func (p Paper) Pixels(dpi int) (w, h int) {
	return int(p.Width*float64(dpi)/72 + 0.5), int(p.Height*float64(dpi)/72 + 0.5)
}
