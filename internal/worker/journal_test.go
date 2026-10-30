package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := j.Get("a"); err != nil || e != nil {
		t.Fatalf("missing entry: %v %v", e, err)
	}
	if err := j.Put(Entry{JobID: "a", PrinterID: "p", State: StateSent}); err != nil {
		t.Fatal(err)
	}
	if err := j.Put(Entry{JobID: "b", PrinterID: "p", State: StatePrinted}); err != nil {
		t.Fatal(err)
	}
	if err := j.Put(Entry{JobID: "../x", State: StateSent}); err == nil {
		t.Fatal("invalid id accepted")
	}
	// Leftover temp files are ignored.
	os.WriteFile(filepath.Join(dir, ".tmp-123"), []byte("garbage"), 0o600)

	// Reopening simulates a restart: "sent" becomes "uncertain".
	j, err = OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := j.Get("a")
	if a.State != StateUncertain || !a.Uncertain || a.Error == "" || !a.pendingReport() {
		t.Fatalf("entry a: %+v", a)
	}
	b, _ := j.Get("b")
	if b.State != StatePrinted {
		t.Fatalf("entry b: %+v", b)
	}
	if err := j.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if err := j.Remove("a"); err != nil {
		t.Fatal(err)
	}
	list, err := j.List()
	if err != nil || len(list) != 1 || list[0].JobID != "b" {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestBackoff(t *testing.T) {
	b := newBackoff(100, 1000)
	prev := b.next()
	for i := 0; i < 10; i++ {
		d := b.next()
		if d > 1000*5/4 {
			t.Fatalf("backoff %d exceeds max", d)
		}
		if i < 2 && d <= prev {
			t.Fatalf("backoff not growing: %d <= %d", d, prev)
		}
		prev = d
	}
	b.reset()
	if d := b.next(); d > 125 {
		t.Fatalf("reset backoff %d", d)
	}
}
