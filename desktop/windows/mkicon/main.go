// Command mkicon draws the platen icon (the same design as the macOS app
// icon) and writes it as a Windows .ico with PNG images, or as a 256 pixel
// PNG for Linux if the name ends in .png:
//
//	go run ./desktop/windows/mkicon cmd/platen-desktop/platen.ico
//	go run ./desktop/windows/mkicon cmd/platen-desktop/platen.png
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"strings"
)

// Shapes in a 1024 unit square, origin top left.
type rect struct{ x, y, w, h, r float64 }

func (q rect) contains(x, y float64) bool {
	if x < q.x || y < q.y || x > q.x+q.w || y > q.y+q.h {
		return false
	}
	cx := min(max(x, q.x+q.r), q.x+q.w-q.r)
	cy := min(max(y, q.y+q.r), q.y+q.h-q.r)
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= q.r*q.r
}

type rgba struct{ r, g, b, a float64 }

func over(dst, src rgba) rgba {
	a := src.a + dst.a*(1-src.a)
	if a == 0 {
		return rgba{}
	}
	mix := func(s, d float64) float64 { return (s*src.a + d*dst.a*(1-src.a)) / a }
	return rgba{mix(src.r, dst.r), mix(src.g, dst.g), mix(src.b, dst.b), a}
}

// gradient runs from top (c0) to bottom (c1) of q.
func gradient(q rect, y float64, c0, c1 rgba) rgba {
	t := min(max((y-q.y)/q.h, 0), 1)
	return rgba{c0.r + (c1.r-c0.r)*t, c0.g + (c1.g-c0.g)*t, c0.b + (c1.b-c0.b)*t, 1}
}

var (
	bg      = rect{100, 100, 824, 824, 185}
	sheet   = rect{300, 304, 424, 470, 24}
	roller  = rect{220, 224, 584, 110, 55}
	shine   = rect{260, 246, 504, 18, 9}
	lineGap = 70.0
	lines   = []float64{300, 340, 250, 320}
)

func sample(x, y float64) rgba {
	var c rgba
	if bg.contains(x, y) {
		c = over(c, gradient(bg, y, rgba{0.20, 0.45, 0.95, 1}, rgba{0.08, 0.22, 0.62, 1}))
	}
	if sheet.contains(x, y) {
		c = over(c, rgba{1, 1, 1, 1})
	}
	for i, w := range lines {
		if (rect{352, 1024 - (560 - float64(i)*lineGap) - 28, w, 28, 14}).contains(x, y) {
			c = over(c, rgba{0.78, 0.78, 0.78, 1})
		}
	}
	if roller.contains(x, y) {
		c = over(c, gradient(roller, y, rgba{0.22, 0.22, 0.22, 1}, rgba{0.08, 0.08, 0.08, 1}))
	}
	if shine.contains(x, y) {
		c = over(c, rgba{1, 1, 1, 0.18})
	}
	return c
}

func draw(size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	scale := 1024 / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					c := sample((float64(px)+(float64(sx)+0.5)/ss)*scale, (float64(py)+(float64(sy)+0.5)/ss)*scale)
					r, g, b, a = r+c.r*c.a, g+c.g*c.a, b+c.b*c.a, a+c.a
				}
			}
			if a > 0 {
				img.SetNRGBA(px, py, color.NRGBA{uint8(r / a * 255), uint8(g / a * 255), uint8(b / a * 255), uint8(a / ss / ss * 255)})
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: mkicon <out.ico|out.png>")
	}
	if strings.HasSuffix(os.Args[1], ".png") {
		if err := os.WriteFile(os.Args[1], draw(256), 0o644); err != nil {
			log.Fatal(err)
		}
		return
	}
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 256}
	var images [][]byte
	for _, s := range sizes {
		images = append(images, draw(s))
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := uint8(s)
		if s >= 256 {
			dim = 0
		}
		binary.Write(&out, binary.LittleEndian, struct {
			W, H, Colors, Reserved uint8
			Planes, BPP            uint16
			Size, Offset           uint32
		}{dim, dim, 0, 0, 1, 32, uint32(len(images[i])), uint32(offset)})
		offset += len(images[i])
	}
	for _, img := range images {
		out.Write(img)
	}
	if err := os.WriteFile(os.Args[1], out.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}
