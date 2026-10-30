// Command brinspect dissects a captured Brother print data stream and
// optionally compares the decoded pages with a PDFium rendering of the
// source PDF.
//
//	go run ./internal/brother/inspect/brinspect [-width 640] [-pbm prefix] [-pdf source.pdf] capture.prn
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/tim-riep/go-platen/internal/brother/inspect"
	"github.com/tim-riep/go-platen/internal/render"
)

func main() {
	width := flag.Int("width", 640, "row buffer width in bytes")
	pbm := flag.String("pbm", "", "write decoded pages as <prefix>-NN.pbm")
	pdf := flag.String("pdf", "", "source PDF to compare with (rendered at 600 dpi A4)")
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	data, err := os.ReadFile(flag.Arg(0))
	check(err)
	s, err := inspect.Parse(data)
	check(err)

	fmt.Printf("bytes: %d, UELs: %d, pages: %d\n", len(data), s.UELs, len(s.Pages))
	fmt.Println("PJL header:")
	for _, l := range s.PJL {
		fmt.Printf("  %q\n", l)
	}
	fmt.Println("PJL trailer:")
	for _, l := range s.Trailer {
		fmt.Printf("  %q\n", l)
	}
	var refs []*render.Bitmap
	if *pdf != "" {
		refs = renderPDF(*pdf)
	}
	for n, p := range s.Pages {
		fmt.Printf("page %d: form feed %v, compression %d, %d raster groups, %d segments\n", n+1, p.FormFeed, p.Compression, p.RasterGroups, len(p.Segments))
		for _, c := range p.Commands {
			fmt.Printf("  %s\n", c)
		}
		hist := map[int]int{}
		total := 0
		for _, sg := range p.Segments {
			hist[sg.Rows]++
			total += sg.Rows
		}
		keys := make([]int, 0, len(hist))
		for k := range hist {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		fmt.Printf("  rows: %d; segment row counts:", total)
		for _, k := range keys {
			fmt.Printf(" %d×%d", hist[k], k)
		}
		fmt.Println()
		rows, st, err := inspect.DecodeRows(p, *width)
		if err != nil {
			fmt.Printf("  decode error: %v\n", err)
			continue
		}
		used := 0
		for _, r := range rows {
			for i := len(r) - 1; i >= used; i-- {
				if r[i] != 0 {
					used = i + 1
					break
				}
			}
		}
		fmt.Printf("  decoded: %+v, rightmost non-zero byte: %d\n", st, used)
		if *pbm != "" {
			writePBM(fmt.Sprintf("%s-%02d.pbm", *pbm, n+1), rows, *width)
		}
		if n < len(refs) {
			iou, dy, dx := align(rows, *width, refs[n])
			fmt.Printf("  best alignment with reference: dy=%d dx=%d pixels, IoU of black pixels %.4f\n", dy, dx, iou)
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func writePBM(path string, rows [][]byte, width int) {
	f, err := os.Create(path)
	check(err)
	defer f.Close()
	fmt.Fprintf(f, "P4\n%d %d\n", width*8, len(rows))
	for _, r := range rows {
		f.Write(r)
	}
}

func renderPDF(path string) []*render.Bitmap {
	data, err := os.ReadFile(path)
	check(err)
	r, err := render.NewPDFRenderer()
	check(err)
	defer r.Close()
	paper, _ := render.ParsePaper("A4")
	var out []*render.Bitmap
	check(r.Render(context.Background(), data, render.PageOptions{Paper: paper, DPI: 600, Dither: render.Threshold, Threshold: 128, AutoRotate: true},
		func(_ int, bm *render.Bitmap) error { out = append(out, bm); return nil }))
	return out
}

// align finds the offset of the decoded page in the reference with the
// highest intersection over union of black pixels. The search is done on
// whole bytes horizontally (dx is a multiple of 8) and refined per pixel.
func align(rows [][]byte, width int, ref *render.Bitmap) (float64, int, int) {
	black := func(r []byte, x int) bool { return x/8 < len(r) && r[x/8]&(0x80>>(x%8)) != 0 }
	score := func(dy, dx int) float64 {
		inter, union := 0, 0
		for y := 0; y < len(rows); y += 2 {
			ry := y + dy
			for x := 0; x < width*8; x += 2 {
				a := black(rows[y], x)
				rx := x + dx
				b := ry >= 0 && ry < ref.Height && rx >= 0 && rx < ref.Width && ref.Black(rx, ry)
				if a && b {
					inter++
				}
				if a || b {
					union++
				}
			}
		}
		if union == 0 {
			return 1
		}
		return float64(inter) / float64(union)
	}
	best, bdy, bdx := -1.0, 0, 0
	for dy := 60; dy <= 140; dy += 4 {
		for dx := 0; dx <= 200; dx += 8 {
			if s := score(dy, dx); s > best {
				best, bdy, bdx = s, dy, dx
			}
		}
	}
	cy, cx := bdy, bdx
	for dy := cy - 4; dy <= cy+4; dy++ {
		for dx := cx - 8; dx <= cx+8; dx++ {
			if s := score(dy, dx); s > best {
				best, bdy, bdx = s, dy, dx
			}
		}
	}
	return best, bdy, bdx
}
