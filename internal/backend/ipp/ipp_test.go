package ipp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/backend"
	proto "github.com/tim-riep/go-platen/internal/ipp"
	"github.com/tim-riep/go-platen/internal/printer"
	"github.com/tim-riep/go-platen/internal/pwg"
)

// fakePrinter is an IPP Everywhere printer.
type fakePrinter struct {
	mu          sync.Mutex
	formats     []string
	types       []string
	resolutions []int
	maxCopies   int
	sides       []string
	sheetBack   string
	// printStatus is the status of Print-Job responses.
	printStatus uint16
	// states are returned by Get-Job-Attributes in turn; the last repeats.
	states []int
	polls  int
	jobs   []fakeJob
}

type fakeJob struct {
	req *proto.Message
	doc []byte
}

func u32pair(a, b int) []byte {
	return binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, uint32(a)), uint32(b))
}

func (f *fakePrinter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != proto.ContentType || r.URL.Path != DefaultPath {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	br := bufio.NewReader(r.Body)
	req, err := proto.Decode(br)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	res := &proto.Message{Major: req.Major, Minor: req.Minor, Code: proto.StatusOK, RequestID: req.RequestID}
	res.Add(proto.TagOperation,
		proto.String(proto.TagCharset, "attributes-charset", "utf-8"),
		proto.String(proto.TagLanguage, "attributes-natural-language", "en"))
	switch req.Code {
	case proto.OpGetPrinterAttributes:
		attrs := []proto.Attribute{
			proto.String(proto.TagMimeType, "document-format-supported", f.formats...),
			proto.Keyword("ipp-versions-supported", "1.1", "2.0"),
			// A collection the decoder must skip, with a nested one.
			{Name: "media-col-default", Values: []proto.Value{
				{Tag: proto.TagBeginCollection},
				{Tag: proto.TagMemberName, Data: []byte("media-size")},
				{Tag: proto.TagBeginCollection},
				{Tag: proto.TagMemberName, Data: []byte("x-dimension")},
				{Tag: proto.TagInteger, Data: binary.BigEndian.AppendUint32(nil, 21000)},
				{Tag: proto.TagEndCollection},
				{Tag: proto.TagEndCollection},
			}},
			proto.Keyword("media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in"),
			proto.Keyword("print-color-mode-supported", "auto", "color", "monochrome"),
			proto.Enum("print-quality-supported", 3, 4, 5),
			{Name: "copies-supported", Values: []proto.Value{{Tag: proto.TagRange, Data: u32pair(1, f.maxCopies)}}},
		}
		if len(f.types) > 0 {
			attrs = append(attrs, proto.Keyword("pwg-raster-document-type-supported", f.types...))
		}
		if len(f.sides) > 0 {
			attrs = append(attrs, proto.Keyword("sides-supported", f.sides...))
		}
		if f.sheetBack != "" {
			attrs = append(attrs, proto.Keyword("pwg-raster-document-sheet-back", f.sheetBack))
		}
		resAttr := proto.Attribute{Name: "pwg-raster-document-resolution-supported"}
		for _, dpi := range f.resolutions {
			resAttr.Values = append(resAttr.Values, proto.Value{Tag: proto.TagResolution, Data: append(u32pair(dpi, dpi), 3)})
		}
		if len(resAttr.Values) > 0 {
			attrs = append(attrs, resAttr)
		}
		res.Add(proto.TagPrinter, attrs...)
	case proto.OpPrintJob:
		doc, _ := io.ReadAll(br)
		f.jobs = append(f.jobs, fakeJob{req: req, doc: doc})
		if f.printStatus != proto.StatusOK {
			res.Code = f.printStatus
			res.Add(proto.TagOperation, proto.String(proto.TagText, "status-message", "printer says no"))
			break
		}
		res.Add(proto.TagJob, proto.Integer("job-id", len(f.jobs)), proto.Enum("job-state", proto.JobPending))
	case proto.OpGetJobAttributes:
		state := proto.JobCompleted
		if len(f.states) > 0 {
			state = f.states[min(f.polls, len(f.states)-1)]
		}
		f.polls++
		res.Add(proto.TagJob, proto.Enum("job-state", state), proto.Keyword("job-state-reasons", "job-canceled-at-device"))
	default:
		res.Code = 0x0501
	}
	w.Header().Set("Content-Type", proto.ContentType)
	w.Write(res.Encode())
}

func (f *fakePrinter) lastJob(t *testing.T) fakeJob {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.jobs) == 0 {
		t.Fatal("no job received")
	}
	return f.jobs[len(f.jobs)-1]
}

func startPrinter(t *testing.T, f *fakePrinter) string {
	hs := httptest.NewServer(f)
	t.Cleanup(hs.Close)
	return strings.TrimPrefix(hs.URL, "http://")
}

func newTestBackend() *Backend {
	b := New()
	b.PollInterval = 10 * time.Millisecond
	return b
}

// print runs a job the way the worker does: Print into a file, then
// Deliver the file.
func print(t *testing.T, b *Backend, doc []byte, opts backend.JobOptions) error {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := b.Print(context.Background(), bytes.NewReader(doc), opts, f); err != nil {
		return err
	}
	size, _ := f.Seek(0, io.SeekCurrent)
	f.Seek(0, io.SeekStart)
	return b.Deliver(context.Background(), f, size, opts)
}

var testPDF = makePDF(595, 842, "0 0 0 rg 100 100 200 200 re f", "1 0 0 rg 50 600 100 100 re f")

func TestPDFPassThrough(t *testing.T) {
	f := &fakePrinter{formats: []string{"application/pdf", "image/pwg-raster", "image/urf"}, maxCopies: 99, sides: []string{"one-sided", "two-sided-long-edge"}, states: []int{proto.JobProcessing, proto.JobCompleted}}
	addr := startPrinter(t, f)
	b := newTestBackend()
	err := print(t, b, testPDF, backend.JobOptions{
		JobID: "j1", Title: "Invoice 4711", ContentType: "application/pdf", Copies: 2, PaperSize: "A4", ResolutionDPI: 600,
		Options: map[string]string{"sides": "two-sided-long-edge", "color": "monochrome", "quality": "high"}, Address: addr,
	})
	if err != nil {
		t.Fatal(err)
	}
	j := f.lastJob(t)
	if !bytes.Equal(j.doc, testPDF) {
		t.Fatal("the PDF was changed")
	}
	op := func(name string) *proto.Attribute { return j.req.Attr(proto.TagOperation, name) }
	job := func(name string) *proto.Attribute { return j.req.Attr(proto.TagJob, name) }
	if op("document-format").String() != "application/pdf" || op("job-name").String() != "Invoice 4711" ||
		op("printer-uri").String() != "ipp://"+addr+DefaultPath || op("requesting-user-name").String() != "platen" {
		t.Fatalf("operation attributes %+v", j.req.Groups[0])
	}
	if n, _ := job("copies").Int(); n != 2 || job("media").String() != "iso_a4_210x297mm" || job("sides").String() != "two-sided-long-edge" ||
		job("print-color-mode").String() != "monochrome" {
		t.Fatalf("job attributes %+v", j.req.Groups)
	}
	if q, _ := job("print-quality").Int(); q != 5 || j.req.Major != 2 {
		t.Fatalf("quality %d, version %d", q, j.req.Major)
	}
	if f.polls != 2 {
		t.Fatalf("%d job state queries, want 2", f.polls)
	}
}

func TestRaster(t *testing.T) {
	f := &fakePrinter{formats: []string{"image/pwg-raster", "image/jpeg"}, types: []string{"sgray_8", "srgb_8"}, resolutions: []int{75, 150}, maxCopies: 1}
	addr := startPrinter(t, f)
	b := newTestBackend()
	// Two copies on a printer without copies: the pages are repeated.
	opts := backend.JobOptions{JobID: "j1", ContentType: "application/pdf", Copies: 2, PaperSize: "A4", ResolutionDPI: 600, Address: addr}
	if err := print(t, b, testPDF, opts); err != nil {
		t.Fatal(err)
	}
	j := f.lastJob(t)
	if j.req.Attr(proto.TagOperation, "document-format").String() != "image/pwg-raster" || j.req.Attr(proto.TagJob, "copies") != nil {
		t.Fatalf("request %+v", j.req.Groups)
	}
	pages := rasterPages(t, j.doc)
	if len(pages) != 4 {
		t.Fatalf("%d pages, want 4", len(pages))
	}
	// 150 dpi is the best resolution below 600; color, since the printer
	// takes sRGB; A4 at 150 dpi.
	for _, p := range pages {
		if p.dpi != 150 || p.space != pwg.ColorSpaceRGB || p.w != 1240 || p.h != 1754 || p.media != "iso_a4_210x297mm" {
			t.Fatalf("page %+v", p)
		}
	}

	// Monochrome is sent as sGray.
	opts.Copies, opts.Options = 1, map[string]string{"color": "monochrome", "format": "pwg-raster"}
	if err := print(t, b, testPDF, opts); err != nil {
		t.Fatal(err)
	}
	if pages := rasterPages(t, f.lastJob(t).doc); len(pages) != 2 || pages[0].space != pwg.ColorSpaceGray {
		t.Fatalf("monochrome pages %+v", pages)
	}

	// The printer does not take PDF.
	opts.Options = map[string]string{"format": "pdf"}
	if err := print(t, b, testPDF, opts); err == nil || !strings.Contains(err.Error(), "does not accept PDF") {
		t.Fatalf("pdf on a raster printer: %v", err)
	}
}

func TestRasterLimits(t *testing.T) {
	f := &fakePrinter{formats: []string{"image/pwg-raster"}, types: []string{"sgray_8"}, resolutions: []int{300}, maxCopies: 1, sides: []string{"one-sided", "two-sided-long-edge"}, sheetBack: "rotated"}
	addr := startPrinter(t, f)
	b := newTestBackend()
	opts := backend.JobOptions{JobID: "j1", ContentType: "application/pdf", PaperSize: "A4", Address: addr}
	opts.Options = map[string]string{"color": "color"}
	if err := print(t, b, testPDF, opts); err == nil || !strings.Contains(err.Error(), "color") {
		t.Fatalf("color on a gray printer: %v", err)
	}
	opts.Options = map[string]string{"sides": "two-sided-long-edge"}
	if err := print(t, b, testPDF, opts); err == nil || !strings.Contains(err.Error(), "sheet-back") {
		t.Fatalf("duplex with rotated back sides: %v", err)
	}
	opts.Options = map[string]string{"sides": "two-sided-short-edge"}
	if err := print(t, b, testPDF, opts); err == nil || !strings.Contains(err.Error(), "does not support sides") {
		t.Fatalf("unsupported sides: %v", err)
	}
	f2 := &fakePrinter{formats: []string{"image/urf", "image/jpeg"}, maxCopies: 1}
	opts.Address, opts.Options = startPrinter(t, f2), nil
	if err := print(t, b, testPDF, opts); err == nil || !strings.Contains(err.Error(), "neither PDF nor PWG Raster") {
		t.Fatalf("URF-only printer: %v", err)
	}
}

func TestRefusedAndAborted(t *testing.T) {
	f := &fakePrinter{formats: []string{"application/pdf"}, maxCopies: 1, printStatus: proto.StatusBusy}
	addr := startPrinter(t, f)
	b := newTestBackend()
	opts := backend.JobOptions{JobID: "j1", ContentType: "application/pdf", Address: addr}
	var refused *printer.RefusedError
	if err := print(t, b, testPDF, opts); !errors.As(err, &refused) || !refused.Retryable {
		t.Fatalf("busy: %v", err)
	}
	f.mu.Lock()
	f.printStatus = proto.StatusDocumentFormatNotSupported
	f.mu.Unlock()
	if err := print(t, b, testPDF, opts); !errors.As(err, &refused) || refused.Retryable || !strings.Contains(err.Error(), "printer says no") {
		t.Fatalf("format not supported: %v", err)
	}
	// An aborted job may have printed pages: neither safe to retry nor
	// refused.
	f.mu.Lock()
	f.printStatus, f.states = proto.StatusOK, []int{proto.JobProcessing, proto.JobAborted}
	f.mu.Unlock()
	err := print(t, b, testPDF, opts)
	if err == nil || errors.As(err, &refused) || printer.IsSafeToRetry(err) || !strings.Contains(err.Error(), "aborted job 3: job-canceled-at-device") {
		t.Fatalf("aborted: %v", err)
	}
	// With wait=0 the job counts as printed once the printer accepted it.
	opts.Options = map[string]string{"wait": "0"}
	if err := print(t, b, testPDF, opts); err != nil {
		t.Fatal(err)
	}
}

func TestUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	b := newTestBackend()
	opts := backend.JobOptions{JobID: "j1", ContentType: "application/pdf", Address: addr}
	if err := b.Print(context.Background(), bytes.NewReader(testPDF), opts, io.Discard); !backend.IsRetryable(err) {
		t.Fatalf("print: %v", err)
	}
	if err := b.Deliver(context.Background(), bytes.NewReader([]byte("x")), 1, opts); !printer.IsSafeToRetry(err) {
		t.Fatalf("deliver: %v", err)
	}
	if err := b.Probe(context.Background(), &net.Dialer{}, addr, time.Second); err == nil {
		t.Fatal("probe succeeded")
	}
}

func TestPrinterURI(t *testing.T) {
	for _, c := range []struct {
		addr string
		opts map[string]string
		want string
	}{
		{"192.168.1.30", nil, "ipp://192.168.1.30:631/ipp/print"},
		{"canon.local:8631", nil, "ipp://canon.local:8631/ipp/print"},
		{"canon.local", map[string]string{"path": "ipp/printer"}, "ipp://canon.local:631/ipp/printer"},
		{"ipps://canon.local/ipp/print", nil, "ipps://canon.local/ipp/print"},
	} {
		if got, err := PrinterURI(c.addr, c.opts); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.addr, got, err, c.want)
		}
	}
	if _, err := PrinterURI("lpd://host/queue", nil); err == nil {
		t.Error("lpd accepted")
	}
	for k, v := range map[string]string{"format": "urf", "color": "rainbow", "sides": "both", "quality": "best", "wait": "soon", "insecure-tls": "maybe", "rotate": "left"} {
		if _, err := parseSettings(backend.JobOptions{Address: "h", Options: map[string]string{k: v}}); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

type rasterPage struct {
	dpi, w, h, space int
	media            string
}

// rasterPages reads the page headers of a PWG Raster stream and skips
// the compressed lines.
func rasterPages(t *testing.T, data []byte) []rasterPage {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("RaS2")) {
		t.Fatalf("not PWG Raster: %q", data[:min(len(data), 8)])
	}
	data = data[4:]
	var pages []rasterPage
	for len(data) > 0 {
		h := data[:pwg.HeaderSize]
		data = data[pwg.HeaderSize:]
		u32 := func(off int) int { return int(binary.BigEndian.Uint32(h[off:])) }
		p := rasterPage{dpi: u32(276), w: u32(372), h: u32(376), space: u32(400), media: string(bytes.TrimRight(h[1732:], "\x00"))}
		bpp := u32(388) / 8
		for y := 0; y < p.h; {
			y += int(data[0]) + 1
			data = data[1:]
			for n := 0; n < p.w; {
				c := int(data[0])
				data = data[1:]
				if c < 128 {
					n += c + 1
					data = data[bpp:]
				} else {
					n += 257 - c
					data = data[(257-c)*bpp:]
				}
			}
		}
		pages = append(pages, p)
	}
	return pages
}
