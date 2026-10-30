package raw

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim-riep/go-platen/internal/backend"
)

func TestPrint(t *testing.T) {
	var out bytes.Buffer
	if err := (Backend{}).Print(context.Background(), strings.NewReader("^XA^XZ"), backend.JobOptions{Copies: 1}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "^XA^XZ" {
		t.Fatalf("got %q", out.String())
	}
}

func TestCopies(t *testing.T) {
	p := filepath.Join(t.TempDir(), "doc")
	os.WriteFile(p, []byte("ab"), 0o600)
	f, _ := os.Open(p)
	defer f.Close()
	var out bytes.Buffer
	if err := (Backend{}).Print(context.Background(), f, backend.JobOptions{Copies: 3}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ababab" {
		t.Fatalf("got %q", out.String())
	}
	if err := (Backend{}).Print(context.Background(), strings.NewReader("x"), backend.JobOptions{Copies: 2}, &out); err != nil {
		// strings.Reader is seekable, so this works too.
		t.Fatal(err)
	}
}

func TestRegistry(t *testing.T) {
	r := backend.NewRegistry(Factory)
	if ct, ok := r.ContentTypes(Name); !ok || ct[0] != "*/*" {
		t.Fatalf("content types %v %v", ct, ok)
	}
	if _, ok := r.ContentTypes("nope"); ok {
		t.Fatal("unknown backend found")
	}
	b1, err := r.Get(Name)
	if err != nil || !b1.Accepts("application/pdf") {
		t.Fatal(err)
	}
	if _, err := r.Get("nope"); err == nil {
		t.Fatal("unknown backend created")
	}
	if names := r.Names(); len(names) != 1 || names[0] != Name {
		t.Fatal(names)
	}
}
