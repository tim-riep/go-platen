// Package brother encodes 1-bit bitmaps in the print data format of the
// Brother HL-2135W. The format is described in docs/brother-raster-format.md,
// which was derived from captured print data streams and the public HP
// PJL/PCL documentation; this encoder is based on that document only.
package brother

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/tim-riep/go-platen/internal/backend/raster"
	"github.com/tim-riep/go-platen/internal/render"
)

// Name is the registry name of the backend.
const Name = "brother-raster"

// Factory registers the backend: PDF rendered at printer resolution and
// encoded for the Brother HL-2135W. Gray is rendered as a clustered-dot
// halftone by default: the laser does not develop the isolated pixels of
// error diffusion in light areas (test prints, 2026-10-30).
var Factory = raster.NewFactoryWithDefaults(Name, func() raster.Encoder { return &Encoder{} },
	map[string]string{"dither": string(render.Halftone)})

// Format is the page geometry for one paper size and resolution, as
// measured in the captures (docs/brother-raster-format.md, section 2.3).
type Format struct {
	Paper string
	DPI   int
	// PageSizeCode is the value of the PCL page size command ESC&l#A.
	PageSizeCode int
	// Rows is the number of raster rows per page.
	Rows int
	// RowBytes is the row length; only bytes DataFirst to DataEnd-1 carry
	// data, the driver never writes the others.
	RowBytes, DataFirst, DataEnd int
	// OriginX and OriginY locate raster pixel (0, 0) on the paper bitmap.
	OriginX, OriginY int
}

// Formats lists the paper sizes and resolutions covered by captures.
var Formats = []Format{
	{Paper: "A4", DPI: 300, PageSizeCode: 26, Rows: 3408, RowBytes: 300, DataFirst: 1, DataEnd: 299, OriginX: 40, OriginY: 50},
	{Paper: "A4", DPI: 600, PageSizeCode: 26, Rows: 6816, RowBytes: 600, DataFirst: 2, DataEnd: 598, OriginX: 80, OriginY: 100},
	{Paper: "A4", DPI: 1200, PageSizeCode: 26, Rows: 13631, RowBytes: 1200, DataFirst: 5, DataEnd: 1196, OriginX: 160, OriginY: 200},
	{Paper: "Letter", DPI: 600, PageSizeCode: 2, Rows: 6400, RowBytes: 617, DataFirst: 2, DataEnd: 615, OriginX: 80, OriginY: 100},
	{Paper: "A5", DPI: 600, PageSizeCode: 25, Rows: 4761, RowBytes: 417, DataFirst: 2, DataEnd: 415, OriginX: 80, OriginY: 100},
}

// FormatFor returns the format for a paper size and resolution.
func FormatFor(paper string, dpi int) (Format, error) {
	for _, f := range Formats {
		if strings.EqualFold(f.Paper, paper) && f.DPI == dpi {
			return f, nil
		}
	}
	var have []string
	for _, f := range Formats {
		have = append(have, fmt.Sprintf("%s at %d dpi", f.Paper, f.DPI))
	}
	return Format{}, fmt.Errorf("%w: %s at %d dpi is not supported (supported: %s; see docs/brother-raster-format.md)",
		errUnsupported, paper, dpi, strings.Join(have, ", "))
}

// Segmentation limits (section 3).
const (
	// Only segments of blank rows hold more than 16 rows, as in the
	// driver's streams.
	maxBlankRows   = 128
	maxEditRows    = 16
	maxSegmentSize = 3900
	// bandRows is the printer's band height. Segments never cross a band
	// boundary, and the first row of a band does not depend on any
	// previous row.
	bandRows = 128
	maxEdits = 254
)

const uel = "\x1b%-12345X"

// Paper sources (printer option "source") and their PCL commands
// (section 1.2). "tray" is what the HL-2135W driver sent in the first
// captures and what the test prints used.
var sources = map[string]string{
	"tray":   "\x1b&l1h1001H",
	"auto":   "\x1b&l7H",
	"manual": "\x1b&l2H",
}

// Media types (printer option "media"): PJL value and PCL media name.
var media = map[string][2]string{
	"plain": {"REGULAR", "Regular"},
	"thick": {"THICK", "Thick"},
}

// Encoder implements raster.Encoder.
type Encoder struct {
	f       Format
	jobName string
	pages   int
	setup   string
	seed    []byte
	row     []byte
	w       *bufio.Writer
}

var errUnsupported = errors.New("brother-raster")

func option(opts map[string]string, key, def string, allowed ...string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(opts[key]))
	if v == "" {
		return def, nil
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", fmt.Errorf("%w: option %s must be one of %s, not %q", errUnsupported, key, strings.Join(allowed, ", "), v)
}

// Begin validates the job and writes the PJL header.
func (e *Encoder) Begin(out io.Writer, job raster.Job) error {
	dpi := job.ResolutionDPI
	if dpi == 0 {
		dpi = 600
	}
	f, err := FormatFor(job.Paper.Name, dpi)
	if err != nil {
		return err
	}
	source, err := option(job.Options, "source", "tray", "tray", "auto", "manual")
	if err != nil {
		return err
	}
	mediaType, err := option(job.Options, "media", "plain", "plain", "thick")
	if err != nil {
		return err
	}
	tonerSave, err := option(job.Options, "toner_save", "off", "off", "on")
	if err != nil {
		return err
	}
	m := media[mediaType]
	e.f = f
	e.setup = fmt.Sprintf("\x1b&u%dD\x1b*t%dR\x1b&n%dWd%s%s\x1b&l0S\x1b&l1X", dpi, dpi, len(m[1])+1, m[1], sources[source])
	e.jobName = jobName(job.Title, job.JobID)
	e.w = bufio.NewWriterSize(out, 64<<10)
	e.seed = make([]byte, f.RowBytes)
	e.row = make([]byte, f.RowBytes)
	fmt.Fprintf(e.w, "%s@PJL \n@PJL SET REPRINT=OFF\n@PJL SET HOLD=OFF\n@PJL SET USERNAME=\"\"\r\n", uel)
	fmt.Fprintf(e.w, "@PJL SET JOBNAME=\"%s\"\r\n@PJL JOB NAME=\"%s\"\r\n", e.jobName, e.jobName)
	fmt.Fprintf(e.w, "@PJL SET STRINGCODESET=HPROMAN8\n@PJL SET ECONOMODE=%s\n@PJL SET MEDIATYPE=%s\n", strings.ToUpper(tonerSave), m[0])
	fmt.Fprintf(e.w, "@PJL SET RESOLUTION=%d\n@PJL ENTER LANGUAGE=PCL\n", dpi)
	return e.w.Flush()
}

// jobName returns a PJL-safe job name: printable ASCII without quotes.
func jobName(title, id string) string {
	var b strings.Builder
	for _, r := range title {
		if r >= 0x20 && r < 0x7f && r != '"' {
			b.WriteRune(r)
		}
		if b.Len() >= 80 {
			break
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		return s
	}
	return "platen " + id
}

// Page writes one page. The bitmap is the whole sheet at the job's
// resolution; the part inside the printable area is sent.
func (e *Encoder) Page(out io.Writer, bm *render.Bitmap) error {
	f := e.f
	if bm.DPI != f.DPI {
		return fmt.Errorf("%w: bitmap has %d dpi, want %d", errUnsupported, bm.DPI, f.DPI)
	}
	if e.pages == 0 {
		io.WriteString(e.w, e.setup)
	}
	fmt.Fprintf(e.w, "\x1b&l0O\x1b&l4096A\x1b&l%da6d1E\x1b&l0U\x1b&l0Z\x1b*p0X\x1b*p0Y\x1b*b1030M", f.PageSizeCode)
	e.pages++

	rows := make([][]byte, f.Rows)
	for y := range rows {
		extractRow(e.row, bm, f.OriginY+y, f.OriginX)
		clear(e.row[:f.DataFirst])
		clear(e.row[f.DataEnd:])
		if y%bandRows == 0 {
			// The first row of a band rewrites the whole data area
			// (section 2.2).
			rows[y] = EncodeBandStart(nil, e.row, f.DataFirst, f.DataEnd)
		} else {
			rows[y] = EncodeRow(nil, e.seed, e.row)
		}
		if rows[y][0] == blankRow {
			clear(e.seed)
		} else {
			copy(e.seed, e.row)
		}
	}
	writeSegments(e.w, rows)
	e.w.WriteByte(0x0c)
	return e.w.Flush()
}

// End writes the job end.
func (e *Encoder) End(out io.Writer) error {
	fmt.Fprintf(e.w, "%s@PJL EOJ NAME=\"%s\"\r\n%s", uel, e.jobName, uel)
	return e.w.Flush()
}

// extractRow copies len(dst) bytes of bitmap row y starting at pixel x0
// into dst. Pixels outside the bitmap are white.
func extractRow(dst []byte, bm *render.Bitmap, y, x0 int) {
	clear(dst)
	if y < 0 || y >= bm.Height {
		return
	}
	src := bm.Row(y)
	shift := uint(x0 % 8)
	first := x0 / 8
	for i := range dst {
		k := first + i
		var v byte
		if k < len(src) {
			v = src[k] << shift
		}
		if shift != 0 && k+1 < len(src) {
			v |= src[k+1] >> (8 - shift)
		}
		dst[i] = v
	}
	// Clear bits beyond the bitmap width.
	if last := bm.Width - x0; last < len(dst)*8 {
		for x := max(last, 0); x < len(dst)*8; x++ {
			dst[x/8] &^= 0x80 >> (x % 8)
		}
	}
}

// Row codes (section 2.2).
const (
	blankRow  = 0xff
	repeatRow = 0x00
)

type edit struct {
	off, cnt int  // offset relative to the end of the previous edit, length
	run      bool // repeated byte
	start    int  // absolute start in the row
}

// EncodeBandStart appends the encoding of the first row of a band to dst:
// a blank row, or edits that write every byte of the data area (bytes
// first to end-1), so that the result does not depend on the printer's
// seed row.
func EncodeBandStart(dst, row []byte, first, end int) []byte {
	if allZero(row) {
		return append(dst, blankRow)
	}
	// A pseudo seed that differs from row in every data byte.
	seed := make([]byte, len(row))
	for i := first; i < end && i < len(row); i++ {
		seed[i] = ^row[i]
	}
	return EncodeRow(dst, seed, row)
}

// EncodeRow appends the encoding of row against seed to dst.
func EncodeRow(dst, seed, row []byte) []byte {
	if allZero(row) {
		return append(dst, blankRow)
	}
	edits := diffEdits(seed, row)
	if len(edits) > maxEdits {
		// As the driver does: the last edit is a literal that covers
		// everything up to the end of the last change (section 2.2).
		last := edits[len(edits)-1]
		end := last.start + last.cnt
		cut := edits[maxEdits-1]
		cut.run, cut.cnt = false, end-cut.start
		edits = append(edits[:maxEdits-1], cut)
	}
	if len(edits) == 0 {
		return append(dst, repeatRow)
	}
	dst = append(dst, byte(len(edits)))
	for _, ed := range edits {
		if ed.run {
			dst = appendCommand(dst, 0x80, ed.off, 3, 5, ed.cnt-2, 31)
			dst = append(dst, row[ed.start])
		} else {
			dst = appendCommand(dst, 0x00, ed.off, 15, 3, ed.cnt-1, 7)
			dst = append(dst, row[ed.start:ed.start+ed.cnt]...)
		}
	}
	return dst
}

// appendCommand writes a command byte with an offset field (maximum
// offMax, at bit position countBits) and a count field (maximum cntMax),
// followed by the extension bytes of both fields.
func appendCommand(dst []byte, flag byte, off, offMax int, countBits uint, cnt, cntMax int) []byte {
	dst = append(dst, flag|byte(min(off, offMax))<<countBits|byte(min(cnt, cntMax)))
	dst = appendExtension(dst, off, offMax)
	return appendExtension(dst, cnt, cntMax)
}

func appendExtension(dst []byte, v, fieldMax int) []byte {
	if v < fieldMax {
		return dst
	}
	rem := v - fieldMax
	for rem >= 255 {
		dst = append(dst, 255)
		rem -= 255
	}
	return append(dst, byte(rem))
}

// minRun is the shortest stretch of equal bytes sent as a repeated byte.
const minRun = 3

// diffEdits finds the changed byte ranges and splits them into runs of at
// least minRun equal bytes and literal stretches.
func diffEdits(seed, row []byte) []edit {
	var ranges [][2]int
	for i := 0; i < len(row); {
		if row[i] == seed[i] {
			i++
			continue
		}
		j := i
		for j < len(row) && row[j] != seed[j] {
			j++
		}
		ranges = append(ranges, [2]int{i, j})
		i = j
	}
	if len(ranges) == 0 {
		return nil
	}
	var edits []edit
	pos := 0
	add := func(start, end int, run bool) {
		edits = append(edits, edit{off: start - pos, cnt: end - start, run: run, start: start})
		pos = end
	}
	for _, r := range ranges {
		lit := r[0]
		for i := r[0]; i < r[1]; {
			j := i + 1
			for j < r[1] && row[j] == row[i] {
				j++
			}
			if j-i >= minRun {
				if lit < i {
					add(lit, i, false)
				}
				add(i, j, true)
				lit = j
			}
			i = j
		}
		if lit < r[1] {
			add(lit, r[1], false)
		}
	}
	return edits
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// writeSegments groups encoded rows into segments and writes the raster
// transfer commands (section 2.1 and 3).
func writeSegments(w *bufio.Writer, rows [][]byte) {
	blankRun := func(i int) int {
		n := 0
		for i+n < len(rows) && len(rows[i+n]) == 1 && rows[i+n][0] == blankRow {
			n++
		}
		return n
	}
	var pending [][][]byte // blank segments waiting to be combined
	flushBlank := func() {
		for k, seg := range pending {
			if k == 0 {
				w.WriteString("\x1b*b")
			}
			term := byte('w')
			if k == len(pending)-1 {
				term = 'W'
			}
			writeSegment(w, seg, term)
		}
		pending = nil
	}
	for i := 0; i < len(rows); {
		if t := blankRun(i); t >= maxEditRows || (t > 0 && i+t == len(rows)) {
			for t > 0 {
				n := min(t, maxBlankRows, bandRows-i%bandRows)
				pending = append(pending, rows[i:i+n])
				i += n
				t -= n
			}
			continue
		}
		flushBlank()
		n, size := 0, 2
		for i+n < len(rows) && n < maxEditRows {
			if n > 0 && (i+n)%bandRows == 0 {
				break
			}
			if n > 0 && size+len(rows[i+n]) > maxSegmentSize {
				break
			}
			if n > 0 && blankRun(i+n) >= maxEditRows {
				break
			}
			size += len(rows[i+n])
			n++
		}
		w.WriteString("\x1b*b")
		writeSegment(w, rows[i:i+n], 'W')
		i += n
	}
	flushBlank()
}

func writeSegment(w *bufio.Writer, rows [][]byte, term byte) {
	size := 2
	for _, r := range rows {
		size += len(r)
	}
	fmt.Fprintf(w, "%d%c", size, term)
	if len(rows) > math.MaxUint16 {
		panic("brother: too many rows in segment")
	}
	w.WriteByte(byte(len(rows) >> 8))
	w.WriteByte(byte(len(rows)))
	for _, r := range rows {
		w.Write(r)
	}
}
