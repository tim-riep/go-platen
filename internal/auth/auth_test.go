package auth

import (
	"net/http"
	"strings"
	"testing"
)

func TestTokenHashVerify(t *testing.T) {
	tok := NewToken(PrefixApp)
	if !strings.HasPrefix(tok, PrefixApp) || len(tok) < 40 {
		t.Fatalf("token %q", tok)
	}
	if NewToken(PrefixApp) == tok {
		t.Fatal("tokens not random")
	}
	h := Hash(tok)
	if !ValidHash(h) {
		t.Fatalf("invalid hash %q", h)
	}
	if !Verify(tok, h) {
		t.Fatal("verify failed")
	}
	for _, bad := range []string{"", tok + "x", strings.TrimPrefix(tok, PrefixApp)} {
		if Verify(bad, h) {
			t.Fatalf("verified %q", bad)
		}
	}
	if Verify(tok, "") {
		t.Fatal("verified against empty hash")
	}
	for _, bad := range []string{"", "sha256:", "sha256:zz", "md5:" + strings.TrimPrefix(h, "sha256:")} {
		if ValidHash(bad) {
			t.Fatalf("ValidHash(%q)", bad)
		}
	}
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc": "abc", "bearer abc ": "abc", "Basic abc": "", "Bearer ": "", "": "",
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestNewID(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b || len(a) != 16 {
		t.Fatalf("ids %q %q", a, b)
	}
}
