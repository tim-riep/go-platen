# Brother HL-2135W print data format

This document describes the print data stream that the Brother driver sends
to the Brother HL-2135W, as far as it can be determined from the captures in
[`testdata/captures`](../testdata/captures). The platen encoder
(`internal/brother`) is written from this document alone.

## Sources and method

The only sources are:

- the print data streams produced by the Brother driver for this printer on
  Windows 11 and provided by the project owner, together with the source
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

The captures are kept locally (they contain third-party documents) and are
not part of the repository. Captures `03`–`18` were made from the test pages
in [`testdata/capture-requests`](../testdata/capture-requests), which have
exact geometry, with [`capture.ps1`](../testdata/capture-requests/capture.ps1)
and SumatraPDF at actual size.

| Capture | Driver | Source | Pages | Settings |
| --- | --- | --- | --- | --- |
| `01-test-document` | Brother driver as installed for the printer | text, frames, logo, drawing, QR code | 1 | 600 dpi, A4 |
| `02-sample-11-pages` | same | text pages | 11 | 600 dpi, A4 |
| `03-calibration-600` | Brother Laser Type1 Class Driver 10.0.17119.1 | calibration page | 1 | 600 dpi, A4, Auto Select |
| `04-calibration-300` | same | calibration page | 1 | 300 dpi |
| `05-calibration-hq1200` | same | calibration page | 1 | HQ 1200 |
| `06-full-black` | same | black sheet | 1 | 600 dpi |
| `07-blank` | same | empty page | 1 | 600 dpi |
| `08-stripes` | same | hairlines, random dots | 1 | 600 dpi |
| `09-grays` | same | gray wedge, gradient, gray text | 1 | 600 dpi |
| `10-grays-toner-save` | same | as `09` | 1 | toner save on |
| `11-landscape` | same | landscape calibration page | 1 | landscape |
| `12-letter` | same | calibration page | 1 | Letter |
| `13-a5` | same | calibration page | 1 | A5 |
| `14-copies-collated` | same | three pages | 3 | 2 copies, collate on |
| `15-copies-uncollated` | same | three pages | 3 | 2 copies, collate off |
| `16-manual-feed` | same | calibration page | 1 | paper source manual |
| `17-thick-paper` | same | calibration page | 1 | media type thick |
| `18-job-name` | same | empty page named `Ä test.pdf` | 1 | 600 dpi |

The Brother Laser Type1 Class Driver is the driver for this printer. Its
streams have the same structure as those of `01`/`02`; the only difference in
the setup is the paper source command (section 1.2). Every statement below
holds for all 32 pages of the 18 captures unless noted. Evidence is given as
capture name and byte offset where useful.

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
| `@PJL SET USERNAME=""` | CR LF | empty user name in all captures |
| `@PJL SET JOBNAME="<x><document name>"` | CR LF | see below |
| `@PJL JOB NAME="<x><document name>"` | CR LF | same value |
| `@PJL PRINTLOG ITEM = 1,PRINTER` | LF | |
| `@PJL PRINTLOG ITEM = 2,<Day>,<DD Mon YYYY HH:MM:SS>` | LF | print time, e.g. `Fri,30 Oct 2026 22:27:07` |
| `@PJL PRINTLOG ITEM = 3,` | LF | |
| `@PJL PRINTLOG ITEM = 4,` | LF | |
| `@PJL SET STRINGCODESET=HPROMAN8` | LF | |
| `@PJL SET ECONOMODE=OFF` | LF | `ON` with toner save (`10`) |
| `@PJL SET MEDIATYPE=REGULAR` | LF | `THICK` with thick paper (`17`) |
| `@PJL SET RESOLUTION=600` | LF | `300` (`04`), `1200` for HQ 1200 (`05`) |
| `@PJL ENTER LANGUAGE=PCL` | LF | PCL follows directly |

`JOB`/`EOJ`, `SET`, `ENTER LANGUAGE`, `STRINGCODESET`, `ECONOMODE` and
`RESOLUTION` are documented in the HP PJL Technical Reference. `REPRINT`,
`HOLD`, `USERNAME`, `JOBNAME`, `MEDIATYPE` and `PRINTLOG` are not part of
the HP reference used here; they appear to be printer-specific variables
(job retention, job log, media type).

**Toner save** changes nothing but the `ECONOMODE` line: apart from that line
and the print time, `09-grays` and `10-grays-toner-save` are identical, raster
data included.

**Job name.** The job name is the document name the application passes to
the driver. With SumatraPDF this is the full path; the driver replaces the
path separators with spaces (`C: Users tri05 Downloads ... 01-calibration-a4.pdf`)
and ends the name before the first non-ASCII character: in `18-job-name` the
file name `Ä test.pdf` and the separator before it are missing. The name is
preceded by one byte whose meaning is unknown: `0xC5` in `01`, `0x20` in `02`,
`T` in `03`–`18`.

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
(`02-sample-11-pages`, 10 times; `14`/`15`, 2 times each):

```
ESC&l0O  ESC&l4096A  ESC&l26a6d1E  ESC&l0U  ESC&l0Z  ESC*p0X  ESC*p0Y  ESC*b1030M
```

Lower case termination characters combine parameters of the same group as
documented by HP (`ESC&l26a6d1E` = `ESC&l26A` `ESC&l6D` `ESC&l1E`).

| Command | Meaning (HP PCL 5 reference) | Observed values |
| --- | --- | --- |
| `ESC&u#D` | unit of measure | 600, 300 (`04`), 1200 (`05`) |
| `ESC*t#R` | raster graphics resolution | as `ESC&u#D` |
| `ESC&n#Wd<name>` | alphanumeric ID: operation `d` (media type by name), `#` = 1 + length of the name | `8WdRegular`; `6WdThick` (`17`) |
| `ESC&l#H` | paper source | `1` followed by `1001` (`01`/`02`); `7` = auto select (`03`–`15`, `17`, `18`); `2` = manual feed (`16`) |
| `ESC&l0S` | simplex | always 0 |
| `ESC&l#X` | number of copies | 1; 2 in `14` and `15` |
| `ESC&l0O` | portrait orientation | always 0, also for landscape pages (`11`) |
| `ESC&l4096A` | page size value 4096 — **not documented by HP; meaning unknown** | always sent before the page size |
| `ESC&l#A` | page size | 26 = A4, 2 = Letter (`12`), 25 = A5 (`13`); HP's codes |
| `ESC&l6D` | 6 lines per inch | |
| `ESC&l1E` | top margin 1 line | |
| `ESC&l0U`, `ESC&l0Z` | left and top offset registration 0 | |
| `ESC*p0X`, `ESC*p0Y` | cursor to position 0, 0 | |
| `ESC*b1030M` | raster compression mode **1030** — not an HP method; described in section 2 | |

The values of `ESC&l#H` are HP's documented paper source codes (1 main tray,
2 manual feed, 7 auto select); `1001` is not documented.

**Copies**: with 2 copies the driver sends each page once and `ESC&l2X` in the
setup of the first page. Collate on and off produce the same stream apart
from the print time (`14`, `15`), so collation is not expressed in the
stream. HP defines `ESC&l#X` as the number of copies of each page.

**Landscape**: the driver rotates the page itself and sends a portrait
raster with `ESC&l0O` (`11`).

There is no `ESC*r#A` (start raster graphics) and no `ESC*rB`/`ESC*rC`
(end raster graphics) anywhere in the captures.

### 1.3 Page end and job end

Each page ends with a single form feed byte `0x0C` directly after the last
raster data block (32 of 32 pages). After the form feed of the last page the
stream ends with (`01-test-document`, offsets 160 983–161 033):

```
ESC%-12345X@PJL EOJ NAME="<x><document name>"<CR><LF>ESC%-12345X
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
the row counts of each page add up to the page's row count (section 2.3) on
all 32 pages, and decoding consumes every segment exactly to its last byte.

Observed segmentation by the driver (9296 segments):

- A segment holds 1 to 128 rows.
- **Only segments consisting entirely of blank rows** (`0xFF`, see 2.2) hold
  more than 16 rows, up to 128 (1016 such segments). Consecutive blank
  segments are combined in one escape sequence with lower case `w`
  (176 combined sequences, all of blank rows only). Example: three segments
  of 128, 128 and 42 blank rows at the top of `01-test-document`.
- All other segments, including those that consist only of repeated rows
  (`0x00`), hold at most 16 rows and are sent in their own escape sequence.
  Example: after the segment with the first rule, the unchanged rows of the
  vertical frame lines in `01-test-document` are sent as five repeat-only
  segments of 16, 16, 16, 16 and 6 rows.
- The largest segment has 5239 bytes (`05-calibration-hq1200`); at 600 dpi
  3911 bytes (`02-sample-11-pages`).
- **No segment crosses a 128-row band boundary**, counting rows from the
  first row of the page (rows 0, 128, 256, ...), at every resolution. When a
  run of rows would cross a boundary, the driver ends the segment there.
  Example: `01-test-document` sends a segment of only 6 repeated rows
  (rows 378–383) so that the next segment starts at row 384 = 3 × 128.

### 2.2 Row encoding

Rows are delta encoded against a **seed row**, the previous row. Within a
128-row band the seed row carries over from one segment to the next and from
one escape sequence to the next: decoding with a blank seed at every segment
start gives a different image than the source (checked with
`inspect.DecodeRowsResetSeed`).

**The first row of every band does not depend on any previous row.** In all
captures, the first row of every band is either a blank row (`0xFF`) or a
row whose edits write **exactly the whole data area** (section 2.3), so that
the result is the same whatever the seed row holds (991 of 991 such rows).
The driver even re-describes a row that is identical to the previous one
when it starts a band: row 384 of `01-test-document` is encoded with 9 edits,
including runs of zero bytes, although rows 302–383 are unchanged repeats of
it. No edit ever writes outside the data area.

The printer depends on this. Two platen test pages whose segments crossed
band boundaries and whose row 384 was a repeat row printed only the rows up
to row 383 (the top rule and the start of the frame lines); the driver's own
stream of the same page printed completely on the same printer (test prints
on 2026-10-30).

Each row starts with one byte `n`:

| `n` | Meaning |
| --- | --- |
| `0xFF` | blank row: all pixels white; the seed row becomes blank |
| `0x00` | repeat row: identical to the seed row |
| `1`–`254` | the row is the seed row with `n` edits applied; `n` edit commands follow |

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
Extension bytes of 255 occur 4296 times in `01` and `02` alone.

**Offsets are relative**: the offset counts bytes from the end of the
previous edit in the same row (from the start of the row for the first
edit). Bytes not covered by an edit keep the seed row's value.

**At most 254 edits.** Rows that would need more changes end with a single
literal edit that covers everything from its start to the end of the last
change. In `08-stripes` (vertical hairlines every 0.7 mm, about 290 changes
per row) the 8 affected rows have exactly 254 edits, the last a literal of
90–91 bytes; in `05-calibration-hq1200` 12 rows end with a literal of up to
684 bytes.

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

A set bit is a black pixel; the most significant bit is the leftmost pixel.

The calibration pages have frames 0.5 mm wide whose outer edge lies exactly
5, 10 and 20 mm from each paper edge. Their positions in the decoded raster
give the **origin** of the raster on the paper: at 600 dpi, row 0 / pixel 0
is **80 pixels from the left and 100 pixels from the top edge** (3.39 mm and
4.23 mm). Evidence (`03-calibration-600`, row 1704 and column 1240): the left
frames start at raster x 38, 156 and 393 (paper 118.1, 236.2 and 472.4 px),
the top frames at raster y 19, 137 and 372; the right and bottom frames give
the same origin within one pixel. The origin is the same for Letter (`12`)
and A5 (`13`) and scales with the resolution (40/50 at 300 dpi, 160/200 at
1200 dpi).

The first captures (`01`, `02`) had suggested 76 and 91–95 pixels; those
values came from comparing the driver's rendering of documents printed from
a PDF viewer with platen's own rendering, not from exact geometry, and are
superseded.

| Format | Page size code | Rows | Row length (bytes) | Data area (bytes) | Origin (px) | Capture |
| --- | --- | --- | --- | --- | --- | --- |
| A4, 300 dpi | 26 | 3408 | 300 | 1–298 | 40, 50 | `04` |
| A4, 600 dpi | 26 | 6816 | 600 | 2–597 | 80, 100 | `01`–`03`, `06`–`11`, `14`–`18` |
| A4, 1200 dpi (HQ 1200) | 26 | 13631 | 1200 | 5–1195 | 160, 200 | `05` |
| Letter, 600 dpi | 2 | 6400 | 617 | 2–614 | 80, 100 | `12` |
| A5, 600 dpi | 25 | 4761 | 417 | 2–414 | 80, 100 | `13` |

- **Rows**: the paper height minus twice the top origin (A4: 7016 − 200 =
  6816; Letter: 6600 − 200; A5: 4961 − 200). At 1200 dpi the page has 13631
  rows, one less than twice the 600 dpi value.
- **Data area**: band start rows write exactly this range, no edit writes
  outside it, and the full black page (`06`) is black exactly there. The row
  length is the paper width minus twice the left origin, in whole bytes; the
  data area leaves 2 bytes on each side at 600 dpi.
- The row length itself is not transmitted: rows only contain edits.

## 3. Encoder decisions in platen

The platen encoder follows the observed structure. Where the captures leave
a choice, it decides as follows:

| Topic | Decision |
| --- | --- |
| Formats | the five formats of the table in 2.3; others are rejected |
| PJL header | same commands and line endings as observed, without the `PRINTLOG` lines (job log entries); the job name is the job title, ASCII only, without the unexplained prefix byte |
| Page setup | the observed byte sequences for the first and the following pages, with resolution, media type, paper source and page size of the job |
| Paper source | printer option `source`: `tray` (default, `ESC&l1h1001H` as in `01`/`02` and the test prints), `auto` (`ESC&l7H`), `manual` (`ESC&l2H`) |
| Media type | printer option `media`: `plain` (default, `REGULAR` / `Regular`) or `thick` (`THICK` / `Thick`) |
| Toner save | printer option `toner_save`: `off` (default) or `on` (`ECONOMODE=ON`) |
| Page geometry | origin, rows, row length and data area from the table in 2.3; ink outside the data area is dropped |
| Bands | segments end at every 128-row band boundary; the first row of every band is a blank row or rewrites the whole data area |
| Segments | runs of at least 16 blank rows in segments of up to 128 rows, combined in one escape sequence; all other rows in segments of at most 16 rows, each in its own escape sequence, ending early before a segment would exceed 3900 bytes |
| Edits | runs of 3 or more equal bytes become repeated-byte edits, other changed bytes literal edits; a row with more than 254 edits ends with one literal up to its last change, as the driver does |
| Copies | pages are repeated by the raster backend, so copies come out collated; `ESC&l1X` is always sent |
| Landscape | the renderer rotates landscape pages; the stream is always portrait |
| Gray | rendered with a clustered-dot halftone screen by default (see 3.1) |

The encoder tests encode the decoded raster of every capture and compare
the result with the capture: same PJL header (apart from job log and job
name), same page commands (apart from the number of copies), identical
decoded rows, and the band and segment rules above.

### 3.1 Gray levels

The driver prints gray and colored areas as **clustered dots**: in
`01-test-document`, the light gray shading of the drawing consists of dots of
about 2×2 to 3×4 pixels on a regular lattice spanned by (8, 0) and (4, 4)
pixels, a 45° screen of about 106 lines per inch at 600 dpi. This is image
content, not part of the data format; the printer prints whatever bitmap it
receives.

Test prints on 2026-10-30 compared three renderings of the same page on the
HL-2135W: with a threshold, light gray areas are missing; with
Floyd–Steinberg error diffusion, the stream contained the light gray areas as
isolated single pixels, but they did not appear on paper; with a clustered-dot
screen of the same geometry as the driver's, the print showed no difference
to the driver's print. platen's `brother-raster` backend therefore uses the
clustered-dot screen (`dither=halftone`) by default.

## 4. Open questions

1. **`ESC&l1001H` and `ESC&l4096A`**: values not documented by HP; sent
   unchanged.
2. **Job name prefix byte** (`0xC5`, `0x20`, `T`): meaning unknown; not sent.
3. **Segment size limit**: the largest observed segment has 5239 bytes; a
   printer limit is unknown. platen stays below 3900 bytes.
4. **Collated copies**: the stream has no collation; whether the printer can
   collate copies it receives as `ESC&l#X` is unknown, so platen repeats
   the pages.
5. **Not covered by captures**: Letter and A5 at 300 and 1200 dpi, other
   paper sizes (Legal, envelopes, custom sizes), other media types, duplex
   (the HL-2135W has no duplex unit).
