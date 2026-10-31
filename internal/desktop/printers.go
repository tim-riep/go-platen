package desktop

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/tim-riep/go-platen/internal/api"
)

// Local printers on Windows. Each platen printer becomes a printer with
// the "Microsoft Print To PDF" driver on a Standard TCP/IP port in RAW mode
// to 127.0.0.1 and a port of its own, where the app listens. The driver
// renders every document to PDF and the spooler sends it there. The app
// picks free port numbers and moves a printer to another one if its port
// is taken. The Windows port is named platen-<port>-<printer ID>, which
// also marks the printers the app manages.
//
// Versions 0.1.3 and 0.1.4 used local ports that were named pipes
// (\\.\pipe\platen-<printer ID>); Windows 11 does not write print jobs into
// pipes. Such printers are moved to a TCP port.

const (
	PortPrefix  = "platen-"
	pipePrefix  = `\\.\pipe\platen-`
	PDFDriver   = "Microsoft Print To PDF"
	nameSuffix  = " (platen)"
	maxNameRune = 200
)

// PortName returns the Windows port of a printer listening on port.
func PortName(printerID string, port int) string {
	return fmt.Sprintf("%s%d-%s", PortPrefix, port, printerID)
}

// LocalPrinter is a printer installed on this computer.
type LocalPrinter struct {
	Name     string `json:"Name"`
	PortName string `json:"PortName"`
}

// Managed returns the platen printer and the TCP port of a printer the
// app manages; port is 0 for printers on an old pipe port.
func (p LocalPrinter) Managed() (id string, port int, ok bool) {
	return parsePortName(p.PortName)
}

// PrinterID returns the platen printer of a managed printer.
func (p LocalPrinter) PrinterID() (string, bool) {
	id, _, ok := p.Managed()
	return id, ok
}

func parsePortName(name string) (id string, port int, ok bool) {
	if len(name) > len(pipePrefix) && strings.EqualFold(name[:len(pipePrefix)], pipePrefix) {
		id = name[len(pipePrefix):]
		return id, 0, validPrinterID.MatchString(id)
	}
	if len(name) <= len(PortPrefix) || !strings.EqualFold(name[:len(PortPrefix)], PortPrefix) {
		return "", 0, false
	}
	num, id, found := strings.Cut(name[len(PortPrefix):], "-")
	port, err := strconv.Atoi(num)
	if !found || err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != num || !validPrinterID.MatchString(id) {
		return "", 0, false
	}
	return id, port, true
}

// PrinterAction is a change to the local printers.
type PrinterAction struct {
	Kind      string `json:"kind"` // add, move, rename, remove
	PrinterID string `json:"printer_id"`
	Name      string `json:"name"`
	OldName   string `json:"old_name,omitempty"`
	PaperSize string `json:"paper_size,omitempty"`
	// Port is the TCP port for add and move.
	Port int `json:"port,omitempty"`
	// OldPort is the Windows port that move and remove give up.
	OldPort string `json:"old_port,omitempty"`
}

// AcceptsPDF reports whether the printer's backend takes PDF documents.
func AcceptsPDF(p api.Printer) bool {
	return len(p.ContentTypes) == 0 || slices.Contains(p.ContentTypes, "application/pdf")
}

// PrinterName turns a platen printer name into a Windows printer name,
// which must not contain backslashes or commas.
func PrinterName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '\\' || r == ',' || r < 0x20 {
			return '-'
		}
		return r
	}, strings.TrimSpace(name))
	if r := []rune(name); len(r) > maxNameRune {
		name = string(r[:maxNameRune])
	}
	return name
}

// WantedPrinters returns the platen printers that get a local printer.
func WantedPrinters(remote []api.Printer) []api.Printer {
	var out []api.Printer
	for _, r := range remote {
		if AcceptsPDF(r) {
			out = append(out, r)
		}
	}
	return out
}

// PlanPrinters returns the changes that make the local printers match the
// platen printers, with each printer on the TCP port in ports. Printers
// without a port are left out; names already used by other printers get a
// suffix.
func PlanPrinters(installed []LocalPrinter, remote []api.Printer, ports map[string]int) []PrinterAction {
	managed := map[string]LocalPrinter{}
	taken := map[string]bool{} // lower-case names of printers the app does not manage
	for _, p := range installed {
		if id, ok := p.PrinterID(); ok {
			managed[id] = p
		} else {
			taken[strings.ToLower(p.Name)] = true
		}
	}
	var actions []PrinterAction
	wanted := map[string]bool{}
	used := map[string]bool{}
	for _, r := range WantedPrinters(remote) {
		wanted[r.ID] = true
		port := ports[r.ID]
		name := PrinterName(r.Name)
		if name == "" {
			name = r.ID
		}
		if taken[strings.ToLower(name)] {
			name += nameSuffix
		}
		if used[strings.ToLower(name)] {
			name += " (" + r.ID + ")"
		}
		used[strings.ToLower(name)] = true
		cur, ok := managed[r.ID]
		if !ok {
			if port > 0 {
				actions = append(actions, PrinterAction{Kind: "add", PrinterID: r.ID, Name: name, Port: port, PaperSize: windowsPaperSize(r.PaperSize)})
			}
			continue
		}
		if _, curPort, _ := cur.Managed(); port > 0 && curPort != port {
			actions = append(actions, PrinterAction{Kind: "move", PrinterID: r.ID, Name: cur.Name, Port: port, OldPort: cur.PortName})
		}
		if cur.Name != name {
			actions = append(actions, PrinterAction{Kind: "rename", PrinterID: r.ID, Name: name, OldName: cur.Name})
		}
	}
	for id, p := range managed {
		if !wanted[id] {
			actions = append(actions, PrinterAction{Kind: "remove", PrinterID: id, Name: p.Name, OldPort: p.PortName})
		}
	}
	// Removals first, so their names are free; moves before renames, which
	// change the name the move refers to.
	order := map[string]int{"remove": 0, "move": 1, "add": 2, "rename": 3}
	slices.SortStableFunc(actions, func(a, b PrinterAction) int {
		if d := order[a.Kind] - order[b.Kind]; d != 0 {
			return d
		}
		return strings.Compare(a.PrinterID, b.PrinterID)
	})
	return actions
}

// PlanMoves returns the moves of the managed printers to the ports in
// ports, e.g. at start when the server is not reachable.
func PlanMoves(installed []LocalPrinter, ports map[string]int) []PrinterAction {
	var actions []PrinterAction
	for _, p := range installed {
		id, cur, ok := p.Managed()
		if port := ports[id]; ok && port > 0 && port != cur {
			actions = append(actions, PrinterAction{Kind: "move", PrinterID: id, Name: p.Name, Port: port, OldPort: p.PortName})
		}
	}
	return actions
}

// windowsPaperSize maps platen's paper size to the PrintManagement
// PaperSize value.
func windowsPaperSize(s string) string {
	switch strings.ToLower(s) {
	case "a4":
		return "A4"
	case "a5":
		return "A5"
	case "letter":
		return "Letter"
	case "legal":
		return "Legal"
	}
	return ""
}

// PrinterChange is a set of printer actions.
type PrinterChange struct {
	Location string          `json:"location"`
	Actions  []PrinterAction `json:"actions"`
}

var validPrinterID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Validate checks a change before it runs with administrator rights, so
// that data from the server can only change the app's own printers.
func (c PrinterChange) Validate() error {
	if c.Location != PrinterName(c.Location) {
		return fmt.Errorf("invalid location %q", c.Location)
	}
	for _, a := range c.Actions {
		if !validPrinterID.MatchString(a.PrinterID) {
			return fmt.Errorf("invalid printer ID %q", a.PrinterID)
		}
		if a.Name == "" || a.Name != PrinterName(a.Name) || (a.Kind == "rename" && a.OldName == "") {
			return fmt.Errorf("invalid printer name %q", a.Name)
		}
		if a.Kind == "remove" || a.Kind == "move" {
			// Only the app's own ports may be given up.
			if id, _, ok := parsePortName(a.OldPort); !ok || id != a.PrinterID {
				return fmt.Errorf("invalid port %q", a.OldPort)
			}
		}
		switch a.Kind {
		case "add":
			if a.PaperSize != "" && windowsPaperSize(a.PaperSize) != a.PaperSize {
				return fmt.Errorf("invalid paper size %q", a.PaperSize)
			}
			fallthrough
		case "move":
			if a.Port < 1 || a.Port > 65535 {
				return fmt.Errorf("invalid port %d", a.Port)
			}
		case "rename", "remove":
		default:
			return fmt.Errorf("invalid action %q", a.Kind)
		}
	}
	return nil
}

// psQuote quotes s as a PowerShell string literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

const psPrologue = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
`

// ListPrintersScript prints all printers as JSON.
const ListPrintersScript = psPrologue + `ConvertTo-Json -Compress -InputObject @(Get-Printer | Select-Object Name, PortName)
`

// ApplyScript returns a PowerShell script that performs the actions with
// the PrintManagement cmdlets. It needs administrator rights. Every action
// runs even if an earlier one failed; failures are written to stderr and
// make the script exit with 1.
func ApplyScript(actions []PrinterAction, location string) string {
	var b strings.Builder
	b.WriteString(psPrologue)
	b.WriteString("$failed = @()\n")
	for _, a := range actions {
		// A RAW port to the app's listener; without SNMP, which would only
		// mark the printer as offline.
		addPort := fmt.Sprintf(`if (-not (Get-PrinterPort -Name %[1]s -ErrorAction SilentlyContinue)) {
    Add-PrinterPort -Name %[1]s -PrinterHostAddress '127.0.0.1' -PortNumber %[2]d
  }`, psQuote(PortName(a.PrinterID, a.Port)), a.Port)
		// The spooler may hold a port for a moment after it is released.
		removePort := fmt.Sprintf(`for ($i = 0; $i -lt 10; $i++) {
    try { Remove-PrinterPort -Name %s; break } catch { Start-Sleep -Milliseconds 500 }
  }`, psQuote(a.OldPort))
		var body string
		switch a.Kind {
		case "add":
			body = fmt.Sprintf("%s\n  Add-Printer -Name %s -DriverName %s -PortName %s -Location %s -Comment 'platen'",
				addPort, psQuote(a.Name), psQuote(PDFDriver), psQuote(PortName(a.PrinterID, a.Port)), psQuote(location))
			if a.PaperSize != "" {
				body += fmt.Sprintf("\n  Set-PrintConfiguration -PrinterName %s -PaperSize %s", psQuote(a.Name), a.PaperSize)
			}
		case "move":
			body = fmt.Sprintf("%s\n  Set-Printer -Name %s -PortName %s\n  %s",
				addPort, psQuote(a.Name), psQuote(PortName(a.PrinterID, a.Port)), removePort)
		case "rename":
			body = fmt.Sprintf("Rename-Printer -Name %s -NewName %s", psQuote(a.OldName), psQuote(a.Name))
		case "remove":
			body = fmt.Sprintf("Remove-Printer -Name %s\n  %s", psQuote(a.Name), removePort)
		default:
			continue
		}
		fmt.Fprintf(&b, "try {\n  %s\n} catch { $failed += %s + ': ' + $_.Exception.Message }\n",
			body, psQuote(a.Kind+" "+a.Name))
	}
	b.WriteString("if ($failed.Count -gt 0) { [Console]::Error.WriteLine(($failed -join \"`n\")); exit 1 }\n")
	return b.String()
}

// EncodeCommand encodes a script for powershell.exe -EncodedCommand.
func EncodeCommand(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
}
