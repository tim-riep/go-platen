// Package inspect dissects Brother print data streams captured from the
// official driver: PJL header, PCL command sequences, raster blocks and page
// ends. It is the analysis tool behind docs/brother-raster-format.md and is
// used by the encoder tests to decode streams.
//
// It is based only on the captures in testdata/captures and the public HP
// PJL and PCL 5 documentation.
package inspect

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// Command is one PCL command, e.g. ESC & l 26 A. Combined sequences such
// as ESC & l 26a6d1E are split into one Command per parameter.
type Command struct {
	Offset int
	// Param and Group are the parameterized and group characters, e.g.
	// '&' and 'l'. For two-character commands (ESC E) Param is 'E' and
	// Group is 0.
	Param, Group byte
	Value        string
	// Term is the upper case termination character, e.g. 'A'.
	Term byte
	// Data holds the bytes following commands with a data block, e.g.
	// ESC & n # W.
	Data []byte
}

func (c Command) String() string {
	if c.Group == 0 {
		return "ESC " + string(c.Param)
	}
	s := fmt.Sprintf("ESC %c%c%s%c", c.Param, c.Group, c.Value, c.Term)
	if c.Data != nil {
		s += fmt.Sprintf(" %q", c.Data)
	}
	return s
}

// Is reports whether the command has the given param, group and
// termination characters.
func (c Command) Is(param, group, term byte) bool {
	return c.Param == param && c.Group == group && c.Term == term
}

// Int returns the numeric value.
func (c Command) Int() int {
	v, _ := strconv.Atoi(c.Value)
	return v
}

// Segment is one data block of an ESC * b # W (or # w) raster transfer.
type Segment struct {
	Offset int
	// Rows is the row count from the first two bytes of the block.
	Rows int
	// Data is the complete block including the two count bytes.
	Data []byte
}

// Page is the part of the stream between two page ends.
type Page struct {
	// Commands are the PCL commands before the raster data.
	Commands []Command
	// Compression is the value of ESC * b # M.
	Compression int
	Segments    []Segment
	// RasterGroups counts ESC * b ... W escape sequences; one sequence can
	// carry several segments (lower case w continues the sequence).
	RasterGroups int
	// FormFeed is set when the page was ended with a form feed.
	FormFeed bool
}

// Stream is a dissected print job.
type Stream struct {
	// PJL lists the header PJL lines (after the first UEL).
	PJL []string
	// Trailer lists the PJL lines after the closing UEL.
	Trailer []string
	Pages   []*Page
	// UELs counts Universal Exit Language sequences.
	UELs int
}

const uel = "\x1b%-12345X"

// Parse dissects a print data stream.
func Parse(data []byte) (*Stream, error) {
	s := &Stream{}
	i := 0
	var page *Page
	inPCL := false
	readPJL := func(dst *[]string) {
		for bytes.HasPrefix(data[i:], []byte("@PJL")) {
			j := bytes.IndexByte(data[i:], '\n')
			if j < 0 {
				j = len(data) - i - 1
			}
			*dst = append(*dst, string(bytes.TrimRight(data[i:i+j], "\r")))
			i += j + 1
		}
	}
	for i < len(data) {
		if bytes.HasPrefix(data[i:], []byte(uel)) {
			s.UELs++
			i += len(uel)
			if s.UELs == 1 {
				readPJL(&s.PJL)
				inPCL = true
			} else {
				readPJL(&s.Trailer)
				inPCL = false
			}
			continue
		}
		if !inPCL {
			return nil, fmt.Errorf("offset %d: unexpected data outside PCL: %q", i, clip(data[i:], 16))
		}
		if page == nil {
			page = &Page{}
			s.Pages = append(s.Pages, page)
		}
		switch data[i] {
		case 0x1b:
			n, err := parseEscape(data, i, page)
			if err != nil {
				return nil, err
			}
			i = n
		case 0x0c:
			page.FormFeed = true
			page = nil
			i++
		default:
			return nil, fmt.Errorf("offset %d: unexpected byte %#x in PCL", i, data[i])
		}
	}
	if page != nil && len(page.Commands) == 0 && len(page.Segments) == 0 {
		s.Pages = s.Pages[:len(s.Pages)-1]
	}
	return s, nil
}

// parseEscape parses one escape sequence starting at data[i] and returns
// the offset after it.
func parseEscape(data []byte, i int, page *Page) (int, error) {
	start := i
	if i+1 >= len(data) {
		return 0, fmt.Errorf("offset %d: truncated escape", i)
	}
	if c := data[i+1]; c >= 0x30 && c <= 0x7e && (i+2 >= len(data) || !isParam(c)) {
		page.Commands = append(page.Commands, Command{Offset: i, Param: c})
		return i + 2, nil
	}
	if i+2 >= len(data) {
		return 0, fmt.Errorf("offset %d: truncated escape", i)
	}
	param, group := data[i+1], data[i+2]
	i += 3
	raster := param == '*' && group == 'b'
	counted := false
	for {
		j := i
		for j < len(data) && (data[j] == '+' || data[j] == '-' || data[j] == '.' || (data[j] >= '0' && data[j] <= '9')) {
			j++
		}
		if j >= len(data) {
			return 0, fmt.Errorf("offset %d: truncated escape", start)
		}
		value, t := string(data[i:j]), data[j]
		i = j + 1
		upper := t &^ 0x20
		last := t >= 'A' && t <= 'Z'
		cmd := Command{Offset: start, Param: param, Group: group, Value: value, Term: upper}
		if upper == 'W' {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || i+n > len(data) {
				return 0, fmt.Errorf("offset %d: invalid data length %q", start, value)
			}
			block := data[i : i+n]
			if raster {
				if !counted {
					page.RasterGroups++
					counted = true
				}
				if n < 2 {
					return 0, fmt.Errorf("offset %d: raster block shorter than its row count", i)
				}
				page.Segments = append(page.Segments, Segment{Offset: i, Rows: int(block[0])<<8 | int(block[1]), Data: block})
			} else {
				cmd.Data = block
				page.Commands = append(page.Commands, cmd)
			}
			i += n
		} else {
			if raster && upper == 'M' {
				page.Compression = cmd.Int()
			}
			page.Commands = append(page.Commands, cmd)
		}
		if last {
			return i, nil
		}
	}
}

func isParam(c byte) bool { return c >= 0x21 && c <= 0x2f }

func clip(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// RowStats counts row encodings while decoding.
type RowStats struct {
	Rows, BlankRows, RepeatRows, EditRows int
	Edits, RunEdits, LiteralEdits         int
	LiteralBytes, RunBytes                int
	MaxEditsPerRow                        int
	// ExtensionBytes counts offset and count extension bytes;
	// Extension255 those with the value 255 (another byte follows).
	ExtensionBytes, Extension255 int
}

// ErrDecode is returned for data that does not follow the documented row
// encoding.
var ErrDecode = errors.New("decode")

// BandRows is the height of the bands the printer processes: the seed row
// is blank at the start of every band (rows 0, 128, 256, ... of a page).
const BandRows = 128

// DecodeRows decodes all segments of a page with the row encoding of
// compression mode 1030 as documented in docs/brother-raster-format.md,
// including the seed row reset at every band. width is the row length in
// bytes; data beyond it is an error.
func DecodeRows(p *Page, width int) ([][]byte, RowStats, error) {
	return decodeRows(p, width, false)
}

// DecodeRowsResetSeed decodes like DecodeRows but starts every segment with
// a blank seed row. Comparing both shows whether a stream relies on the
// seed row carrying over from one segment to the next.
func DecodeRowsResetSeed(p *Page, width int) ([][]byte, RowStats, error) {
	return decodeRows(p, width, true)
}

func decodeRows(p *Page, width int, reset bool) ([][]byte, RowStats, error) {
	var st RowStats
	var rows [][]byte
	seed := make([]byte, width)
	for _, seg := range p.Segments {
		if reset {
			seed = make([]byte, width)
		}
		buf := seg.Data
		pos := 2
		for r := 0; r < seg.Rows; r++ {
			if len(rows)%BandRows == 0 {
				seed = make([]byte, width)
			}
			row, n, err := decodeRow(buf, pos, seed, &st)
			if err != nil {
				return nil, st, fmt.Errorf("%w: segment at %d, row %d: %v", ErrDecode, seg.Offset, r, err)
			}
			pos = n
			rows = append(rows, row)
			seed = row
		}
		if pos != len(buf) {
			return nil, st, fmt.Errorf("%w: segment at %d: %d of %d bytes used", ErrDecode, seg.Offset, pos, len(buf))
		}
	}
	return rows, st, nil
}

func next(buf []byte, pos *int) (byte, error) {
	if *pos >= len(buf) {
		return 0, errors.New("unexpected end of segment")
	}
	b := buf[*pos]
	*pos++
	return b, nil
}

// extend adds extension bytes while v has its field maximum: each
// following byte is added, and a byte of 255 means another one follows.
func extend(buf []byte, pos *int, v, max int, st *RowStats) (int, error) {
	if v != max {
		return v, nil
	}
	for {
		b, err := next(buf, pos)
		if err != nil {
			return 0, err
		}
		st.ExtensionBytes++
		if b == 255 {
			st.Extension255++
		}
		v += int(b)
		if b != 255 {
			return v, nil
		}
	}
}

func decodeRow(buf []byte, pos int, seed []byte, st *RowStats) ([]byte, int, error) {
	st.Rows++
	n, err := next(buf, &pos)
	if err != nil {
		return nil, 0, err
	}
	if n == 0xff {
		st.BlankRows++
		return make([]byte, len(seed)), pos, nil
	}
	row := append([]byte(nil), seed...)
	if n == 0 {
		st.RepeatRows++
		return row, pos, nil
	}
	st.EditRows++
	st.Edits += int(n)
	st.MaxEditsPerRow = max(st.MaxEditsPerRow, int(n))
	cur := 0
	for e := 0; e < int(n); e++ {
		c, err := next(buf, &pos)
		if err != nil {
			return nil, 0, err
		}
		if c&0x80 != 0 {
			// Repeated byte: offset in bits 6-5, count-2 in bits 4-0.
			off, err := extend(buf, &pos, int(c>>5)&3, 3, st)
			if err != nil {
				return nil, 0, err
			}
			cnt, err := extend(buf, &pos, int(c&31), 31, st)
			if err != nil {
				return nil, 0, err
			}
			cnt += 2
			v, err := next(buf, &pos)
			if err != nil {
				return nil, 0, err
			}
			cur += off
			if cur+cnt > len(row) {
				return nil, 0, fmt.Errorf("run beyond row end (%d > %d)", cur+cnt, len(row))
			}
			for k := 0; k < cnt; k++ {
				row[cur+k] = v
			}
			cur += cnt
			st.RunEdits++
			st.RunBytes += cnt
		} else {
			// Literal bytes: offset in bits 6-3, count-1 in bits 2-0.
			off, err := extend(buf, &pos, int(c>>3)&15, 15, st)
			if err != nil {
				return nil, 0, err
			}
			cnt, err := extend(buf, &pos, int(c&7), 7, st)
			if err != nil {
				return nil, 0, err
			}
			cnt++
			cur += off
			if cur+cnt > len(row) || pos+cnt > len(buf) {
				return nil, 0, fmt.Errorf("literal beyond row or segment end")
			}
			copy(row[cur:], buf[pos:pos+cnt])
			pos += cnt
			cur += cnt
			st.LiteralEdits++
			st.LiteralBytes += cnt
		}
	}
	return row, pos, nil
}
