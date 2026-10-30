package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestWriteOpenRemove(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	size, hash, err := s.Write("job1", strings.NewReader("hello"), 10)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	if size != 5 || hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("size %d hash %s", size, hash)
	}
	f, err := s.Open("job1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "hello" {
		t.Fatalf("content %q", b)
	}
	if err := s.Remove("job1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("job1"); err != nil {
		t.Fatal("removing missing document failed:", err)
	}
	if _, err := s.Open("job1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestLimit(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, _, err := s.Write("big", strings.NewReader("12345678901"), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	if _, _, err := s.Write("exact", strings.NewReader("1234567890"), 10); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List()
	if len(entries) != 1 || entries[0].ID != "exact" {
		t.Fatalf("leftover files: %+v", entries)
	}
}

func TestInvalidID(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, id := range []string{"", "../x", "a/b", ".incoming-1"} {
		if _, _, err := s.Write(id, strings.NewReader("x"), 0); err == nil {
			t.Fatalf("id %q accepted", id)
		}
	}
}
