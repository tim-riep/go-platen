# Brother HL-2135W print data format

This document describes the print data stream that the official Brother
driver sends to the Brother HL-2135W, as far as it can be determined from
the captures in [`testdata/captures`](../testdata/captures). The platen
encoder (`internal/brother`) is written from this document alone.

## Sources and method

The only sources are:

- the print data streams produced by the official Brother driver
  (Windows 11) and provided by the project owner, together with the source
  PDFs; this is observation of the program's behavior under § 69d (3) UrhG;
- the public HP *PJL Technical Reference* and *PCL 5 Technical Reference*,
  for the meaning of standard commands and for the delta row compression
  scheme of PCL compression method 9.

No driver files, driver code or third-party implementations were opened.

The streams were dissected with the analysis tool in
[`internal/brother/inspect`](../internal/brother/inspect):

```bash
go run ./internal/brother/inspect/brinspect -pdf testdata/captures/01-test-document/source.pdf \
    testdata/captures/01-test-document/capture.prn
```

It prints the PJL lines and PCL commands, decodes the raster data with the
encoding described below, and compares the decoded pages with a PDFium
rendering of the source PDF (600 dpi, A4, threshold).

### Captures

| Capture | Source | Pages | Settings | Size |
| --- | --- | --- | --- | --- |
| `01-test-document` | text, frames, logo, drawing, QR code | 1 | 600 dpi, A4 | 161 034 bytes |
| `02-sample-11-pages` | text pages | 11 | 600 dpi, A4 | 2 773 872 bytes |

Every statement below holds for all 12 pages of both captures unless noted.
Evidence is given as capture name and byte offset where useful.

## 1. Job structure

```
UEL  PJL header  @PJL ENTER LANGUAGE=PCL
  page 1: PCL page setup, raster data, form feed (0x0C)
  page 2: PCL page setup, raster data, form feed
  ...
UEL  @PJL EOJ NAME="..."<CR><LF>  UEL
```

UEL is the Universal Exit Language sequence `ESC % - 1 2 3 4 5 X` (HP PJL
Technical Reference). The stream contains exactly three UELs.

### 1.1 PJL header

Byte-exact header of `01-test-document` (offsets 0–401), one line per PJL
command:

| Line | Line end | Notes |
| --- | --- | --- |
| `@PJL ` (with trailing space) | LF | directly after the UEL |
| `@PJL SET REPRINT=OFF` | LF | |
| `@PJL SET HOLD=OFF` | LF | |
| `@PJL SET USERNAME=""` | CR LF | empty user name in both captures |
| `@PJL SET JOBNAME="<x><file name>"` | CR LF | see below |
| `@PJL JOB NAME="<x><file name>"` | CR LF | same value |
| `@PJL PRINTLOG ITEM = 1,PRINTER` | LF | |
| `@PJL PRINTLOG ITEM = 2,<Day>,<DD Mon YYYY HH:MM:SS>` | LF | print time, e.g. `Fri,30 Oct 2026 22:27:07` |
| `@PJL PRINTLOG ITEM = 3,` | LF | |
| `@PJL PRINTLOG ITEM = 4,` | LF | |
| `@PJL SET STRINGCODESET=HPROMAN8` | LF | |
| `@PJL SET ECONOMODE=OFF` | LF | toner save off in both captures |
| `@PJL SET MEDIATYPE=REGULAR` | LF | |
| `@PJL SET RESOLUTION=600` | LF | |
| `@PJL ENTER LANGUAGE=PCL` | LF | PCL follows directly |

`JOB`/`EOJ`, `SET`, `ENTER LANGUAGE`, `STRINGCODESET`, `ECONOMODE` and
`RESOLUTION` are documented in the HP PJL Technical Reference. `REPRINT`,
`HOLD`, `USERNAME`, `JOBNAME`, `MEDIATYPE` and `PRINTLOG` are not part of
the HP reference used here; they appear to be printer-specific variables
(job retention, job log, media type).

The job name is the file name of the printed document, preceded by one byte
whose value differs between the captures: `0xC5` in `01-test-document`
(offset 0x5E), `0x20` in `02-sample-11-pages` (offset 0x5E). Its meaning is
unknown (see open questions).

### 1.2 Page setup (PCL)

Directly after `@PJL ENTER LANGUAGE=PCL` the **first page** begins with
(`01-test-document`, offsets 402–499; same bytes in `02-sample-11-pages`):

```
ESC&u600D        ESC*t600R        ESC&n8WdRegular   ESC&l1h1001H
ESC&l0S          ESC&l1X          ESC&l0O           ESC&l4096A
ESC&l26a6d1E     ESC&l0U          ESC&l0Z           ESC*p0X
ESC*p0Y          ESC*b1030M
```

**Every following page** begins with the second half of this sequence only
(`02-sample-11-pages`, 10 times):

```
ESC&l0O  ESC&l4096A  ESC&l26a6d1E  ESC&l0U  ESC&l0Z  ESC*p0X  ESC*p0Y  ESC*b1030M
```

Lower case termination characters combine parameters of the same group as
documented by HP (`ESC&l26a6d1E` = `ESC&l26A` `ESC&l6D` `ESC&l1E`).

| Command | Meaning (HP PCL 5 reference) |
| --- | --- |
| `ESC&u600D` | unit of measure: 600 units per inch |
| `ESC*t600R` | raster graphics resolution 600 dpi |
| `ESC&n8WdRegular` | alphanumeric ID, 8 data bytes: operation `d` (media type by name), name `Regular` |
| `ESC&l1H` | paper source 1 (main tray) |
| `ESC&l1001H` | paper source value 1001 — **not documented by HP; meaning unknown** |
| `ESC&l0S` | simplex |
| `ESC&l1X` | number of copies 1 |
| `ESC&l0O` | portrait orientation |
| `ESC&l4096A` | page size value 4096 — **not documented by HP; meaning unknown** |
| `ESC&l26A` | page size A4 |
| `ESC&l6D` | 6 lines per inch |
| `ESC&l1E` | top margin 1 line |
| `ESC&l0U`, `ESC&l0Z` | left and top offset registration 0 |
| `ESC*p0X`, `ESC*p0Y` | cursor to position 0, 0 |
| `ESC*b1030M` | raster compression mode **1030** — not an HP method; described in section 2 |

There is no `ESC*r#A` (start raster graphics) and no `ESC*rB`/`ESC*rC`
(end raster graphics) anywhere in the captures.

### 1.3 Page end and job end

Each page ends with a single form feed byte `0x0C` directly after the last
raster data block (12 of 12 pages). After the form feed of the last page the
stream ends with (`01-test-document`, offsets 160 983–161 033):

```
ESC%-12345X@PJL EOJ NAME="<x><file name>"<CR><LF>ESC%-12345X
```

Note that the byte `0x0C` also occurs inside raster data; page ends can only
be found by following the data lengths of the raster transfer commands.

## 2. Raster data (compression mode 1030)

### 2.1 Transfer commands and segments

Raster data is sent with the PCL transfer command `ESC*b#W` followed by
`#` data bytes. Several blocks can be combined into one escape sequence with
the lower case form: `ESC*b130w<130 bytes>130w<130 bytes>44W<44 bytes>`
(`01-test-document`, offset 500).

Each data block is a **segment**:

```
+--------+--------+---------------- ... ----+
| rows (16-bit, big endian)  | row 1 | row 2 | ... | row n |
+--------+--------+---------------- ... ----+
```

The first two bytes give the number of rows encoded in the segment. Evidence:
the row counts of each page add up to exactly 6816 rows on all 12 pages, and
decoding consumes every segment exactly to its last byte (no page of the
captures has a decoding error or unused bytes).

Observed segmentation by the driver (all 12 pages):

- A segment holds 1 to 128 rows.
- **Only segments consisting entirely of blank rows** (`0xFF`, see 2.2) hold
  more than 16 rows, up to 128 (613 of 613 such segments in both captures).
  Consecutive blank segments are combined in one escape sequence with lower
  case `w` (123 of 123 combined sequences contain only blank rows). Example:
  three segments of 128, 128 and 42 blank rows at the top of
  `01-test-document`.
- All other segments, including those that consist only of repeated rows
  (`0x00`), hold at most 16 rows and are sent in their own escape sequence.
  Example: after the segment with the first rule, the unchanged rows of the
  vertical frame lines in `01-test-document` are sent as five repeat-only
  segments of 16, 16, 16, 16 and 6 rows. Most segments have exactly 16 rows; shorter segments occur at
  transitions between blank and printed areas.
- The largest segment is 3911 bytes (`02-sample-11-pages`).

Whether the printer requires this limit is not known. platen follows it
because the driver does.

### 2.2 Row encoding

Rows are delta encoded against a **seed row**, the previous row. The seed row
carries over from one segment to the next and from one escape sequence to the
next: decoding with a blank seed at every segment start gives a different
image than the source (checked with `inspect.DecodeRowsResetSeed`).

Each row starts with one byte `n`:

| `n` | Meaning |
| --- | --- |
| `0xFF` | blank row: all pixels white; the seed row becomes blank |
| `0x00` | repeat row: identical to the seed row |
| `1`–`254` | the row is the seed row with `n` edits applied; `n` edit commands follow |

The largest observed `n` is 215. All 12 pages begin with a blank row.

Each edit command replaces bytes of the row. Its layout matches the command
byte of HP PCL compression method 9 ("compressed replacement delta row
encoding", HP PCL 5 Technical Reference):

**Literal replacement** — command byte with bit 7 = 0:

```
bit  7    6 5 4 3       2 1 0
     0    offset (0-15) count-1 (0-7)
```

followed by offset extension bytes (if offset = 15), count extension bytes
(if count-1 = 7), then `count` literal bytes that replace the row bytes.

**Repeated byte** — command byte with bit 7 = 1:

```
bit  7    6 5           4 3 2 1 0
     1    offset (0-3)  count-2 (0-31)
```

followed by offset extension bytes (if offset = 3), count extension bytes
(if count-2 = 31), then one byte that is written `count` times.

**Extension bytes**: if a field has its maximum value, the next byte is
added to it; if that byte is 255, another byte follows and is added as well,
until a byte below 255. Offset extensions come before count extensions.
Extension bytes of 255 occur 4296 times in the captures (440 and 3856).

**Offsets are relative**: the offset counts bytes from the end of the
previous edit in the same row (from the start of the row for the first
edit). Bytes not covered by an edit keep the seed row's value.

Example (`01-test-document`, first row with ink, segment data at offset 824, `ESC*b44W`):

```
00 10                    16 rows
02                       row 1: 2 edits
   ff 3d ff c8 ff        repeated byte: offset 3+61 = 64, count 31+255+200+2 = 488, byte 0xFF
   00 c0                 literal: offset 0, count 1, byte 0xC0
00 00 00                 rows 2-4: repeat (a horizontal rule 4 rows thick)
07 78 31 f0 9f 7b 00 ... row 5: 7 edits, turning the rule into vertical frame lines
```

### 2.3 Pixels and geometry

- A set bit is a black pixel; the most significant bit is the leftmost pixel.
- Each page has 6816 rows. A4 at 600 dpi is 7016 rows high; 6816 = 7016 − 200.
- Rows use at most 567 bytes: no capture has ink beyond byte 566
  (0-based) of a row (`02-sample-11-pages`).
  The row length itself is not transmitted: rows only contain edits.
- Comparing the decoded pages with a PDFium rendering of the source PDF
  (A4, 600 dpi) gives the best match at a horizontal offset of **76 pixels**
  on all 12 pages and a vertical offset of **91 to 95 rows** (93 on 10 of 12
  pages). Row 0, pixel 0 of the raster data is therefore about 76 pixels
  (3.2 mm) from the left paper edge and about 93 rows (3.9 mm) from the top
  edge, as printed through the Windows PDF viewer used for the captures.

The decoded images reproduce the source pages: text, lines, frames and the QR
code match the reference rendering. Where the source has gray or colored
areas (the logo in `01-test-document`, gray text), the driver produces a
halftone pattern while the reference rendering uses a plain threshold, which
explains why only 55 % to 75 % of black pixels coincide (intersection over
union per page as reported by `brinspect`).

## 3. Encoder decisions in platen

The platen encoder follows the observed structure. Where the captures leave
a choice, it decides as follows:

| Topic | Decision |
| --- | --- |
| PJL header | same commands and line endings as observed, without the `PRINTLOG` lines (job log entries); the job name is the job title, ASCII only, without the unexplained prefix byte |
| Page setup | the observed byte sequences for the first and the following pages |
| Page geometry | A4 at 600 dpi only; 6816 rows; source bitmap origin at (76, 100) pixels; row length 602 bytes (4816 pixels, the paper width minus 76 pixels on both sides, rounded up to whole bytes) |
| Segments | runs of at least 16 blank rows in segments of up to 128 rows, combined in one escape sequence; all other rows in segments of at most 16 rows, each in its own escape sequence, ending early before a segment would exceed 3900 bytes |
| Edits | longest runs of 3 or more equal bytes become repeated-byte edits, other changed bytes literal edits; edits are merged if a row would otherwise need more than 254 |
| First row of a page | encoded against a blank seed row |
| Copies | pages are repeated by the raster backend; `ESC&l1X` is always sent |
| Toner save, other resolutions, other paper sizes | not supported (no captures) |

## 4. Open questions

1. **Row length and right edge.** The row length is not part of the stream,
   and no capture has ink beyond byte 566. It is unknown how the printer
   treats data beyond its printable width. platen sends at most 602 bytes.
2. **Vertical origin.** The measured offset is 91–95 rows, while the page
   height suggests 100 rows (7016 − 6816 = 200, split evenly). The
   difference may come from the PDF viewer or the driver's own positioning.
   A capture of a page with marks at known positions (e.g. a 1 mm frame at
   10 mm from every edge) would settle it. platen uses 100 rows.
3. **Seed row at the start of a page.** Every captured page starts with a
   blank row, so it is not visible whether the seed row is reset at a new
   page or at `ESC*b1030M`. platen assumes a blank seed at each page start.
4. **More than 254 edits in a row**: not observed (maximum 215).
5. **Segment size limit**: the largest observed segment has 3911 bytes; a
   printer limit is unknown.
6. **`ESC&l1001H` and `ESC&l4096A`**: values not documented by HP; sent
   unchanged.
7. **Job name prefix byte** (`0xC5`, `0x20`): meaning unknown; not sent.
8. **Not covered by the captures**: other resolutions (e.g. 300 dpi, HQ 1200),
   other paper sizes (Letter, A5, envelopes), toner save
   (`ECONOMODE=ON`?), media types, copies > 1, multiple copies collated by
   the printer, landscape orientation. Each needs a capture before it is
   implemented.
