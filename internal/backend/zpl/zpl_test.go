package zpl

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/tim-riep/go-platen/internal/backend"
	"github.com/tim-riep/go-platen/internal/backend/raster"
	"github.com/tim-riep/go-platen/internal/render"
)

var gfRe = regexp.MustCompile(`\^GFA,(\d+),(\d+),(\d+),([^\^]*)\^FS`)

// decodeGraphic expands ^GFA data as a printer would.
func decodeGraphic(t *testing.T, data string, total, stride int) []byte {
	t.Helper()
	var out, row []byte
	prev := make([]byte, stride*2)
	flush := func() {
		if len(row) != stride*2 {
			t.Fatalf("row of %d hex digits, want %d", len(row), stride*2)
		}
		prev = append(prev[:0], row...)
		for i := 0; i < len(row); i += 2 {
			b, _ := strconv.ParseUint(string(row[i:i+2]), 16, 8)
			out = append(out, byte(b))
		}
		row = row[:0]
	}
	count := 0
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c >= 'G' && c <= 'Y':
			count += int(c-'G') + 1
		case c >= 'g' && c <= 'z':
			count += (int(c-'g') + 1) * 20
		case c == ',' || c == '!':
			fill := byte('0')
			if c == '!' {
				fill = 'F'
			}
			for len(row) < stride*2 {
				row = append(row, fill)
			}
			flush()
		case c == ':':
			if len(row) != 0 {
				t.Fatal(": inside a row")
			}
			row = append(row, prev...)
			flush()
		case strings.IndexByte(hexDigits, c) >= 0:
			n := max(count, 1)
			count = 0
			for k := 0; k < n; k++ {
				row = append(row, c)
			}
			if len(row) > stride*2 {
				t.Fatalf("run crosses a row: %d", len(row))
			}
			if len(row) == stride*2 {
				flush()
			}
		default:
			t.Fatalf("unexpected %q", c)
		}
	}
	if len(out) != total {
		t.Fatalf("decoded %d bytes, want %d", len(out), total)
	}
	return out
}

func TestRunCount(t *testing.T) {
	for n, want := range map[int]string{2: "H", 19: "Y", 20: "g", 21: "gG", 45: "hK", 400: "z", 419: "zY"} {
		if got := runCount(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}

func TestGraphicRoundTrip(t *testing.T) {
	bm := render.NewBitmap(1000, 7, 203)
	// row 0 white, row 1 black, rows 2-3 a pattern, row 4 like row 3,
	// row 5 black then white, row 6 long runs.
	for x := 0; x < 1000; x++ {
		bm.Set(x, 1)
		if x%3 == 0 {
			bm.Set(x, 2)
			bm.Set(x, 3)
			bm.Set(x, 4)
		}
		if x < 500 {
			bm.Set(x, 5)
		}
		if x/100%2 == 1 {
			bm.Set(x, 6)
		}
	}
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	writeGraphic(w, bm)
	w.Flush()
	got := decodeGraphic(t, b.String(), len(bm.Data), bm.Stride)
	if !bytes.Equal(got, bm.Data) {
		t.Fatal("decoded graphic differs")
	}
	if b.Len() > len(bm.Data) {
		t.Fatalf("%d characters for %d bytes: not compressed", b.Len(), len(bm.Data))
	}
}

func TestLabels(t *testing.T) {
	ctx := context.Background()
	b, err := Factory.New()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := raster.PDFToRaster.New()
	if err != nil {
		t.Fatal(err)
	}
	// A 4x6 inch shipping label with a barcode-like pattern, two pages.
	var bars strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&bars, "%d 300 %d 80 re f ", 20+i*8, 2+i%3)
	}
	pdf := makePDF(288, 432, "0 0 0 rg 10 10 268 40 re f "+bars.String(), "0.3 g 50 50 100 100 re f")
	opts := backend.JobOptions{ContentType: "application/pdf", PaperSize: "4x6in", ResolutionDPI: 203, Copies: 2,
		Options: map[string]string{"darkness": "5"}}
	var out bytes.Buffer
	if err := b.Print(ctx, bytes.NewReader(pdf), opts, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if n := strings.Count(s, "^XA"); n != 4 || strings.Count(s, "^XZ") != 4 {
		t.Fatalf("%d labels, want 4 (2 pages, 2 copies)", n)
	}
	if !strings.Contains(s, "^PW812\n^LL1218\n") || !strings.Contains(s, "^MD5\n") {
		t.Fatalf("label setup: %.80q", s)
	}
	// The graphics are exactly what the renderer makes (threshold).
	var pbm bytes.Buffer
	refOpts := opts
	refOpts.Copies, refOpts.Options = 1, map[string]string{"dither": "threshold"}
	if err := ref.Print(ctx, bytes.NewReader(pdf), refOpts, &pbm); err != nil {
		t.Fatal(err)
	}
	header := regexp.MustCompile(`P4\n# dpi=\d+\n(\d+) (\d+)\n`)
	pages := header.Split(pbm.String(), -1)[1:]
	graphics := gfRe.FindAllStringSubmatch(s, -1)
	if len(graphics) != 4 || len(pages) != 2 {
		t.Fatalf("%d graphics, %d reference pages", len(graphics), len(pages))
	}
	for i, g := range graphics {
		total, _ := strconv.Atoi(g[1])
		stride, _ := strconv.Atoi(g[3])
		if stride != 102 || total != 102*1218 {
			t.Fatalf("graphic %d: %d bytes, %d per row", i, total, stride)
		}
		if got := decodeGraphic(t, g[4], total, stride); !bytes.Equal(got, []byte(pages[i%2])) {
			t.Fatalf("label %d differs from the rendered page", i)
		}
		if len(g[4]) > total/4 {
			t.Errorf("label %d: %d characters for %d bytes", i, len(g[4]), total)
		}
	}

	opts.Options = map[string]string{"darkness": "40"}
	if err := b.Print(ctx, bytes.NewReader(pdf), opts, &out); err == nil {
		t.Fatal("darkness 40 accepted")
	}
}
