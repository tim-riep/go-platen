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
		{Name: "Office", PortName: PortName("office", 50001)},
		{Name: "Old", PortName: "PLATEN-50002-old"}, // port names are case-insensitive
		{Name: "Hall", PortName: `\\.\PIPE\platen-hall`},
		{Name: "Lab", PortName: "USB001"}, // someone else's printer named Lab
		{Name: "Microsoft Print to PDF", PortName: "PORTPROMPT:"},
	}
	remote := []api.Printer{
		{ID: "office", Name: "Office 2", PaperSize: "A4"},
		{ID: "lab", Name: "lab", PaperSize: "letter"},
		{ID: "hall", Name: "Hall"},
		{ID: "east", Name: `East\Wing, 1`},
		{ID: "east2", Name: "East-Wing- 1"},
		{ID: "noport", Name: "No port"},
		{ID: "zebra", Name: "Labels", ContentTypes: []string{"application/vnd.zebra-zpl"}},
	}
	ports := map[string]int{"office": 50001, "lab": 50003, "hall": 50004, "east": 50005, "east2": 50006}
	got := PlanPrinters(installed, remote, ports)
	want := []PrinterAction{
		{Kind: "remove", PrinterID: "old", Name: "Old", OldPort: "PLATEN-50002-old"},
		{Kind: "move", PrinterID: "hall", Name: "Hall", Port: 50004, OldPort: `\\.\PIPE\platen-hall`},
		{Kind: "add", PrinterID: "east", Name: "East-Wing- 1", Port: 50005},
		{Kind: "add", PrinterID: "east2", Name: "East-Wing- 1 (east2)", Port: 50006},
		{Kind: "add", PrinterID: "lab", Name: "lab (platen)", Port: 50003, PaperSize: "Letter"},
		{Kind: "rename", PrinterID: "office", Name: "Office 2", OldName: "Office"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan\n got %+v\nwant %+v", got, want)
	}
	if err := (PrinterChange{Location: "platen on x", Actions: got}).Validate(); err != nil {
		t.Fatal(err)
	}
	// A printer whose port is taken moves.
	moves := PlanMoves(installed, map[string]int{"office": 50010, "hall": 50004})
	if len(moves) != 2 || moves[0].Port != 50010 || moves[0].OldPort != PortName("office", 50001) || moves[1].PrinterID != "hall" {
		t.Fatalf("moves %+v", moves)
	}
	// Removing everything touches only the app's printers.
	all := PlanPrinters(installed, nil, nil)
	if len(all) != 3 {
		t.Fatalf("remove all %+v", all)
	}
	for _, name := range []string{"platen-0-x", "platen-70000-x", "platen-01-x", "platen-5-../x", "platen-x", "platen_x"} {
		if _, _, ok := parsePortName(name); ok {
			t.Errorf("port name %q accepted", name)
		}
	}
}

func TestPrinterChangeValidate(t *testing.T) {
	for _, a := range []PrinterAction{
		{Kind: "add", PrinterID: "../x", Name: "x", Port: 1},
		{Kind: "add", PrinterID: "x", Name: `a\b`, Port: 1},
		{Kind: "add", PrinterID: "x", Name: "x", Port: 1, PaperSize: "A4; rm"},
		{Kind: "add", PrinterID: "x", Name: "x"},
		{Kind: "exec", PrinterID: "x", Name: "x"},
		{Kind: "rename", PrinterID: "x", Name: "x"},
		{Kind: "remove", PrinterID: "x", Name: "x", OldPort: "USB001"},
		{Kind: "remove", PrinterID: "x", Name: "x", OldPort: PortName("y", 5)},
		{Kind: "move", PrinterID: "x", Name: "x", Port: 0, OldPort: PortName("x", 5)},
	} {
		if (PrinterChange{Actions: []PrinterAction{a}}).Validate() == nil {
			t.Errorf("accepted %+v", a)
		}
	}
}

func TestApplyScript(t *testing.T) {
	s := ApplyScript([]PrinterAction{
		{Kind: "remove", PrinterID: "old", Name: "Old", OldPort: PortName("old", 50002)},
		{Kind: "move", PrinterID: "hall", Name: "Hall", Port: 50004, OldPort: `\\.\pipe\platen-hall`},
		{Kind: "add", PrinterID: "lab", Name: "Bob's printer", Port: 50003, PaperSize: "A4"},
		{Kind: "rename", PrinterID: "office", Name: "Office 2", OldName: "Office"},
	}, "platen on server")
	for _, want := range []string{
		"Remove-Printer -Name 'Old'",
		"Remove-PrinterPort -Name 'platen-50002-old'",
		"Add-PrinterPort -Name 'platen-50004-hall' -PrinterHostAddress '127.0.0.1' -PortNumber 50004",
		"Set-Printer -Name 'Hall' -PortName 'platen-50004-hall'",
		`Remove-PrinterPort -Name '\\.\pipe\platen-hall'`,
		"Add-PrinterPort -Name 'platen-50003-lab' -PrinterHostAddress '127.0.0.1' -PortNumber 50003",
		"Add-Printer -Name 'Bob''s printer' -DriverName 'Microsoft Print To PDF' -PortName 'platen-50003-lab' -Location 'platen on server'",
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
