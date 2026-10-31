package desktop

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/tim-riep/go-platen/internal/api"
)

// Local printers on Windows. Each platen printer becomes a printer with
// the "Microsoft Print To PDF" driver on a local port that is the named
// pipe \\.\pipe\platen-<printer ID>. The driver renders every document to
// PDF and the spooler writes it into the pipe, where the app reads it. Pipe
// names belong to the app alone, so unlike a TCP port nothing else can
// occupy them; the port name also marks the printers the app manages.

const (
	PortPrefix  = `\\.\pipe\platen-`
	PDFDriver   = "Microsoft Print To PDF"
	nameSuffix  = " (platen)"
	maxNameRune = 200
)

// LocalPrinter is a printer installed on this computer.
type LocalPrinter struct {
	Name     string `json:"Name"`
	PortName string `json:"PortName"`
}

// PrinterID returns the platen printer of a managed printer.
func (p LocalPrinter) PrinterID() (string, bool) {
	if len(p.PortName) <= len(PortPrefix) || !strings.EqualFold(p.PortName[:len(PortPrefix)], PortPrefix) {
		return "", false
	}
	id := p.PortName[len(PortPrefix):]
	return id, validPrinterID.MatchString(id)
}

// PrinterAction is a change to the local printers.
type PrinterAction struct {
	Kind      string `json:"kind"` // add, rename, remove
	PrinterID string `json:"printer_id"`
	Name      string `json:"name"`
	OldName   string `json:"old_name,omitempty"`
	PaperSize string `json:"paper_size,omitempty"`
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

// PlanPrinters returns the changes that make the local printers match the
// platen printers. Names already used by other printers get a suffix.
func PlanPrinters(installed []LocalPrinter, remote []api.Printer) []PrinterAction {
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
	for _, r := range remote {
		if !AcceptsPDF(r) {
			continue
		}
		wanted[r.ID] = true
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
		switch {
		case !ok:
			actions = append(actions, PrinterAction{Kind: "add", PrinterID: r.ID, Name: name, PaperSize: windowsPaperSize(r.PaperSize)})
		case cur.Name != name:
			actions = append(actions, PrinterAction{Kind: "rename", PrinterID: r.ID, Name: name, OldName: cur.Name})
		}
	}
	for id, p := range managed {
		if !wanted[id] {
			actions = append(actions, PrinterAction{Kind: "remove", PrinterID: id, Name: p.Name})
		}
	}
	// Removals first, so their names are free for additions and renames.
	slices.SortStableFunc(actions, func(a, b PrinterAction) int {
		ra, rb := a.Kind == "remove", b.Kind == "remove"
		switch {
		case ra && !rb:
			return -1
		case rb && !ra:
			return 1
		}
		return strings.Compare(a.PrinterID, b.PrinterID)
	})
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
		switch a.Kind {
		case "add":
			if a.PaperSize != "" && windowsPaperSize(a.PaperSize) != a.PaperSize {
				return fmt.Errorf("invalid paper size %q", a.PaperSize)
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
		port := psQuote(PortPrefix + a.PrinterID)
		var body string
		switch a.Kind {
		case "add":
			body = fmt.Sprintf(`if (-not (Get-PrinterPort -Name %[1]s -ErrorAction SilentlyContinue)) {
    Add-PrinterPort -Name %[1]s
  }
  Add-Printer -Name %[2]s -DriverName %[3]s -PortName %[1]s -Location %[4]s -Comment 'platen'`,
				port, psQuote(a.Name), psQuote(PDFDriver), psQuote(location))
			if a.PaperSize != "" {
				body += fmt.Sprintf("\n  Set-PrintConfiguration -PrinterName %s -PaperSize %s", psQuote(a.Name), a.PaperSize)
			}
		case "rename":
			body = fmt.Sprintf("Rename-Printer -Name %s -NewName %s", psQuote(a.OldName), psQuote(a.Name))
		case "remove":
			// The spooler may hold the port for a moment after the printer is gone.
			body = fmt.Sprintf(`Remove-Printer -Name %s
  for ($i = 0; $i -lt 10; $i++) {
    try { Remove-PrinterPort -Name %s; break } catch { Start-Sleep -Milliseconds 500 }
  }`, psQuote(a.Name), port)
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
