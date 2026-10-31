package desktop

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestCUPSParsing(t *testing.T) {
	devices := parseCUPSDevices("device for HP: ipp://10.0.0.5/ipp\ndevice for platen_office: socket://127.0.0.1:50001\nlpstat: other line\n")
	if devices["platen_office"] != "socket://127.0.0.1:50001" || len(devices) != 2 {
		t.Fatalf("devices %v", devices)
	}
	opts := parseCUPSOptions(`device-uri=socket://127.0.0.1:50001 printer-info='Office \'A\' printer' printer-location="2nd floor" printer-type=16810052` + "\n")
	if !reflect.DeepEqual(opts, map[string]string{"device-uri": "socket://127.0.0.1:50001", "printer-info": "Office 'A' printer",
		"printer-location": "2nd floor", "printer-type": "16810052"}) {
		t.Fatalf("options %v", opts)
	}
	p, ok := cupsPrinter("platen_office", "socket://127.0.0.1:50001", "Office")
	if id, port, _ := p.Managed(); !ok || id != "office" || port != 50001 || p.Name != "Office" {
		t.Fatalf("printer %+v", p)
	}
	for _, c := range [][2]string{{"HP", "socket://127.0.0.1:50001"}, {"platen_office", "lpd://127.0.0.1:6515/office"}, {"platen_../x", "socket://127.0.0.1:1"}} {
		if _, ok := cupsPrinter(c[0], c[1], ""); ok {
			t.Errorf("%v managed", c)
		}
	}
	args := lpadminArgs(PrinterAction{Kind: "add", PrinterID: "lab", Name: "Lab", Port: 50003, PaperSize: "Letter"}, "platen on x", "/p.ppd")
	want := []string{"-p", "platen_lab", "-D", "Lab", "-L", "platen on x", "-v", "socket://127.0.0.1:50003", "-P", "/p.ppd", "-E",
		"-o", "printer-is-shared=false", "-o", "printer-error-policy=retry-job", "-o", "PageSize=Letter"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("add args %v", args)
	}
	if a := lpadminArgs(PrinterAction{Kind: "move", PrinterID: "lab", Port: 50009}, "", ""); !reflect.DeepEqual(a, []string{"-p", "platen_lab", "-v", "socket://127.0.0.1:50009"}) {
		t.Fatalf("move args %v", a)
	}
	if a := lpadminArgs(PrinterAction{Kind: "remove", PrinterID: "lab"}, "", ""); !reflect.DeepEqual(a, []string{"-x", "platen_lab"}) {
		t.Fatalf("remove args %v", a)
	}
}

// ippResponse builds a Get-Jobs response with the given jobs.
func ippResponse(jobs []ippJob) []byte {
	var b bytes.Buffer
	b.Write([]byte{2, 0, 0, 0, 0, 0, 0, 1})
	b.WriteByte(ippTagOperation)
	ippAttr(&b, ippTagCharset, "attributes-charset", "utf-8")
	for _, j := range jobs {
		b.WriteByte(ippTagJob)
		ippAttr(&b, ippTagName, "job-name", j.name)
		state := make([]byte, 4)
		binary.BigEndian.PutUint32(state, uint32(j.state))
		ippAttr(&b, ippTagEnum, "job-state", string(state))
	}
	b.WriteByte(ippTagEnd)
	return b.Bytes()
}

func TestIPP(t *testing.T) {
	req := ippGetJobsRequest("ipp://localhost/printers/platen_lab")
	if !bytes.HasPrefix(req, []byte{2, 0, 0, 0x0a, 0, 0, 0, 1, ippTagOperation}) || req[len(req)-1] != ippTagEnd ||
		!bytes.Contains(req, []byte("printers/platen_lab")) || !bytes.Contains(req, []byte("not-completed")) {
		t.Fatalf("request %q", req)
	}
	jobs, err := ippParseJobs(ippResponse([]ippJob{{"Queued.pdf", 3}, {"Report Q3.pdf", 5}}))
	if err != nil || len(jobs) != 2 || ippActiveJobName(jobs) != "Report Q3.pdf" {
		t.Fatalf("jobs %+v %v", jobs, err)
	}
	if jobs, _ := ippParseJobs(ippResponse([]ippJob{{"Only.pdf", 3}})); ippActiveJobName(jobs) != "Only.pdf" {
		t.Fatal("single job")
	}
	if jobs, _ := ippParseJobs(ippResponse([]ippJob{{"a", 3}, {"b", 4}})); ippActiveJobName(jobs) != "" {
		t.Fatal("no active job")
	}
	bad := ippResponse(nil)
	bad[2] = 4 // client-error status
	if _, err := ippParseJobs(bad); err == nil {
		t.Fatal("error status accepted")
	}
	if _, err := ippParseJobs(ippResponse([]ippJob{{"x", 5}})[:20]); err == nil {
		t.Fatal("truncated response accepted")
	}
}
