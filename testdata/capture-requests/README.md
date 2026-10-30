# Capture requests for the Brother HL-2135W

These PDFs answer the open questions in
[docs/brother-raster-format.md](../../docs/brother-raster-format.md). They are
generated with exact geometry by
[`internal/brother/inspect/testpages`](../../internal/brother/inspect/testpages):

```bash
go run ./internal/brother/inspect/testpages testdata/capture-requests
```

## How to capture

### With the script (recommended)

[`capture.ps1`](capture.ps1) prints all cases and files the results. It needs
[SumatraPDF](https://www.sumatrapdfreader.org) (prints PDFs at actual size
from the command line) and an elevated PowerShell (Run as administrator), in
this folder:

```powershell
powershell -ExecutionPolicy Bypass -File .\capture.ps1
```

The script asks which installed printer's Brother driver to use and creates
a printer "platen capture" with that driver on a port that writes to a file,
so nothing goes to a real printer. For each case it resets the printer to
the driver defaults, prints the PDF and stores `capture.prn`, `source.pdf`
and `settings.txt` in `testdata/captures/<folder>/`. For resolution, toner
save, paper source and media type it opens the driver's printing preferences
and asks you to change that one setting, and afterwards to set it back.
Existing captures are skipped; `-Only 03-calibration-600,08-stripes` captures
selected cases and `-Force` captures again. At the end it offers to remove the
capture printer.

### By hand

Use the Windows "Brother capture" printer on the `FILE:` port, as for the
first two captures.

- Print from the same PDF viewer as before and set its scaling to **Actual
  size** (not "Fit"). The calibration pages only work unscaled.
- Change only the driver setting named in the table; leave everything else at
  the defaults (600 dpi, A4, toner save off, 1 copy, plain paper, tray).
- Save each capture as `testdata/captures/<folder>/capture.prn`, copy the PDF
  into the same folder as `source.pdf`, and write the driver version and the
  settings you used into `settings.txt` (see
  [testdata/captures/README.md](../captures/README.md)).

## Captures

| Folder | PDF | Driver settings (everything else default) | Answers |
| --- | --- | --- | --- |
| `03-calibration-600` | `01-calibration-a4.pdf` | defaults | exact image origin (vertical offset 91–95 vs. 100 rows), printable area, row width and right edge |
| `04-calibration-300` | `01-calibration-a4.pdf` | Resolution **300 dpi** | encoding at 300 dpi |
| `05-calibration-hq1200` | `01-calibration-a4.pdf` | Resolution **HQ 1200** | encoding at HQ 1200 |
| `06-full-black` | `02-full-black-a4.pdf` | defaults | largest data area, longest runs |
| `07-blank` | `03-blank-a4.pdf` | defaults | the stream of an empty page |
| `08-stripes` | `04-stripes-a4.pdf` | defaults | rows with more than 254 changes, very large segments |
| `09-grays` | `05-grays-a4.pdf` | defaults | the driver's halftone for every gray level, gray text and lines |
| `10-grays-toner-save` | `05-grays-a4.pdf` | **Toner Save Mode on** | how toner save is sent (PJL, PCL or thinner raster) |
| `11-landscape` | `06-landscape-a4.pdf` | Orientation **Landscape** | landscape pages (rotation in the raster or a PCL command) |
| `12-letter` | `07-calibration-letter.pdf` | Paper size **Letter** | Letter page size command, rows and margins |
| `13-a5` | `08-calibration-a5.pdf` | Paper size **A5** | A5 page size command, rows and margins |
| `14-copies-collated` | `09-three-pages-a4.pdf` | Copies **2**, Collate **on** | how copies are sent |
| `15-copies-uncollated` | `09-three-pages-a4.pdf` | Copies **2**, Collate **off** | uncollated copies |
| `16-manual-feed` | `01-calibration-a4.pdf` | Paper source **Manual** | meaning of `ESC&l1H` / `ESC&l1001H` |
| `17-thick-paper` | `01-calibration-a4.pdf` | Media type **Thick Paper** | media type in PJL and in `ESC&n#Wd` |

The most valuable captures are `03`, `08`, `09` and `12`: they settle the
geometry, the limits of the row encoding, gray levels and a second paper
size. The others can follow later.

Optional: print `03-blank-a4.pdf` once more after renaming the file to
`Ä test.pdf`, as `18-job-name`. It shows whether the unexplained byte before
the job name depends on the file name.

## What the pages contain

- **Calibration** (`01`, `07`, `08`): frames 0.5 mm wide whose outer edge is 5,
  10 and 20 mm from the paper edge; rulers with 1 mm ticks running inwards
  from the middle of each edge (and 40 mm further along); 5 × 5 mm squares in
  the paper corners; a center cross; a 1 × 1 mm square at 30/30 mm. What the
  driver clips shows the printable area, and the positions of the marks give
  the image origin to the pixel.
- **Full black** (`02`): the whole sheet black.
- **Blank** (`03`): an empty page.
- **Stripes** (`04`): 0.1 mm vertical lines every 0.7 mm across the full
  width (about 300 changes per row), a band of random 0.17 mm dots (a 1-bit
  image, incompressible rows), and horizontal hairlines every 0.5 mm.
- **Grays** (`05`): squares from 0 % to 100 % in 5 % steps, a gradient, and
  gray text and lines.
- **Landscape** (`06`): a calibration page in landscape format with an arrow
  pointing to its top edge.
- **Three pages** (`09`): pages with one, two and three bars, to tell pages
  and copies apart.
