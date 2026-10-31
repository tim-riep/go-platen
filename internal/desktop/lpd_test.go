package desktop

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// conn feeds input in chunks and records what is written.
type conn struct {
	in    []byte
	chunk int
	out   bytes.Buffer
}

func (c *conn) Read(p []byte) (int, error) {
	if len(c.in) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.chunk, len(c.in))
	copy(p, c.in[:n])
	c.in = c.in[n:]
	return n, nil
}

func (c *conn) Write(p []byte) (int, error) { return c.out.Write(p) }

func lpdBytes(queue, control string, data []byte, dataLen int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\x02%s\n", queue)
	fmt.Fprintf(&b, "\x02%d cfA001host\n%s\x00", len(control), control)
	fmt.Fprintf(&b, "\x03%d dfA001host\n", dataLen)
	b.Write(data)
	if dataLen != 0 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

func TestControlFile(t *testing.T) {
	cf := ParseControlFile([]byte("Hpc\nPtim\nJQuarterly report\nldfA001\nldfA001\nNreport.pdf\nUdfA001\n"))
	if cf.User != "tim" || cf.Host != "pc" || cf.Title() != "Quarterly report" {
		t.Fatalf("%+v", cf)
	}
	if len(cf.Prints) != 1 || cf.Prints[0].Count != 2 {
		t.Fatalf("prints %+v", cf.Prints)
	}
	if ParseControlFile([]byte("NFile.pdf\n")).Title() != "File.pdf" || ParseControlFile([]byte("J \n")).Title() != "Untitled" {
		t.Fatal("title fallback")
	}
}

func TestReceiveLPD(t *testing.T) {
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("A"), 70000)...)
	for _, chunk := range []int{1, 7, 4096, 1 << 20} {
		dir := t.TempDir()
		c := &conn{in: lpdBytes("office", "Ptim\nJDoc\nldfA001host\nldfA001host\n", pdf, len(pdf)), chunk: chunk}
		jobs, err := ReceiveLPD(c, dir, LPDLimits{})
		if err != nil || len(jobs) != 1 {
			t.Fatalf("chunk %d: %v %+v", chunk, err, jobs)
		}
		j := jobs[0]
		if j.Queue != "office" || j.Control.Title() != "Doc" || j.Copies != 2 || len(j.Files) != 1 {
			t.Fatalf("job %+v", j)
		}
		if got, _ := os.ReadFile(j.Files[0]); !bytes.Equal(got, pdf) || ContentType(j.Files[0]) != "application/pdf" {
			t.Fatal("data file")
		}
		if !bytes.Equal(c.out.Bytes(), []byte{0, 0, 0, 0, 0}) {
			t.Fatalf("acks %v", c.out.Bytes())
		}
	}
}

func TestReceiveLPDStreamedAndErrors(t *testing.T) {
	dir := t.TempDir()
	pdf := []byte("%PDF-1.4 streamed")
	jobs, err := ReceiveLPD(&conn{in: lpdBytes("q", "JX\nldfA001host\n", pdf, 0), chunk: 3}, dir, LPDLimits{})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("streamed: %v %+v", err, jobs)
	}
	if got, _ := os.ReadFile(jobs[0].Files[0]); !bytes.Equal(got, pdf) {
		t.Fatalf("streamed data %q", got)
	}

	// Too large: nothing stays behind.
	dir = t.TempDir()
	if _, err := ReceiveLPD(&conn{in: lpdBytes("q", "JX\n", pdf, len(pdf)), chunk: 100}, dir, LPDLimits{MaxDataFile: 5}); err == nil {
		t.Fatal("limit not enforced")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files left: %v", entries)
	}

	bad := &conn{in: []byte("\x02q\n\x02x cfA\n"), chunk: 100}
	if _, err := ReceiveLPD(bad, dir, LPDLimits{}); err == nil || !bytes.Equal(bad.out.Bytes(), []byte{0, 1}) {
		t.Fatalf("bad length: %v %v", err, bad.out.Bytes())
	}
	state := &conn{in: []byte("\x03q\n"), chunk: 100}
	if jobs, err := ReceiveLPD(state, dir, LPDLimits{}); err != nil || jobs != nil || state.out.Len() == 0 {
		t.Fatal("queue state")
	}
	aborted := &conn{in: []byte("\x02q\n\x033 dfA\nabc\x00\x01\n"), chunk: 100}
	if jobs, err := ReceiveLPD(aborted, dir, LPDLimits{}); err != nil || len(jobs) != 0 {
		t.Fatalf("abort: %v %+v", err, jobs)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("aborted files left: %v", entries)
	}
	long := &conn{in: []byte("\x02" + strings.Repeat("q", 5000) + "\n"), chunk: 100}
	if _, err := ReceiveLPD(long, dir, LPDLimits{}); err == nil {
		t.Fatal("long line accepted")
	}
}
