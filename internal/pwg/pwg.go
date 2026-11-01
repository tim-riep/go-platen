// Package pwg writes PWG Raster (PWG 5102.4), the raster format every IPP
// Everywhere printer accepts. A stream is the sync word "RaS2" followed by
// pages; each page is a 1796-byte header and the compressed image.
package pwg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
)

// HeaderSize is the size of a page header.
const HeaderSize = 1796

// Color spaces (cupsColorSpace values used by PWG Raster).
const (
	ColorSpaceGray = 18 // sGray, 0 is black
	ColorSpaceRGB  = 19 // sRGB
)

// Page describes one page.
type Page struct {
	// DPI is the resolution in both directions.
	DPI int
	// Width and Height of the paper in points, from the media size.
	WidthPt, HeightPt int
	// MediaName is the PWG self-describing media name, e.g.
	// "iso_a4_210x297mm"; it may be empty.
	MediaName string
	// Duplex and Tumble describe two-sided printing; Tumble means the
	// short edge binding.
	Duplex, Tumble bool
	// TotalPages is the number of pages in the stream, or 0 if unknown.
	TotalPages int
	// Quality is print-quality: 3 draft, 4 normal, 5 high, 0 default.
	Quality int
}

// Writer writes a PWG Raster stream.
type Writer struct {
	w       io.Writer
	started bool
	line    []byte
	prev    []byte
}

// NewWriter returns a writer for the stream.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WritePage writes an *image.Gray (sGray, 8 bit) or *image.RGBA (sRGB, 8
// bit per color, alpha ignored) as one page.
func (pw *Writer) WritePage(p Page, img image.Image) error {
	var bpp, space int
	var pix []byte
	var stride int
	switch m := img.(type) {
	case *image.Gray:
		bpp, space, pix, stride = 1, ColorSpaceGray, m.Pix, m.Stride
	case *image.RGBA:
		bpp, space, pix, stride = 3, ColorSpaceRGB, m.Pix, m.Stride
	default:
		return fmt.Errorf("pwg: unsupported image type %T", img)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || p.DPI <= 0 {
		return errors.New("pwg: empty page")
	}
	if !pw.started {
		if _, err := io.WriteString(pw.w, "RaS2"); err != nil {
			return err
		}
		pw.started = true
	}
	if _, err := pw.w.Write(header(p, w, h, bpp, space)); err != nil {
		return err
	}
	rowBytes := w * bpp
	if cap(pw.line) < rowBytes {
		pw.line, pw.prev = make([]byte, rowBytes), make([]byte, rowBytes)
	}
	row := func(y int) []byte {
		src := pix[y*stride:]
		if bpp == 1 {
			return src[:w]
		}
		// RGBA to RGB.
		out := pw.line[:rowBytes]
		for x := 0; x < w; x++ {
			copy(out[x*3:x*3+3], src[x*4:x*4+3])
		}
		return out
	}
	buf := make([]byte, 0, rowBytes+rowBytes/64+16)
	for y := 0; y < h; {
		line := row(y)
		cur := pw.prev[:rowBytes]
		copy(cur, line)
		// Count identical following lines (at most 256 lines per record).
		repeat := 1
		for y+repeat < h && repeat < 256 && equal(row(y+repeat), cur) {
			repeat++
		}
		buf = append(buf[:0], byte(repeat-1))
		buf = encodeLine(buf, cur, bpp)
		if _, err := pw.w.Write(buf); err != nil {
			return err
		}
		y += repeat
	}
	return nil
}

func equal(a, b []byte) bool { return string(a) == string(b) }

// encodeLine compresses one line: a control byte 0-127 repeats the next
// pixel n+1 times, 129-255 is followed by 257-n literal pixels.
func encodeLine(dst, line []byte, bpp int) []byte {
	n := len(line) / bpp
	px := func(i int) []byte { return line[i*bpp : (i+1)*bpp] }
	for i := 0; i < n; {
		// Length of the run of identical pixels starting at i.
		run := 1
		for i+run < n && run < 128 && equal(px(i+run), px(i)) {
			run++
		}
		if run > 1 || i+1 == n {
			dst = append(dst, byte(run-1))
			dst = append(dst, px(i)...)
			i += run
			continue
		}
		// Literal pixels until two identical pixels follow.
		lit := 1
		for i+lit < n && lit < 128 && (i+lit+1 >= n || !equal(px(i+lit), px(i+lit+1))) {
			lit++
		}
		if lit == 1 {
			dst = append(dst, 0)
		} else {
			dst = append(dst, byte(257-lit))
		}
		dst = append(dst, line[i*bpp:(i+lit)*bpp]...)
		i += lit
	}
	return dst
}

// header builds a page header. Fields not set are zero, as PWG 5102.4
// allows; offsets are given in the comments.
func header(p Page, w, h, bpp, space int) []byte {
	b := make([]byte, HeaderSize)
	copy(b[0:64], "PwgRaster")
	u32 := func(off int, v int) { binary.BigEndian.PutUint32(b[off:], uint32(int32(v))) }
	if p.Duplex {
		u32(272, 1)
	}
	u32(276, p.DPI) // HWResolution
	u32(280, p.DPI)
	u32(340, 1)         // NumCopies; copies are separate pages
	u32(352, p.WidthPt) // PageSize
	u32(356, p.HeightPt)
	if p.Tumble {
		u32(368, 1)
	}
	u32(372, w)
	u32(376, h)
	u32(384, 8)     // BitsPerColor
	u32(388, bpp*8) // BitsPerPixel
	u32(392, w*bpp) // BytesPerLine
	u32(396, 0)     // ColorOrder: chunky
	u32(400, space) // ColorSpace
	u32(420, bpp)   // NumColors
	u32(452, p.TotalPages)
	u32(456, 1) // CrossFeedTransform
	u32(460, 1) // FeedTransform
	u32(472, w) // ImageBox: left 0, top 0, right, bottom
	u32(476, h)
	u32(480, 0x00ffffff) // AlternatePrimary: white
	u32(484, p.Quality)
	copy(b[1732:1796], p.MediaName)
	return b
}
