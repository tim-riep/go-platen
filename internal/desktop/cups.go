package desktop

import (
	"fmt"
	"strconv"
	"strings"
)

// Local printers on Linux are CUPS queues named platen_<printer ID> with
// the device URI socket://127.0.0.1:<port> (CUPS' AppSocket backend) and
// the app's PPD, which makes CUPS deliver PDF. As on Windows, every printer
// has a port of its own that the app picks; the CUPS side is mapped onto
// LocalPrinter (Name is the queue's description, PortName the Windows-style
// platen-<port>-<id>) so that the planning is the same.

const cupsQueuePrefix = "platen_"

// CUPSQueue returns the queue name of a printer.
func CUPSQueue(printerID string) string { return cupsQueuePrefix + printerID }

// cupsDeviceURI returns the device URI of a printer listening on port.
func cupsDeviceURI(port int) string { return fmt.Sprintf("socket://127.0.0.1:%d", port) }

// cupsPrinter maps a queue to a LocalPrinter if the app manages it.
func cupsPrinter(queue, uri, description string) (LocalPrinter, bool) {
	id, ok := strings.CutPrefix(queue, cupsQueuePrefix)
	if !ok || !validPrinterID.MatchString(id) {
		return LocalPrinter{}, false
	}
	port := 0
	if rest, ok := strings.CutPrefix(uri, "socket://127.0.0.1:"); ok {
		if p, err := strconv.Atoi(strings.TrimRight(rest, "/")); err == nil && p > 0 && p < 65536 {
			port = p
		}
	}
	if port == 0 {
		// Not a URI the app writes; adding the printer again repairs it.
		return LocalPrinter{}, false
	}
	return LocalPrinter{Name: description, PortName: PortName(id, port)}, true
}

// parseCUPSDevices parses the lines of `lpstat -v` in the C locale,
// "device for NAME: URI".
func parseCUPSDevices(out string) map[string]string {
	devices := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "device for ")
		if !ok {
			continue
		}
		if name, uri, ok := strings.Cut(rest, ": "); ok {
			devices[name] = strings.TrimSpace(uri)
		}
	}
	return devices
}

// parseCUPSOptions parses the key=value list of `lpoptions -p`; values
// may be quoted with ' or " and contain backslash escapes.
func parseCUPSOptions(out string) map[string]string {
	result := map[string]string{}
	var key, value strings.Builder
	inValue := false
	var quote rune
	flush := func() {
		if key.Len() > 0 {
			result[key.String()] = value.String()
		}
		key.Reset()
		value.Reset()
		inValue = false
	}
	rs := []rune(strings.TrimSpace(out))
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			i++
			if inValue {
				value.WriteRune(rs[i])
			} else {
				key.WriteRune(rs[i])
			}
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				value.WriteRune(c)
			}
		case inValue && (c == '\'' || c == '"'):
			quote = c
		case c == ' ' || c == '\n':
			flush()
		case !inValue && c == '=':
			inValue = true
		case inValue:
			value.WriteRune(c)
		default:
			key.WriteRune(c)
		}
	}
	flush()
	return result
}

// cupsPageSize maps platen's paper size to the PPD's PageSize.
func cupsPageSize(s string) string {
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

// lpadminArgs returns the lpadmin arguments of an action.
func lpadminArgs(a PrinterAction, location, ppd string) []string {
	q := CUPSQueue(a.PrinterID)
	switch a.Kind {
	case "add":
		args := []string{"-p", q, "-D", a.Name, "-L", location, "-v", cupsDeviceURI(a.Port), "-P", ppd, "-E",
			"-o", "printer-is-shared=false", "-o", "printer-error-policy=retry-job"}
		if size := cupsPageSize(a.PaperSize); size != "" {
			args = append(args, "-o", "PageSize="+size)
		}
		return args
	case "move":
		return []string{"-p", q, "-v", cupsDeviceURI(a.Port)}
	case "rename":
		return []string{"-p", q, "-D", a.Name}
	case "remove":
		return []string{"-x", q}
	}
	return nil
}
