// Package ipp implements the ipp backend for driverless printers (IPP
// Everywhere, AirPrint). The backend asks the printer for its
// capabilities, sends PDF documents unchanged when the printer accepts
// PDF and renders them to PWG Raster otherwise. The printer is reached
// over IPP (HTTP, port 631) instead of raw TCP; the worker's journal and
// retry rules stay the same.
package ipp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tim-riep/go-platen/internal/backend"
	"github.com/tim-riep/go-platen/internal/backend/raster"
	proto "github.com/tim-riep/go-platen/internal/ipp"
	"github.com/tim-riep/go-platen/internal/printer"
	"github.com/tim-riep/go-platen/internal/pwg"
	"github.com/tim-riep/go-platen/internal/render"
)

// Name is the registry name of the backend.
const Name = "ipp"

// DefaultPort is the IPP port used when the address has none.
const DefaultPort = 631

// DefaultPath is the printer's resource path when the address is not a
// URI; IPP Everywhere printers use /ipp/print.
const DefaultPath = "/ipp/print"

const (
	formatPDF = "application/pdf"
	formatPWG = "image/pwg-raster"

	// capsTTL is how long the printer's capabilities are cached.
	capsTTL = 5 * time.Minute
	// defaultWait bounds the wait for a job to complete.
	defaultWait = 10 * time.Minute
	// maxDocument bounds the PDF size held in memory while rendering.
	maxDocument = 512 << 20
)

// Factory registers the ipp backend.
var Factory = backend.Factory{
	Name:         Name,
	ContentTypes: []string{formatPDF},
	New:          func() (backend.Backend, error) { return New(), nil },
}

// Backend prints on IPP printers.
type Backend struct {
	mu   sync.Mutex
	caps map[string]*capabilities
	// PollInterval is the interval of job state queries. Default 2s.
	PollInterval time.Duration
	requestID    atomic.Uint32
}

// New returns the backend.
func New() *Backend { return &Backend{caps: map[string]*capabilities{}, PollInterval: 2 * time.Second} }

// Accepts accepts PDF documents.
func (b *Backend) Accepts(contentType string) bool { return contentType == formatPDF }

// PrinterURI returns the IPP URI of the printer: the address itself if it
// is an ipp:// or ipps:// URI, otherwise ipp://<address><path>.
func PrinterURI(address string, opts map[string]string) (string, error) {
	if strings.Contains(address, "://") {
		if _, err := proto.HTTPURL(address); err != nil {
			return "", err
		}
		return address, nil
	}
	if address == "" {
		return "", errors.New("ipp: printer address is empty")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, strconv.Itoa(DefaultPort))
	}
	path := opts["path"]
	if path == "" {
		path = DefaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "ipp://" + address + path, nil
}

// hostPort returns the TCP address of the printer URI.
func hostPort(uri string) (string, error) {
	target, err := proto.HTTPURL(uri)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	return u.Host, nil
}

// Probe checks that the printer's IPP port accepts connections.
func (b *Backend) Probe(ctx context.Context, d backend.Dialer, address string, timeout time.Duration) error {
	uri, err := PrinterURI(address, nil)
	if err != nil {
		return err
	}
	addr, err := hostPort(uri)
	if err != nil {
		return err
	}
	return printer.Probe(ctx, d, addr, timeout)
}

// ---- options ----

type jobSettings struct {
	uri      string
	format   string // "auto", "pdf", "pwg-raster"
	color    string // "auto", "color", "monochrome"
	sides    string // "" (printer default) or an IPP sides keyword
	quality  int    // print-quality enum, 0 for the printer default
	rotate   bool
	wait     time.Duration
	insecure bool
}

func parseSettings(opts backend.JobOptions) (*jobSettings, error) {
	o := opts.Options
	uri, err := PrinterURI(opts.Address, o)
	if err != nil {
		return nil, err
	}
	s := &jobSettings{uri: uri, format: "auto", color: "auto", rotate: true, wait: defaultWait}
	switch v := o["format"]; v {
	case "", "auto":
	case "pdf", "pwg-raster":
		s.format = v
	default:
		return nil, fmt.Errorf("ipp: format must be auto, pdf or pwg-raster")
	}
	switch v := o["color"]; v {
	case "", "auto":
	case "color", "monochrome":
		s.color = v
	default:
		return nil, fmt.Errorf("ipp: color must be auto, color or monochrome")
	}
	switch v := o["sides"]; v {
	case "":
	case "one-sided", "two-sided-long-edge", "two-sided-short-edge":
		s.sides = v
	default:
		return nil, fmt.Errorf("ipp: sides must be one-sided, two-sided-long-edge or two-sided-short-edge")
	}
	switch v := o["quality"]; v {
	case "":
	case "draft":
		s.quality = 3
	case "normal":
		s.quality = 4
	case "high":
		s.quality = 5
	default:
		return nil, fmt.Errorf("ipp: quality must be draft, normal or high")
	}
	switch o["rotate"] {
	case "", "auto":
	case "none":
		s.rotate = false
	default:
		return nil, fmt.Errorf("ipp: rotate must be auto or none")
	}
	if v := o["wait"]; v != "" {
		if s.wait, err = time.ParseDuration(v); err != nil || s.wait < 0 {
			return nil, fmt.Errorf("ipp: wait must be a duration such as 10m, or 0")
		}
	}
	switch o["insecure-tls"] {
	case "", "false":
	case "true":
		s.insecure = true
	default:
		return nil, fmt.Errorf("ipp: insecure-tls must be true or false")
	}
	return s, nil
}

// ---- capabilities ----

type capabilities struct {
	fetched    time.Time
	version    [2]byte
	formats    *proto.Attribute
	resolution []proto.Resolution
	types      *proto.Attribute
	sheetBack  string
	media      *proto.Attribute
	copies     []proto.Range
	sides      *proto.Attribute
	colorModes *proto.Attribute
	qualities  []int
}

func (c *capabilities) maxCopies() int {
	m := 1
	for _, r := range c.copies {
		m = max(m, r.Upper)
	}
	return m
}

var capsAttributes = []string{
	"document-format-supported", "pwg-raster-document-resolution-supported",
	"pwg-raster-document-type-supported", "pwg-raster-document-sheet-back",
	"media-supported", "copies-supported", "sides-supported",
	"print-color-mode-supported", "print-quality-supported", "ipp-versions-supported",
}

func (b *Backend) capabilities(ctx context.Context, client *http.Client, uri string) (*capabilities, error) {
	b.mu.Lock()
	c, ok := b.caps[uri]
	b.mu.Unlock()
	if ok && time.Since(c.fetched) < capsTTL {
		return c, nil
	}
	req := proto.NewRequest(proto.OpGetPrinterAttributes, b.requestID.Add(1))
	req.Add(proto.TagOperation,
		proto.String(proto.TagURI, "printer-uri", uri),
		proto.Keyword("requested-attributes", capsAttributes...))
	res, err := proto.Do(ctx, client, uri, req)
	if err == nil && res.Code == proto.StatusVersionNotSupported {
		req.Major, req.Minor = 1, 1
		res, err = proto.Do(ctx, client, uri, req)
	}
	if err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	attr := func(name string) *proto.Attribute { return res.Attr(proto.TagPrinter, name) }
	c = &capabilities{
		fetched:    time.Now(),
		version:    [2]byte{1, 1},
		formats:    attr("document-format-supported"),
		resolution: attr("pwg-raster-document-resolution-supported").Resolutions(),
		types:      attr("pwg-raster-document-type-supported"),
		sheetBack:  attr("pwg-raster-document-sheet-back").String(),
		media:      attr("media-supported"),
		copies:     attr("copies-supported").Ranges(),
		sides:      attr("sides-supported"),
		colorModes: attr("print-color-mode-supported"),
		qualities:  attr("print-quality-supported").Ints(),
	}
	if attr("ipp-versions-supported").Has("2.0") {
		c.version = [2]byte{2, 0}
	}
	b.mu.Lock()
	b.caps[uri] = c
	b.mu.Unlock()
	return c, nil
}

func (b *Backend) forget(uri string) {
	b.mu.Lock()
	delete(b.caps, uri)
	b.mu.Unlock()
}

// ---- HTTP ----

// idleTimeout bounds a single read or write on the connection: printers
// stop reading while busy, so there is no total timeout.
const idleTimeout = 2 * time.Minute

func httpClient(d backend.Dialer, insecure bool) *http.Client {
	if d == nil {
		d = &net.Dialer{}
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &idleConn{Conn: c}, nil
		},
		// Printers use self-signed certificates; verification is
		// switched off only with the insecure-tls option.
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // opt-in per printer
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 5 * time.Minute,
	}}
}

type idleConn struct{ net.Conn }

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(idleTimeout))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(idleTimeout))
	return c.Conn.Write(p)
}

// ---- printing ----

// Print asks the printer for its capabilities and writes a complete
// Print-Job request, the IPP message followed by the document, to out.
func (b *Backend) Print(ctx context.Context, doc io.Reader, opts backend.JobOptions, out io.Writer) error {
	s, err := parseSettings(opts)
	if err != nil {
		return err
	}
	caps, err := b.capabilities(ctx, httpClient(opts.Dialer, s.insecure), s.uri)
	if err != nil {
		// Nothing was sent; the printer may be off.
		return backend.Retryable(fmt.Errorf("ipp: ask the printer for its capabilities: %w", err))
	}
	copies := max(opts.Copies, 1)
	format, err := chooseFormat(s.format, caps, copies)
	if err != nil {
		return err
	}
	paper, err := render.ParsePaper(opts.PaperSize)
	if err != nil {
		return err
	}
	media := mediaName(paper)

	req := proto.NewRequest(proto.OpPrintJob, b.requestID.Add(1))
	req.Major, req.Minor = caps.version[0], caps.version[1]
	title := opts.Title
	if title == "" {
		title = "platen job " + opts.JobID
	}
	req.Add(proto.TagOperation,
		proto.String(proto.TagURI, "printer-uri", s.uri),
		proto.String(proto.TagName, "requesting-user-name", "platen"),
		proto.String(proto.TagName, "job-name", title),
		proto.String(proto.TagMimeType, "document-format", format))
	var job []proto.Attribute
	repeat := 1
	if copies > 1 {
		if caps.maxCopies() >= copies {
			job = append(job, proto.Integer("copies", copies))
		} else {
			repeat = copies // only for raster, see chooseFormat
		}
	}
	if media != "" && caps.media.Has(media) {
		job = append(job, proto.Keyword("media", media))
	}
	if s.sides != "" {
		if !caps.sides.Has(s.sides) {
			return fmt.Errorf("ipp: the printer does not support sides %s (sides-supported: %s)", s.sides, list(caps.sides))
		}
		job = append(job, proto.Keyword("sides", s.sides))
	}
	if s.color != "auto" && caps.colorModes.Has(s.color) {
		job = append(job, proto.Keyword("print-color-mode", s.color))
	}
	if s.quality != 0 && contains(caps.qualities, s.quality) {
		job = append(job, proto.Enum("print-quality", s.quality))
	}
	if len(job) > 0 {
		req.Add(proto.TagJob, job...)
	}
	if _, err := out.Write(req.Encode()); err != nil {
		return err
	}
	if format == formatPDF {
		_, err := io.Copy(out, doc)
		return err
	}
	return b.writeRaster(ctx, doc, opts, s, caps, paper, media, repeat, out)
}

// chooseFormat picks PDF when the printer accepts it and can make the
// copies, PWG Raster otherwise.
func chooseFormat(want string, caps *capabilities, copies int) (string, error) {
	pdf, raster := caps.formats.Has(formatPDF), caps.formats.Has(formatPWG)
	switch want {
	case "pdf":
		if !pdf {
			return "", fmt.Errorf("ipp: the printer does not accept PDF (document-format-supported: %s)", list(caps.formats))
		}
		if copies > caps.maxCopies() {
			return "", fmt.Errorf("ipp: the printer cannot print %d copies of a PDF", copies)
		}
		return formatPDF, nil
	case "pwg-raster":
		if !raster {
			return "", fmt.Errorf("ipp: the printer does not accept PWG Raster (document-format-supported: %s)", list(caps.formats))
		}
		return formatPWG, nil
	}
	switch {
	case pdf && copies <= caps.maxCopies():
		return formatPDF, nil
	case raster:
		return formatPWG, nil
	case pdf:
		return "", fmt.Errorf("ipp: the printer cannot print %d copies of a PDF and does not accept PWG Raster", copies)
	}
	return "", fmt.Errorf("ipp: the printer accepts neither PDF nor PWG Raster (document-format-supported: %s)", list(caps.formats))
}

func (b *Backend) writeRaster(ctx context.Context, doc io.Reader, opts backend.JobOptions, s *jobSettings, caps *capabilities, paper render.Paper, media string, repeat int, out io.Writer) error {
	dpi, err := chooseResolution(caps.resolution, opts.ResolutionDPI)
	if err != nil {
		return err
	}
	color := false
	switch {
	case s.color == "color" && !caps.types.Has("srgb_8"):
		return fmt.Errorf("ipp: the printer does not accept color PWG Raster (pwg-raster-document-type-supported: %s)", list(caps.types))
	case s.color != "monochrome" && caps.types.Has("srgb_8"):
		color = true
	case !caps.types.Has("sgray_8") && !caps.types.Has("srgb_8"):
		return fmt.Errorf("ipp: the printer accepts no 8-bit PWG Raster type (pwg-raster-document-type-supported: %s)", list(caps.types))
	}
	// Gray pages for a printer that only takes sRGB are sent as RGB.
	expand := !color && !caps.types.Has("sgray_8")
	duplex := strings.HasPrefix(s.sides, "two-sided")
	if duplex && caps.sheetBack != "" && caps.sheetBack != "normal" {
		return fmt.Errorf("ipp: two-sided PWG Raster for a printer with pwg-raster-document-sheet-back %q is not supported; use format=pdf or one-sided", caps.sheetBack)
	}
	data, err := render.ReadAllLimited(doc, maxDocument)
	if err != nil {
		return err
	}
	r, err := raster.SharedRenderer()
	if err != nil {
		return err
	}
	page := pwg.Page{
		DPI: dpi, WidthPt: int(paper.Width + 0.5), HeightPt: int(paper.Height + 0.5), MediaName: media,
		Duplex: duplex, Tumble: s.sides == "two-sided-short-edge", Quality: s.quality,
	}
	w := pwg.NewWriter(out)
	po := render.PageOptions{Paper: paper, DPI: dpi, AutoRotate: s.rotate}
	for range repeat {
		err := r.RenderImages(ctx, data, po, color, func(_ int, img image.Image) error {
			if expand {
				img = grayToRGBA(img.(*image.Gray))
			}
			return w.WritePage(page, img)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// chooseResolution picks the printer resolution equal to the requested
// one, else the highest below it, else the lowest.
func chooseResolution(supported []proto.Resolution, want int) (int, error) {
	if want <= 0 {
		want = 600
	}
	best, lowest := 0, 0
	for _, r := range supported {
		if r.X != r.Y {
			continue
		}
		if r.X == want {
			return want, nil
		}
		if r.X < want && r.X > best {
			best = r.X
		}
		if lowest == 0 || r.X < lowest {
			lowest = r.X
		}
	}
	switch {
	case best > 0:
		return best, nil
	case lowest > 0:
		return lowest, nil
	}
	return 0, errors.New("ipp: the printer reports no PWG Raster resolution")
}

func grayToRGBA(g *image.Gray) *image.RGBA {
	out := image.NewRGBA(g.Bounds())
	for i, v := range g.Pix {
		out.Pix[i*4], out.Pix[i*4+1], out.Pix[i*4+2], out.Pix[i*4+3] = v, v, v, 0xff
	}
	return out
}

// mediaName returns the PWG media name of the paper size.
func mediaName(p render.Paper) string {
	switch strings.ToLower(p.Name) {
	case "a4":
		return "iso_a4_210x297mm"
	case "a5":
		return "iso_a5_148x210mm"
	case "a6":
		return "iso_a6_105x148mm"
	case "letter":
		return "na_letter_8.5x11in"
	case "legal":
		return "na_legal_8.5x14in"
	}
	return ""
}

func contains(values []int, v int) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func list(a *proto.Attribute) string {
	if s := a.Strings(); len(s) > 0 {
		return strings.Join(s, ", ")
	}
	return "none"
}

// ---- delivery ----

// countingReader counts the bytes handed to the HTTP client.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Deliver posts the Print-Job request written by Print and waits until
// the printer completed the job, at most for the wait option.
func (b *Backend) Deliver(ctx context.Context, data io.Reader, size int64, opts backend.JobOptions) error {
	s, err := parseSettings(opts)
	if err != nil {
		return &printer.RefusedError{Err: err}
	}
	client := httpClient(opts.Dialer, s.insecure)
	body := &countingReader{r: data}
	res, err := proto.Post(ctx, client, s.uri, body, size)
	var httpErr *proto.HTTPError
	switch {
	case errors.As(err, &httpErr):
		b.forget(s.uri)
		return &printer.RefusedError{Retryable: httpErr.Status >= 500, Err: err}
	case err != nil:
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return &printer.SendError{Written: body.n, Err: err}
	}
	if err := res.Err(); err != nil {
		b.forget(s.uri)
		return &printer.RefusedError{Retryable: retryableStatus(res.Code), Err: err}
	}
	id, ok := res.Attr(proto.TagJob, "job-id").Int()
	if !ok || s.wait == 0 {
		return nil
	}
	return b.wait(ctx, client, s, id, res.Major, res.Minor)
}

func retryableStatus(code uint16) bool {
	switch code {
	case proto.StatusServiceUnavailable, proto.StatusDeviceError, proto.StatusTemporaryError,
		proto.StatusNotAcceptingJobs, proto.StatusBusy, proto.StatusInternalError:
		return true
	}
	return false
}

// wait polls the job state. The printer accepted the job, so a job that
// is still pending or stopped (e.g. out of paper) when the wait ends, or
// when ctx ends, counts as printed. Aborted and canceled jobs fail; the
// printer may have printed some pages.
func (b *Backend) wait(ctx context.Context, client *http.Client, s *jobSettings, id int, major, minor byte) error {
	deadline := time.Now().Add(s.wait)
	for {
		t := time.NewTimer(b.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		req := proto.NewRequest(proto.OpGetJobAttributes, b.requestID.Add(1))
		if major != 0 {
			req.Major, req.Minor = major, minor
		}
		req.Add(proto.TagOperation,
			proto.String(proto.TagURI, "printer-uri", s.uri),
			proto.Integer("job-id", id),
			proto.String(proto.TagName, "requesting-user-name", "platen"),
			proto.Keyword("requested-attributes", "job-state", "job-state-reasons", "job-state-message"))
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := proto.Do(qctx, client, s.uri, req)
		cancel()
		if err == nil && res.Err() == nil {
			state, _ := res.Attr(proto.TagJob, "job-state").Int()
			switch state {
			case proto.JobCompleted:
				return nil
			case proto.JobAborted, proto.JobCanceled:
				what := "aborted"
				if state == proto.JobCanceled {
					what = "canceled"
				}
				reasons := strings.Join(res.Attr(proto.TagJob, "job-state-reasons").Strings(), ", ")
				if m := res.Attr(proto.TagJob, "job-state-message").String(); m != "" {
					reasons = strings.TrimPrefix(reasons+"; "+m, "; ")
				}
				return fmt.Errorf("ipp: the printer %s job %d: %s", what, id, reasons)
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
	}
}
