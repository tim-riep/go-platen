package integration

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/ipp"
	"github.com/tim-riep/go-platen/internal/store"
	"github.com/tim-riep/go-platen/internal/worker"
)

// ippPrinter accepts PDF over IPP. The first Print-Job is refused as busy.
type ippPrinter struct {
	mu   sync.Mutex
	jobs []string
}

func (p *ippPrinter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	br := bufio.NewReader(r.Body)
	req, err := ipp.Decode(br)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	res := &ipp.Message{Major: 2, Code: ipp.StatusOK, RequestID: req.RequestID}
	res.Add(ipp.TagOperation, ipp.String(ipp.TagCharset, "attributes-charset", "utf-8"), ipp.String(ipp.TagLanguage, "attributes-natural-language", "en"))
	switch req.Code {
	case ipp.OpGetPrinterAttributes:
		res.Add(ipp.TagPrinter, ipp.String(ipp.TagMimeType, "document-format-supported", "application/pdf"), ipp.Keyword("ipp-versions-supported", "2.0"))
	case ipp.OpPrintJob:
		doc, _ := io.ReadAll(br)
		p.jobs = append(p.jobs, string(doc))
		if len(p.jobs) == 1 {
			res.Code = ipp.StatusBusy
			break
		}
		res.Add(ipp.TagJob, ipp.Integer("job-id", len(p.jobs)))
	case ipp.OpGetJobAttributes:
		res.Add(ipp.TagJob, ipp.Enum("job-state", ipp.JobCompleted))
	}
	w.Write(res.Encode())
}

func TestIPPPrinter(t *testing.T) {
	e := newEnv(t)
	p := &ippPrinter{}
	hs := httptest.NewServer(p)
	t.Cleanup(hs.Close)
	now := time.Now()
	err := e.store.CreatePrinter(e.ctx, &store.Printer{ID: "inkjet", Name: "Inkjet", WorkerID: store.LocalWorkerID, Address: strings.TrimPrefix(hs.URL, "http://"),
		Backend: "ipp", PaperSize: "A4", ResolutionDPI: 600, Status: store.PrinterReady, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	e.startWorker(&worker.InProcess{Service: e.srv.Workers, WorkerID: store.LocalWorkerID}, t.TempDir())

	const doc = "%PDF-1.4 not really a pdf, the printer takes it as it is"
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/printers/inkjet/jobs", e.http.URL), strings.NewReader(doc))
	req.Header.Set("Authorization", "Bearer "+e.appToken)
	req.Header.Set("Content-Type", "application/pdf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var j api.Job
	if resp.StatusCode != 202 || jsonDecode(resp.Body, &j) != nil {
		t.Fatalf("submit: %d", resp.StatusCode)
	}
	// The busy printer makes the first attempt fail; the retry prints.
	got := e.waitStatus(j.ID, store.JobDone)
	if got.Attempts != 2 {
		t.Fatalf("%d attempts, want 2", got.Attempts)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.jobs) != 2 || p.jobs[1] != doc {
		t.Fatalf("printer got %q", p.jobs)
	}
}
