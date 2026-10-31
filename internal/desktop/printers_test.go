package desktop

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/tim-riep/go-platen/internal/api"
)

func TestPlanPrinters(t *testing.T) {
	installed := []LocalPrinter{
		{Name: "Office", PortName: PortPrefix + "office"},
		{Name: "Old", PortName: `\\.\PIPE\Platen-old`}, // ports are case-insensitive
		{Name: "Lab", PortName: "USB001"},              // someone else's printer named Lab
		{Name: "Microsoft Print to PDF", PortName: "PORTPROMPT:"},
	}
	remote := []api.Printer{
		{ID: "office", Name: "Office 2", PaperSize: "A4"},
		{ID: "lab", Name: "lab", PaperSize: "letter"},
		{ID: "hall", Name: `Hall\East, 1`},
		{ID: "hall2", Name: "Hall-East- 1"},
		{ID: "zebra", Name: "Labels", ContentTypes: []string{"application/vnd.zebra-zpl"}},
	}
	got := PlanPrinters(installed, remote)
	want := []PrinterAction{
		{Kind: "remove", PrinterID: "old", Name: "Old"},
		{Kind: "add", PrinterID: "hall", Name: "Hall-East- 1"},
		{Kind: "add", PrinterID: "hall2", Name: "Hall-East- 1 (hall2)"},
		{Kind: "add", PrinterID: "lab", Name: "lab (platen)", PaperSize: "Letter"},
		{Kind: "rename", PrinterID: "office", Name: "Office 2", OldName: "Office"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan\n got %+v\nwant %+v", got, want)
	}
	// Removing everything touches only the app's printers.
	all := PlanPrinters(installed, nil)
	if len(all) != 2 || all[0].Kind != "remove" || all[1].Kind != "remove" {
		t.Fatalf("remove all %+v", all)
	}
	if _, ok := (LocalPrinter{PortName: PortPrefix + "../x"}).PrinterID(); ok {
		t.Fatal("invalid ID in a port name accepted")
	}
	if err := (PrinterChange{Location: "platen on x", Actions: got}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPrinterChangeValidate(t *testing.T) {
	for _, a := range []PrinterAction{
		{Kind: "add", PrinterID: "../x", Name: "x"},
		{Kind: "add", PrinterID: "x", Name: `a\b`},
		{Kind: "add", PrinterID: "x", Name: "x", PaperSize: "A4; rm"},
		{Kind: "exec", PrinterID: "x", Name: "x"},
		{Kind: "rename", PrinterID: "x", Name: "x"},
	} {
		if (PrinterChange{Actions: []PrinterAction{a}}).Validate() == nil {
			t.Errorf("accepted %+v", a)
		}
	}
}

func TestApplyScript(t *testing.T) {
	s := ApplyScript([]PrinterAction{
		{Kind: "remove", PrinterID: "old", Name: "Old"},
		{Kind: "add", PrinterID: "lab", Name: "Bob's printer", PaperSize: "A4"},
		{Kind: "rename", PrinterID: "office", Name: "Office 2", OldName: "Office"},
	}, "platen on server")
	for _, want := range []string{
		"Remove-Printer -Name 'Old'",
		`Remove-PrinterPort -Name '\\.\pipe\platen-old'`,
		`Add-PrinterPort -Name '\\.\pipe\platen-lab'`,
		`Add-Printer -Name 'Bob''s printer' -DriverName 'Microsoft Print To PDF' -PortName '\\.\pipe\platen-lab' -Location 'platen on server'`,
		"Set-PrintConfiguration -PrinterName 'Bob''s printer' -PaperSize A4",
		"Rename-Printer -Name 'Office' -NewName 'Office 2'",
		"exit 1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	enc, _ := base64.StdEncoding.DecodeString(EncodeCommand("aä"))
	if string(enc) != "a\x00\xe4\x00" {
		t.Fatalf("encoding %q", enc)
	}
}

func TestServerURL(t *testing.T) {
	for in, want := range map[string]string{
		"printserver": "http://printserver", "printserver.netbird.cloud/": "http://printserver.netbird.cloud",
		"100.64.0.1:8080": "http://100.64.0.1:8080", " https://ps.example ": "https://ps.example",
	} {
		if got, err := ServerURL(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "ftp://x", "http://x/path", "http://", "http://u@x", "x?y"} {
		if _, err := ServerURL(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if got := truncate("aä"+strings.Repeat("x", 300), 255); len(got) != 255 {
		t.Fatal(len(got))
	}
	if truncate("ää", 3) != "ä" {
		t.Fatal("truncate splits characters")
	}
}
