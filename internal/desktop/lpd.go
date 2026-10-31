// Package desktop holds the platform-neutral parts of the desktop print
// clients: the LPD receiver that local printer queues send their jobs to,
// the spool that keeps jobs until the server accepted them, the client of
// the server's /client/v1 API and the planning of the local printers.
package desktop

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Receiving side of the Line Printer Daemon protocol (RFC 1179), as far as
// print spoolers use it to deliver a job: "receive a printer job" with a
// control file and data files. The queue name is the platen printer ID.

// LPDLimits bounds what one connection may send.
type LPDLimits struct {
	MaxLine        int
	MaxControlFile int
	MaxDataFile    int64
}

// DefaultLPDLimits are used for zero fields.
var DefaultLPDLimits = LPDLimits{MaxLine: 1024, MaxControlFile: 64 << 10, MaxDataFile: 512 << 20}

// ControlFile holds the parts of an LPD control file platen uses.
type ControlFile struct {
	Host, User, JobName, SourceName string
	// Prints lists the data files in print order with the number of print
	// lines naming each, which is the number of copies.
	Prints []PrintLine
}

// PrintLine is a data file named by print commands of a control file.
type PrintLine struct {
	Name  string
	Count int
}

// printCommands are the control file commands that print a data file
// (RFC 1179, section 7).
const printCommands = "cdfglnoprtv"

// ParseControlFile parses an LPD control file.
func ParseControlFile(b []byte) ControlFile {
	var cf ControlFile
	for _, line := range strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' || r == '\r' }) {
		cmd, value := line[0], line[1:]
		switch cmd {
		case 'H':
			cf.Host = value
		case 'P':
			cf.User = value
		case 'J':
			cf.JobName = value
		case 'N':
			cf.SourceName = value
		default:
			if !strings.ContainsRune(printCommands, rune(cmd)) || value == "" {
				continue
			}
			found := false
			for i := range cf.Prints {
				if cf.Prints[i].Name == value {
					cf.Prints[i].Count++
					found = true
				}
			}
			if !found {
				cf.Prints = append(cf.Prints, PrintLine{Name: value, Count: 1})
			}
		}
	}
	return cf
}

// Title is the job title shown in platen.
func (cf ControlFile) Title() string {
	for _, s := range []string{cf.JobName, cf.SourceName} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "Untitled"
}

// LPDJob is a received job. Its data files are stored in the directory
// given to ReceiveLPD and belong to the caller.
type LPDJob struct {
	Queue   string
	Control ControlFile
	Files   []string
	Copies  int
}

// ReceiveLPD handles one LPD connection and returns the jobs it delivered.
// Data files are written to dir.
func ReceiveLPD(conn io.ReadWriter, dir string, limits LPDLimits) ([]LPDJob, error) {
	if limits.MaxLine == 0 {
		limits.MaxLine = DefaultLPDLimits.MaxLine
	}
	if limits.MaxControlFile == 0 {
		limits.MaxControlFile = DefaultLPDLimits.MaxControlFile
	}
	if limits.MaxDataFile == 0 {
		limits.MaxDataFile = DefaultLPDLimits.MaxDataFile
	}
	s := &lpdSession{r: bufio.NewReaderSize(conn, 64<<10), w: conn, dir: dir, limits: limits}
	line, err := s.readLine()
	if err == io.EOF || (err == nil && len(line) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch line[0] {
	case 0x02:
		queue, _, _ := strings.Cut(string(line[1:]), " ")
		if queue == "" {
			return nil, s.nak()
		}
		if err := s.ack(); err != nil {
			return nil, err
		}
		jobs, err := s.receiveJob(queue)
		if err != nil {
			s.cleanup()
		}
		return jobs, err
	case 0x03, 0x04:
		// Queue state: jobs are forwarded at once, there is no local queue.
		_, err := io.WriteString(conn, "no entries\n")
		return nil, err
	default:
		// Print waiting jobs, remove jobs: nothing to do.
		return nil, s.ack()
	}
}

type lpdSession struct {
	r      *bufio.Reader
	w      io.Writer
	dir    string
	limits LPDLimits
	files  map[string]string // data file name -> path
	order  []string
}

func (s *lpdSession) ack() error { _, err := s.w.Write([]byte{0}); return err }
func (s *lpdSession) nak() error { _, err := s.w.Write([]byte{1}); return err }

func (s *lpdSession) cleanup() {
	for _, p := range s.files {
		os.Remove(p)
	}
	s.files, s.order = nil, nil
}

// readLine returns the next line without the newline; io.EOF at the end.
func (s *lpdSession) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > s.limits.MaxLine {
			return nil, errors.New("lpd: command line too long")
		}
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF && len(line) > 0:
			return nil, errors.New("lpd: connection closed in a command")
		default:
			return nil, err
		}
	}
}

func (s *lpdSession) receiveJob(queue string) ([]LPDJob, error) {
	var controls []ControlFile
	s.files = map[string]string{}
	for {
		line, err := s.readLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		switch sub := line[0]; sub {
		case 0x01: // abort job
			s.cleanup()
			s.files = map[string]string{}
			controls = nil
			if err := s.ack(); err != nil {
				return nil, err
			}
		case 0x02, 0x03:
			countStr, name, ok := strings.Cut(string(line[1:]), " ")
			count, err := strconv.ParseInt(countStr, 10, 64)
			if !ok || err != nil || count < 0 || name == "" {
				s.nak()
				return nil, fmt.Errorf("lpd: bad subcommand %q", line)
			}
			if err := s.ack(); err != nil {
				return nil, err
			}
			if sub == 0x02 {
				if count > int64(s.limits.MaxControlFile) {
					return nil, errors.New("lpd: control file too large")
				}
				body := make([]byte, count)
				if _, err := io.ReadFull(s.r, body); err != nil {
					return nil, fmt.Errorf("lpd: control file: %w", err)
				}
				if err := s.terminator(); err != nil {
					return nil, err
				}
				controls = append(controls, ParseControlFile(body))
			} else {
				path, err := s.receiveFile(count)
				if err != nil {
					return nil, err
				}
				if old, ok := s.files[name]; ok {
					os.Remove(old)
				} else {
					s.order = append(s.order, name)
				}
				s.files[name] = path
				if count == 0 {
					// The file ran to the end of the connection.
					return s.jobs(queue, controls), nil
				}
			}
			if err := s.ack(); err != nil {
				return nil, err
			}
		default:
			s.nak()
			return nil, fmt.Errorf("lpd: unknown subcommand %d", sub)
		}
	}
	return s.jobs(queue, controls), nil
}

// receiveFile stores a data file. A count of 0 means "until the end of
// the connection", which some clients use for streamed jobs.
func (s *lpdSession) receiveFile(count int64) (string, error) {
	f, err := os.CreateTemp(s.dir, "lpd-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if count == 0 {
		n, err := io.Copy(f, io.LimitReader(s.r, s.limits.MaxDataFile+1))
		if err != nil {
			return fail(err)
		}
		if n > s.limits.MaxDataFile {
			return fail(errors.New("lpd: data file too large"))
		}
	} else {
		if count > s.limits.MaxDataFile {
			return fail(errors.New("lpd: data file too large"))
		}
		if _, err := io.CopyN(f, s.r, count); err != nil {
			return fail(fmt.Errorf("lpd: data file: %w", err))
		}
		if err := s.terminator(); err != nil {
			return fail(err)
		}
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	return path, nil
}

func (s *lpdSession) terminator() error {
	b, err := s.r.ReadByte()
	if err != nil || b != 0 {
		return errors.New("lpd: missing file terminator")
	}
	return nil
}

// jobs pairs the control files with the data files they print. Data files
// no control file names are given to the first control file without any,
// or dropped.
func (s *lpdSession) jobs(queue string, controls []ControlFile) []LPDJob {
	if len(s.files) == 0 {
		return nil
	}
	if len(controls) == 0 {
		controls = []ControlFile{{}}
	}
	used := map[string]bool{}
	var jobs []LPDJob
	for _, cf := range controls {
		var names []string
		copies := 1
		for _, p := range cf.Prints {
			if _, ok := s.files[p.Name]; ok && !used[p.Name] {
				names = append(names, p.Name)
				copies = max(copies, p.Count)
			}
		}
		if len(names) == 0 {
			for _, n := range s.order {
				if !used[n] {
					names = append(names, n)
				}
			}
		}
		if len(names) == 0 {
			continue
		}
		job := LPDJob{Queue: queue, Control: cf, Copies: copies}
		for _, n := range names {
			used[n] = true
			job.Files = append(job.Files, s.files[n])
		}
		jobs = append(jobs, job)
	}
	for n, p := range s.files {
		if !used[n] {
			os.Remove(p)
		}
	}
	s.files, s.order = nil, nil
	return jobs
}

// ContentType returns the MIME type of a document by its first bytes.
func ContentType(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return "application/pdf"
	case bytes.HasPrefix(head, []byte("%!")):
		return "application/postscript"
	}
	return "application/octet-stream"
}
