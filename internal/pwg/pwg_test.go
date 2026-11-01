package pwg

import (
	"bytes"
	"encoding/binary"
	"image"
	"math/rand"
	"testing"
)

// decode reads a stream written by Writer back into images, as a printer
// would, to check the encoding.
func decode(t *testing.T, data []byte) []image.Image {
	t.Helper()
	if string(data[:4]) != "RaS2" {
		t.Fatalf("sync word %q", data[:4])
	}
	data = data[4:]
	var pages []image.Image
	for len(data) > 0 {
		h := data[:HeaderSize]
		data = data[HeaderSize:]
		if string(h[:9]) != "PwgRaster" {
			t.Fatal("header name")
		}
		u32 := func(off int) int { return int(binary.BigEndian.Uint32(h[off:])) }
		w, ht, bpp, space := u32(372), u32(376), u32(388)/8, u32(400)
		if u32(392) != w*bpp || u32(384) != 8 {
			t.Fatalf("bytes per line %d, bits per color %d", u32(392), u32(384))
		}
		pix := make([]byte, 0, w*ht*bpp)
		for y := 0; y < ht; {
			repeat := int(data[0]) + 1
			data = data[1:]
			var line []byte
			for len(line) < w*bpp {
				c := int(data[0])
				data = data[1:]
				switch {
				case c < 128:
					p := data[:bpp]
					data = data[bpp:]
					for i := 0; i <= c; i++ {
						line = append(line, p...)
					}
				case c > 128:
					n := (257 - c) * bpp
					line = append(line, data[:n]...)
					data = data[n:]
				default:
					t.Fatal("control byte 128")
				}
			}
			if len(line) != w*bpp {
				t.Fatalf("line of %d bytes, want %d", len(line), w*bpp)
			}
			for i := 0; i < repeat; i++ {
				pix = append(pix, line...)
			}
			y += repeat
		}
		r := image.Rect(0, 0, w, ht)
		if space == ColorSpaceGray {
			pages = append(pages, &image.Gray{Pix: pix, Stride: w, Rect: r})
		} else {
			rgba := image.NewRGBA(r)
			for i := 0; i < w*ht; i++ {
				copy(rgba.Pix[i*4:], pix[i*3:i*3+3])
				rgba.Pix[i*4+3] = 0xff
			}
			pages = append(pages, rgba)
		}
	}
	return pages
}

func testImages() (*image.Gray, *image.RGBA) {
	rnd := rand.New(rand.NewSource(1))
	w, h := 301, 97
	g := image.NewGray(image.Rect(0, 0, w, h))
	c := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(0xff)
			switch {
			case y < 20: // identical white lines
			case y < 40: // long runs
				v = byte(x / 50 * 40)
			default: // noise and short runs
				if rnd.Intn(3) > 0 {
					v = byte(rnd.Intn(4) * 60)
				}
			}
			g.Pix[y*g.Stride+x] = v
			i := y*c.Stride + x*4
			c.Pix[i], c.Pix[i+1], c.Pix[i+2], c.Pix[i+3] = v, 255-v, byte(x), 0xff
		}
	}
	return g, c
}

func TestRoundTrip(t *testing.T) {
	g, c := testImages()
	var buf bytes.Buffer
	pw := NewWriter(&buf)
	p := Page{DPI: 300, WidthPt: 595, HeightPt: 842, MediaName: "iso_a4_210x297mm", TotalPages: 2}
	if err := pw.WritePage(p, g); err != nil {
		t.Fatal(err)
	}
	if err := pw.WritePage(p, c); err != nil {
		t.Fatal(err)
	}
	pages := decode(t, buf.Bytes())
	if len(pages) != 2 {
		t.Fatalf("%d pages", len(pages))
	}
	if got := pages[0].(*image.Gray); !bytes.Equal(got.Pix, g.Pix) {
		t.Fatal("gray page differs")
	}
	if got := pages[1].(*image.RGBA); !bytes.Equal(got.Pix, c.Pix) {
		t.Fatal("color page differs")
	}
	// Compression works on runs.
	if buf.Len() > len(g.Pix)+len(c.Pix)*3/4 {
		t.Fatalf("stream of %d bytes is not compressed", buf.Len())
	}
	h := buf.Bytes()[4 : 4+HeaderSize]
	if res := binary.BigEndian.Uint32(h[276:]); res != 300 || string(bytes.TrimRight(h[1732:], "\x00")) != "iso_a4_210x297mm" {
		t.Fatalf("header resolution %d, media %q", res, h[1732:1760])
	}
}

func TestEncodeLineEdgeCases(t *testing.T) {
	for _, line := range [][]byte{{7}, {1, 2}, {3, 3}, {1, 2, 2}, bytes.Repeat([]byte{9}, 300), func() []byte {
		b := make([]byte, 300)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}()} {
		enc := encodeLine(nil, line, 1)
		var out []byte
		for len(enc) > 0 {
			c := int(enc[0])
			enc = enc[1:]
			if c < 128 {
				out = append(out, bytes.Repeat(enc[:1], c+1)...)
				enc = enc[1:]
			} else {
				out = append(out, enc[:257-c]...)
				enc = enc[257-c:]
			}
		}
		if !bytes.Equal(out, line) {
			t.Fatalf("line %v: decoded %v", line, out)
		}
	}
}
