# Brother HL-2135W print data captures

The Brother raster encoder (milestone 7) is based solely on print data
streams produced with the official Brother driver and on the public HP
PJL/PCL specifications. Captures are provided by the project owner; the
Brother driver is not installed or run by contributors.

For each capture, add a directory `NN-short-name/` containing:

- `source.pdf`: the document that was printed
- `capture.prn`: the raw data stream the driver sent to the printer
  (e.g. printed to a file or captured from port 9100)
- `settings.txt`: the driver settings used (resolution, paper size, toner
  save, copies, duplex, media type, other options), the driver name and
  version, and the operating system

Useful set of captures:

1. A single page with a few black rectangles at known positions, 600 dpi, A4
2. The same page at the other supported resolutions
3. A blank page, and a completely black page
4. Two and three pages
5. Copies = 2
6. Letter paper
7. Toner save on and off
8. A page with fine detail or a halftone/photo area (to see compression)
