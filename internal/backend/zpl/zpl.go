// Package zpl implements the zpl backend: PDF pages rendered to 1-bit
// bitmaps and printed as ZPL graphics on Zebra label printers, e.g. PDF
// shipping labels. Each page becomes one label format (^XA ... ^XZ) with
// the page as a ^GF graphic field in ZPL's ASCII hex compression, as
// described in Zebra's ZPL programming guide.
package zpl

import (
	"bufio"
	"fmt"
	"io"
	"strconv"

	"github.com/tim-riep/go-platen/internal/backend/raster"
	"github.com/tim-riep/go-platen/internal/render"
)

// Name is the registry name of the backend.
const Name = "zpl"

// Factory registers the zpl backend. Labels print text and barcodes, so
// the default reduction to 1 bit is a threshold.
var Factory = raster.NewFactoryWithDefaults(Name, func() raster.Encoder { return &Encoder{} },
	map[string]string{"dither": string(render.Threshold)})

// Encoder writes ZPL label formats.
type Encoder struct {
	darkness string
}

// Begin reads the printer options: "darkness" (-30 to 30) changes the
// printer's darkness setting for these labels (^MD).
func (e *Encoder) Begin(out io.Writer, job raster.Job) error {
	if v := job.Options["darkness"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < -30 || n > 30 {
			return fmt.Errorf("zpl: darkness must be between -30 and 30")
		}
		e.darkness = strconv.Itoa(n)
	}
	return nil
}

// Page writes one label.
func (e *Encoder) Page(out io.Writer, bm *render.Bitmap) error {
	w := bufio.NewWriterSize(out, 64<<10)
	fmt.Fprintf(w, "^XA\n^PW%d\n^LL%d\n^LH0,0\n", bm.Width, bm.Height)
	if e.darkness != "" {
		fmt.Fprintf(w, "^MD%s\n", e.darkness)
	}
	total := bm.Stride * bm.Height
	fmt.Fprintf(w, "^FO0,0^GFA,%d,%d,%d,", total, total, bm.Stride)
	writeGraphic(w, bm)
	w.WriteString("^FS\n^XZ\n")
	return w.Flush()
}

// End finishes the job.
func (e *Encoder) End(io.Writer) error { return nil }

const hexDigits = "0123456789ABCDEF"

// writeGraphic writes the bitmap rows as compressed ASCII hex: ":" repeats
// the previous row, "," fills the rest of a row with 0 and "!" with F, and
// a run of one hex digit is written as a count followed by the digit.
func writeGraphic(w *bufio.Writer, bm *render.Bitmap) {
	hex := make([]byte, bm.Stride*2)
	var prev []byte
	for y := 0; y < bm.Height; y++ {
		row := bm.Row(y)
		if prev != nil && string(row) == string(prev) {
			w.WriteByte(':')
			continue
		}
		prev = row
		for i, b := range row {
			hex[2*i], hex[2*i+1] = hexDigits[b>>4], hexDigits[b&0x0f]
		}
		// A trailing run of 0 or F becomes "," or "!".
		end, fill := len(hex), byte(0)
		if last := hex[len(hex)-1]; last == '0' || last == 'F' {
			i := len(hex)
			for i > 0 && hex[i-1] == last {
				i--
			}
			if len(hex)-i >= 2 {
				end = i
				fill = map[byte]byte{'0': ',', 'F': '!'}[last]
			}
		}
		writeRuns(w, hex[:end])
		if fill != 0 {
			w.WriteByte(fill)
		}
	}
}

// writeRuns writes hex digits with runs of the same digit compressed.
func writeRuns(w *bufio.Writer, hex []byte) {
	for i := 0; i < len(hex); {
		j := i + 1
		for j < len(hex) && hex[j] == hex[i] && j-i < maxRun {
			j++
		}
		if n := j - i; n > 1 {
			w.WriteString(runCount(n))
		}
		w.WriteByte(hex[i])
		i = j
	}
}

// maxRun is the longest run one count can express: z (400) plus Y (19).
const maxRun = 419

// runCount encodes a repeat count: g to z are 20 to 400 in steps of 20,
// G to Y are 1 to 19; their values add up.
func runCount(n int) string {
	var s []byte
	if n >= 20 {
		s = append(s, byte('g'+n/20-1))
		n %= 20
	}
	if n > 0 {
		s = append(s, byte('G'+n-1))
	}
	return string(s)
}
