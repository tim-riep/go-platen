package brother

import (
	"bytes"
	"context"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
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
		if enc[0] != blankRow && len(diffEdits(seed, row)) > maxEdits && enc[0] != maxEdits {
			t.Fatalf("over-full row encoded with %d edits, want %d", enc[0], maxEdits)
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

// a4 is the format most tests use; the names below keep them short.
var a4 = func() Format {
	f, err := FormatFor("A4", 600)
	if err != nil {
		panic(err)
	}
	return f
}()

const DPI = 600

var (
	RowBytes  = a4.RowBytes
	DataFirst = a4.DataFirst
	DataEnd   = a4.DataEnd
	OriginX   = a4.OriginX
	OriginY   = a4.OriginY
)

func paperFor(t *testing.T, f Format) render.Paper {
	t.Helper()
	p, err := render.ParsePaper(f.Paper)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustA4(t *testing.T) render.Paper { return paperFor(t, a4) }

func encodeJob(t *testing.T, f Format, opts map[string]string, title string, pages []*render.Bitmap) []byte {
	t.Helper()
	var out bytes.Buffer
	e := &Encoder{}
	job := raster.Job{JobOptions: backend.JobOptions{Title: title, ResolutionDPI: f.DPI, Options: opts}, Paper: paperFor(t, f)}
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

func encodePages(t *testing.T, title string, pages []*render.Bitmap) []byte {
	t.Helper()
	return encodeJob(t, a4, nil, title, pages)
}

// sheet places decoded raster rows on a sheet of the format's paper size at
// the raster origin.
func sheet(t *testing.T, f Format, rows [][]byte) *render.Bitmap {
	w, h := paperFor(t, f).Pixels(f.DPI)
	bm := render.NewBitmap(w, h, f.DPI)
	for y, r := range rows {
		for i, v := range r {
			for v != 0 {
				b := bits.LeadingZeros8(v)
				v &^= 0x80 >> b
				x := i*8 + b
				if f.OriginX+x < w && f.OriginY+y < h {
					bm.Set(f.OriginX+x, f.OriginY+y)
				}
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
func checkStructure(t *testing.T, s *inspect.Stream, f Format) {
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
			if total/bandRows != (total+sg.Rows-1)/bandRows {
				t.Errorf("page %d: segment at row %d with %d rows crosses a band boundary", n+1, total, sg.Rows)
			}
			total += sg.Rows
			blank := len(sg.Data) == sg.Rows+2 && bytes.Count(sg.Data[2:], []byte{blankRow}) == sg.Rows
			if sg.Rows < 1 || sg.Rows > maxBlankRows || (!blank && sg.Rows > maxEditRows) || len(sg.Data) > maxSegmentSize+f.RowBytes*2 {
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
		if total != f.Rows {
			t.Errorf("page %d: %d rows, want %d", n+1, total, f.Rows)
		}
	}
}

// captureJob derives the format and the printer options of a captured job.
func captureJob(t *testing.T, s *inspect.Stream) (Format, map[string]string) {
	t.Helper()
	dpi, code := 0, 0
	opts := map[string]string{}
	for _, c := range s.Pages[0].Commands {
		switch {
		case c.Is('&', 'u', 'D'):
			dpi = c.Int()
		case c.Is('&', 'l', 'A') && c.Int() != 4096:
			code = c.Int()
		case c.Is('&', 'l', 'H') && c.Int() == 7:
			opts["source"] = "auto"
		case c.Is('&', 'l', 'H') && c.Int() == 2:
			opts["source"] = "manual"
		}
	}
	for _, l := range s.PJL {
		if l == "@PJL SET MEDIATYPE=THICK" {
			opts["media"] = "thick"
		}
		if l == "@PJL SET ECONOMODE=ON" {
			opts["toner_save"] = "on"
		}
	}
	for _, f := range Formats {
		if f.DPI == dpi && f.PageSizeCode == code {
			return f, opts
		}
	}
	t.Fatalf("no format for %d dpi, page size %d", dpi, code)
	return Format{}, nil
}

var copiesCommand = regexp.MustCompile(`^ESC &l\d+X$`)

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
			f, opts := captureJob(t, capture)
			var sheets []*render.Bitmap
			var captured [][][]byte
			for _, p := range capture.Pages {
				rows, _, err := inspect.DecodeRows(p, f.RowBytes)
				if err != nil {
					t.Fatal(err)
				}
				captured = append(captured, rows)
				sheets = append(sheets, sheet(t, f, rows))
			}

			enc := encodeJob(t, f, opts, "test", sheets)
			got, err := inspect.Parse(enc)
			if err != nil {
				t.Fatal(err)
			}
			checkStructure(t, got, f)

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
				// The driver sends copies as ESC&l#X; platen repeats the
				// pages instead and always sends ESC&l1X.
				want := commandStrings(capture.Pages[n].Commands)
				for i, c := range want {
					if copiesCommand.MatchString(c) {
						want[i] = "ESC &l1X"
					}
				}
				if b := commandStrings(got.Pages[n].Commands); fmt.Sprint(want) != fmt.Sprint(b) {
					t.Errorf("page %d commands differ:\ncapture %v\nencoder %v", n+1, want, b)
				}
				rows, _, err := inspect.DecodeRows(got.Pages[n], f.RowBytes)
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
			t.Logf("%s at %d dpi: raster data: encoder %d bytes, driver %d bytes (%.1f%%)", f.Paper, f.DPI, encSize, capSize, 100*float64(encSize)/float64(capSize))
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
	checkStructure(t, s, a4)
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
	legal, _ := render.ParsePaper("Legal")
	if err := e.Begin(&bytes.Buffer{}, raster.Job{Paper: legal}); err == nil {
		t.Error("Legal accepted")
	}
	if err := e.Begin(&bytes.Buffer{}, raster.Job{Paper: mustA4(t), JobOptions: backend.JobOptions{ResolutionDPI: 150}}); err == nil {
		t.Error("150 dpi accepted")
	}
	if err := e.Begin(&bytes.Buffer{}, raster.Job{Paper: letter, JobOptions: backend.JobOptions{ResolutionDPI: 300}}); err == nil {
		t.Error("Letter at 300 dpi accepted")
	}
	for _, opts := range []map[string]string{{"source": "tray2"}, {"media": "glossy"}, {"toner_save": "yes"}} {
		if err := e.Begin(&bytes.Buffer{}, raster.Job{Paper: mustA4(t), JobOptions: backend.JobOptions{ResolutionDPI: 600, Options: opts}}); err == nil {
			t.Errorf("option %v accepted", opts)
		}
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
	checkStructure(t, s, a4)
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

// Every band must decode without the rows of the previous band, as in the
// captures; a stream relying on the seed across a band boundary printed
// only its first band on the HL-2135W.
func TestBandsAreSelfContained(t *testing.T) {
	w, h := mustA4(t).Pixels(DPI)
	bm := render.NewBitmap(w, h, DPI)
	// Vertical lines across many bands.
	for y := 300; y < 3000; y++ {
		bm.Set(600, y)
		bm.Set(2000, y)
	}
	s, err := inspect.Parse(encodePages(t, "bands", []*render.Bitmap{bm}))
	if err != nil {
		t.Fatal(err)
	}
	checkStructure(t, s, a4)
	full, _, err := inspect.DecodeRows(s.Pages[0], RowBytes)
	if err != nil {
		t.Fatal(err)
	}
	// The decoder starts every band with a blank seed, so a band start that
	// relied on the previous band would lose the lines. Band starts must
	// also write exactly the data area, as the driver does.
	for y := 0; y < len(full); y += bandRows {
		x := 600 - OriginX
		if y+OriginY >= 300 && y+OriginY < 3000 && full[y][x/8]&(0x80>>(x%8)) == 0 {
			t.Fatalf("band starting at row %d lost the line", y)
		}
	}
}

func TestBandStartWritesDataArea(t *testing.T) {
	row := make([]byte, RowBytes)
	row[100], row[300] = 0x0f, 0xf0
	enc := EncodeBandStart(nil, row, DataFirst, DataEnd)
	// Decode it after a row full of ink: the result must still be row,
	// whatever the seed is.
	ink := make([]byte, RowBytes)
	for i := DataFirst; i < DataEnd; i++ {
		ink[i] = 0xff
	}
	data := append([]byte{0, 2}, EncodeRow(nil, make([]byte, RowBytes), ink)...)
	data = append(data, enc...)
	p := &inspect.Page{Segments: []inspect.Segment{{Rows: 2, Data: data}}}
	got, _, err := inspect.DecodeRows(p, RowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[1], row) {
		t.Fatal("band start row depends on the seed")
	}
	written := writtenBytes(t, enc)
	for i := 0; i < RowBytes; i++ {
		if written[i] != (i >= DataFirst && i < DataEnd) {
			t.Fatalf("byte %d written: %v", i, written[i])
		}
	}
	if !bytes.Equal(EncodeBandStart(nil, make([]byte, RowBytes), DataFirst, DataEnd), []byte{blankRow}) {
		t.Fatal("blank band start")
	}
}

// writtenBytes reports which row bytes an encoded row writes.
func writtenBytes(t *testing.T, enc []byte) []bool {
	t.Helper()
	w := make([]bool, RowBytes)
	ext := func(p *int, v, m int) int {
		if v == m {
			for {
				b := enc[*p]
				*p++
				v += int(b)
				if b != 255 {
					break
				}
			}
		}
		return v
	}
	p, cur := 1, 0
	for k := 0; k < int(enc[0]); k++ {
		c := enc[p]
		p++
		var off, n int
		if c&0x80 != 0 {
			off = ext(&p, int(c>>5)&3, 3)
			n = ext(&p, int(c&31), 31) + 2
			p++
		} else {
			off = ext(&p, int(c>>3)&15, 15)
			n = ext(&p, int(c&7), 7) + 1
			p += n
		}
		cur += off
		for i := cur; i < cur+n; i++ {
			w[i] = true
		}
		cur += n
	}
	return w
}

func TestHalftoneIsDefault(t *testing.T) {
	b, err := Factory.New()
	if err != nil {
		t.Fatal(err)
	}
	// A page filled with 25 % gray.
	pdf := makePDF(595.276, 841.890, "0.75 g 0 0 595.276 841.890 re f")
	coverage := func(opts map[string]string) float64 {
		var out bytes.Buffer
		if err := b.Print(context.Background(), bytes.NewReader(pdf), backend.JobOptions{PaperSize: "A4", ResolutionDPI: 600, Options: opts}, &out); err != nil {
			t.Fatal(err)
		}
		s, err := inspect.Parse(out.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		rows, _, err := inspect.DecodeRows(s.Pages[0], RowBytes)
		if err != nil {
			t.Fatal(err)
		}
		black := 0
		for _, r := range rows[1000:1100] {
			for _, v := range r[100:500] {
				for ; v != 0; v &= v - 1 {
					black++
				}
			}
		}
		return float64(black) / (100 * 400 * 8)
	}
	if c := coverage(nil); c < 0.15 || c > 0.35 {
		t.Errorf("default: gray rendered with coverage %.2f, want about 0.25", c)
	}
	if c := coverage(map[string]string{"dither": "threshold"}); c != 0 {
		t.Errorf("threshold option ignored: coverage %.2f", c)
	}
}
