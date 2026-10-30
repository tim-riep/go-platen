package brother

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim-riep/go-platen/internal/backend"
	"github.com/tim-riep/go-platen/internal/backend/raster"
	"github.com/tim-riep/go-platen/internal/brother/inspect"
	"github.com/tim-riep/go-platen/internal/render"
)

// decodeOne decodes a single encoded row against seed with the analysis
// tool's decoder.
func decodeOne(t *testing.T, seed, enc []byte) []byte {
	t.Helper()
	// Prepend a row that sets the seed: blank, then the seed as edits.
	setup := EncodeRow(nil, make([]byte, len(seed)), seed)
	data := append([]byte{0, 2}, setup...)
	data = append(data, enc...)
	p := &inspect.Page{Segments: []inspect.Segment{{Rows: 2, Data: data}}}
	rows, _, err := inspect.DecodeRows(p, len(seed))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rows[1]
}

func TestEncodeRowRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	gen := func(kind int) []byte {
		b := make([]byte, RowBytes)
		switch kind {
		case 0: // sparse
			for i := 0; i < 20; i++ {
				b[r.IntN(len(b))] = byte(r.IntN(256))
			}
		case 1: // runs
			for i := 0; i < len(b); {
				n := 1 + r.IntN(600)
				v := []byte{0, 0xff, byte(r.IntN(256))}[r.IntN(3)]
				for k := 0; k < n && i < len(b); k++ {
					b[i] = v
					i++
				}
			}
		case 2: // noise, needs more than 254 edits unless merged
			for i := range b {
				b[i] = byte(r.IntN(256))
			}
		case 3: // alternating changed / unchanged bytes
			for i := 0; i < len(b); i += 2 {
				b[i] = 0x55
			}
		}
		return b
	}
	for i := 0; i < 2000; i++ {
		seed, row := gen(r.IntN(4)), gen(r.IntN(4))
		if r.IntN(4) == 0 {
			copy(row, seed)
			row[r.IntN(len(row))] ^= 0x10
		}
		enc := EncodeRow(nil, seed, row)
		if enc[0] != blankRow && int(enc[0]) > maxEdits {
			t.Fatalf("%d edits", enc[0])
		}
		if got := decodeOne(t, seed, enc); !bytes.Equal(got, row) {
			t.Fatalf("iteration %d: round trip mismatch", i)
		}
	}
	// Special rows.
	zero := make([]byte, RowBytes)
	if enc := EncodeRow(nil, gen(1), zero); !bytes.Equal(enc, []byte{blankRow}) {
		t.Fatalf("blank row: %x", enc)
	}
	same := gen(1)
	if enc := EncodeRow(nil, same, same); !bytes.Equal(enc, []byte{repeatRow}) {
		t.Fatalf("repeat row: %x", enc)
	}
	// Extension chains: a run over the whole row needs 255-continuations.
	full := bytes.Repeat([]byte{0xff}, RowBytes)
	if got := decodeOne(t, zero, EncodeRow(nil, zero, full)); !bytes.Equal(got, full) {
		t.Fatal("full row")
	}
}

// The example row from section 2.2 of the format documentation.
func TestDocumentedExample(t *testing.T) {
	seed := make([]byte, RowBytes)
	row := make([]byte, RowBytes)
	for i := 64; i < 64+488; i++ {
		row[i] = 0xff
	}
	row[552] = 0xc0
	want := []byte{0x02, 0xff, 0x3d, 0xff, 0xc8, 0xff, 0x00, 0xc0}
	if got := EncodeRow(nil, seed, row); !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
}

func encodePages(t *testing.T, title string, pages []*render.Bitmap) []byte {
	t.Helper()
	var out bytes.Buffer
	e := &Encoder{}
	job := raster.Job{JobOptions: backend.JobOptions{Title: title, ResolutionDPI: DPI}, Paper: mustA4(t)}
	if err := e.Begin(&out, job); err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		if err := e.Page(&out, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.End(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustA4(t *testing.T) render.Paper {
	p, err := render.ParsePaper("A4")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sheet places decoded raster rows on an A4 bitmap at the raster origin.
func sheet(t *testing.T, rows [][]byte) *render.Bitmap {
	w, h := mustA4(t).Pixels(DPI)
	bm := render.NewBitmap(w, h, DPI)
	for y, r := range rows {
		for x := 0; x < len(r)*8; x++ {
			if r[x/8]&(0x80>>(x%8)) != 0 && OriginX+x < w {
				bm.Set(OriginX+x, OriginY+y)
			}
		}
	}
	return bm
}

func commandStrings(cmds []inspect.Command) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c.String()
	}
	return out
}

// checkStructure verifies the documented structure of an encoded stream.
func checkStructure(t *testing.T, s *inspect.Stream) {
	t.Helper()
	if s.UELs != 3 {
		t.Errorf("%d UELs", s.UELs)
	}
	for n, p := range s.Pages {
		if !p.FormFeed || p.Compression != 1030 {
			t.Errorf("page %d: form feed %v, compression %d", n+1, p.FormFeed, p.Compression)
		}
		total := 0
		for _, sg := range p.Segments {
			total += sg.Rows
			blank := len(sg.Data) == sg.Rows+2 && bytes.Count(sg.Data[2:], []byte{blankRow}) == sg.Rows
			if sg.Rows < 1 || sg.Rows > maxBlankRows || (!blank && sg.Rows > maxEditRows) || len(sg.Data) > maxSegmentSize+RowBytes*2 {
				t.Errorf("page %d: segment with %d rows and %d bytes (blank only: %v)", n+1, sg.Rows, len(sg.Data), blank)
			}
		}
		if len(p.Segments) != p.RasterGroups {
			// Only blank segments are combined in one escape sequence;
			// checked indirectly: combined segments must be blank.
			for _, sg := range p.Segments {
				if sg.Rows > maxEditRows && bytes.Count(sg.Data[2:], []byte{blankRow}) != sg.Rows {
					t.Errorf("page %d: long segment with non-blank rows", n+1)
				}
			}
		}
		if total != PageRows {
			t.Errorf("page %d: %d rows", n+1, total)
		}
	}
}

func TestCaptures(t *testing.T) {
	dirs, _ := filepath.Glob("../../testdata/captures/*/capture.prn")
	if len(dirs) == 0 {
		t.Skip("no captures in testdata/captures")
	}
	for _, path := range dirs {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			capture, err := inspect.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			var sheets []*render.Bitmap
			var captured [][][]byte
			for _, p := range capture.Pages {
				rows, _, err := inspect.DecodeRows(p, RowBytes)
				if err != nil {
					t.Fatal(err)
				}
				captured = append(captured, rows)
				sheets = append(sheets, sheet(t, rows))
			}

			enc := encodePages(t, "test", sheets)
			got, err := inspect.Parse(enc)
			if err != nil {
				t.Fatal(err)
			}
			checkStructure(t, got)

			// PJL: the same commands except the job log and the job name.
			filter := func(lines []string) []string {
				var out []string
				for _, l := range lines {
					if strings.Contains(l, "PRINTLOG") || strings.Contains(l, "NAME=") {
						continue
					}
					out = append(out, l)
				}
				return out
			}
			if a, b := filter(capture.PJL), filter(got.PJL); fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("PJL header differs:\ncapture %q\nencoder %q", a, b)
			}
			if len(got.Trailer) != 1 || !strings.HasPrefix(got.Trailer[0], "@PJL EOJ NAME=") {
				t.Errorf("trailer %q", got.Trailer)
			}

			if len(got.Pages) != len(capture.Pages) {
				t.Fatalf("%d pages, capture has %d", len(got.Pages), len(capture.Pages))
			}
			encSize, capSize := 0, 0
			for n := range got.Pages {
				if a, b := commandStrings(capture.Pages[n].Commands), commandStrings(got.Pages[n].Commands); fmt.Sprint(a) != fmt.Sprint(b) {
					t.Errorf("page %d commands differ:\ncapture %v\nencoder %v", n+1, a, b)
				}
				rows, _, err := inspect.DecodeRows(got.Pages[n], RowBytes)
				if err != nil {
					t.Fatalf("page %d: %v", n+1, err)
				}
				for y := range rows {
					if !bytes.Equal(rows[y], captured[n][y]) {
						t.Fatalf("page %d row %d differs from the capture", n+1, y)
					}
				}
				for _, sg := range got.Pages[n].Segments {
					encSize += len(sg.Data)
				}
				for _, sg := range capture.Pages[n].Segments {
					capSize += len(sg.Data)
				}
			}
			t.Logf("raster data: encoder %d bytes, driver %d bytes (%.1f%%)", encSize, capSize, 100*float64(encSize)/float64(capSize))
		})
	}
}

func TestBackendRendersPDF(t *testing.T) {
	b, err := Factory.New()
	if err != nil {
		t.Fatal(err)
	}
	// A4 with a black square of 1 inch at 1 inch from the left and top edge.
	pdf := makePDF(595.276, 841.890, "0 0 0 rg 72 697.89 72 72 re f")
	var out bytes.Buffer
	if err := b.Print(context.Background(), bytes.NewReader(pdf), backend.JobOptions{
		Title: "Square \"test\" ä", Copies: 2, PaperSize: "A4", ResolutionDPI: 600,
	}, &out); err != nil {
		t.Fatal(err)
	}
	s, err := inspect.Parse(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	checkStructure(t, s)
	if len(s.Pages) != 2 {
		t.Fatalf("%d pages, want 2 copies", len(s.Pages))
	}
	if s.PJL[4] != `@PJL SET JOBNAME="Square test"` {
		t.Errorf("job name line %q", s.PJL[4])
	}
	rows, _, err := inspect.DecodeRows(s.Pages[0], RowBytes)
	if err != nil {
		t.Fatal(err)
	}
	black := func(x, y int) bool { // paper coordinates
		rx, ry := x-OriginX, y-OriginY
		return rows[ry][rx/8]&(0x80>>(rx%8)) != 0
	}
	if !black(900, 900) || !black(605, 605) || !black(1195, 1195) {
		t.Error("square missing")
	}
	if black(590, 900) || black(1210, 900) || black(900, 590) || black(900, 1210) {
		t.Error("square too large or misplaced")
	}
}

func TestUnsupported(t *testing.T) {
	e := &Encoder{}
	letter, _ := render.ParsePaper("Letter")
	if err := e.Begin(&bytes.Buffer{}, raster.Job{Paper: letter}); err == nil {
		t.Error("Letter accepted")
	}
	if err := e.Begin(&bytes.Buffer{}, raster.Job{Paper: mustA4(t), JobOptions: backend.JobOptions{ResolutionDPI: 300}}); err == nil {
		t.Error("300 dpi accepted")
	}
	if got := jobName("", "abc"); got != "platen abc" {
		t.Error(got)
	}
}

func TestExtractRow(t *testing.T) {
	bm := render.NewBitmap(100, 1, DPI)
	bm.Set(76, 0)
	bm.Set(83, 0)
	bm.Set(99, 0)
	dst := make([]byte, 4)
	extractRow(dst, bm, 0, 76)
	// pixels 0, 7 and 23 of the extracted row; nothing beyond the bitmap.
	if dst[0] != 0x81 || dst[1] != 0 || dst[2] != 0x01 || dst[3] != 0 {
		t.Fatalf("% x", dst)
	}
}

// Rows that repeat the previous row must not form segments longer than 16
// rows; the printer stopped printing on such a segment.
func TestRepeatedRowsInShortSegments(t *testing.T) {
	w, h := mustA4(t).Pixels(DPI)
	bm := render.NewBitmap(w, h, DPI)
	// A vertical line 2000 rows long: one edited row, then repeats.
	for y := 500; y < 2500; y++ {
		bm.Set(1000, y)
	}
	s, err := inspect.Parse(encodePages(t, "line", []*render.Bitmap{bm}))
	if err != nil {
		t.Fatal(err)
	}
	checkStructure(t, s)
	for _, sg := range s.Pages[0].Segments {
		if bytes.Count(sg.Data[2:], []byte{repeatRow}) > 0 && sg.Rows > maxEditRows {
			t.Fatalf("segment of %d rows contains repeated rows", sg.Rows)
		}
	}
	rows, _, err := inspect.DecodeRows(s.Pages[0], RowBytes)
	if err != nil {
		t.Fatal(err)
	}
	x := 1000 - OriginX
	for y := 500 - OriginY; y < 2500-OriginY; y++ {
		if rows[y][x/8]&(0x80>>(x%8)) == 0 {
			t.Fatalf("row %d: line missing", y)
		}
	}
}
