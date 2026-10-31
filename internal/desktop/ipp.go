package desktop

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// A minimal IPP client (RFC 8010, RFC 8011) for the one request the Linux
// client needs: Get-Jobs, to read the title of the job CUPS is sending.

const (
	ippOpGetJobs          = 0x000A
	ippTagOperation       = 0x01
	ippTagJob             = 0x02
	ippTagEnd             = 0x03
	ippTagEnum            = 0x23
	ippTagText            = 0x41
	ippTagName            = 0x42
	ippTagKeyword         = 0x44
	ippTagURI             = 0x45
	ippTagCharset         = 0x47
	ippTagLanguage        = 0x48
	ippJobStateProcessing = 5
)

func ippAttr(b *bytes.Buffer, tag byte, name string, values ...string) {
	for i, v := range values {
		b.WriteByte(tag)
		if i > 0 {
			name = "" // additional value
		}
		binary.Write(b, binary.BigEndian, uint16(len(name)))
		b.WriteString(name)
		binary.Write(b, binary.BigEndian, uint16(len(v)))
		b.WriteString(v)
	}
}

// ippGetJobsRequest asks for the name and state of the printer's
// unfinished jobs.
func ippGetJobsRequest(printerURI string) []byte {
	var b bytes.Buffer
	b.Write([]byte{2, 0})
	binary.Write(&b, binary.BigEndian, uint16(ippOpGetJobs))
	binary.Write(&b, binary.BigEndian, uint32(1))
	b.WriteByte(ippTagOperation)
	ippAttr(&b, ippTagCharset, "attributes-charset", "utf-8")
	ippAttr(&b, ippTagLanguage, "attributes-natural-language", "en")
	ippAttr(&b, ippTagURI, "printer-uri", printerURI)
	ippAttr(&b, ippTagKeyword, "requested-attributes", "job-name", "job-state")
	ippAttr(&b, ippTagKeyword, "which-jobs", "not-completed")
	b.WriteByte(ippTagEnd)
	return b.Bytes()
}

type ippJob struct {
	name  string
	state int
}

// ippParseJobs reads the job groups of a Get-Jobs response.
func ippParseJobs(data []byte) ([]ippJob, error) {
	r := bytes.NewReader(data)
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if status := binary.BigEndian.Uint16(head[2:4]); status > 0xff {
		return nil, errors.New("ipp: request failed")
	}
	var jobs []ippJob
	var cur *ippJob
	last := ""
	for {
		tag, err := r.ReadByte()
		if err != nil {
			return nil, errors.New("ipp: truncated response")
		}
		if tag == ippTagEnd {
			return jobs, nil
		}
		if tag <= 0x0f {
			cur = nil
			if tag == ippTagJob {
				jobs = append(jobs, ippJob{})
				cur = &jobs[len(jobs)-1]
			}
			continue
		}
		var n uint16
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		name := make([]byte, n)
		if _, err := io.ReadFull(r, name); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		value := make([]byte, n)
		if _, err := io.ReadFull(r, value); err != nil {
			return nil, err
		}
		if len(name) > 0 {
			last = string(name)
		}
		if cur == nil || len(name) == 0 {
			continue
		}
		switch {
		case last == "job-name" && (tag == ippTagName || tag == ippTagText):
			cur.name = string(value)
		case last == "job-state" && tag == ippTagEnum && len(value) == 4:
			cur.state = int(binary.BigEndian.Uint32(value))
		}
	}
}

// ippActiveJobName returns the name of the job being processed, or of the
// only job.
func ippActiveJobName(jobs []ippJob) string {
	for _, j := range jobs {
		if j.state == ippJobStateProcessing {
			return j.name
		}
	}
	if len(jobs) == 1 {
		return jobs[0].name
	}
	return ""
}
