package netbirdapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token secret" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"token invalid","code":401}`))
			return
		}
		switch r.URL.Path {
		case "/api/peers":
			w.Write([]byte(`[{"id":"p1","name":"laptop","ip":"100.64.0.1","dns_label":"laptop.netbird.cloud","user_id":"u1","connected":true,"os":"Darwin"}]`))
		case "/api/users":
			w.Write([]byte(`[{"id":"u1","email":"tim@example.com","name":"Tim","role":"owner","status":"active"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hs.Close()
	c := &Client{BaseURL: hs.URL, Token: "secret"}
	peers, err := c.Peers(context.Background())
	if err != nil || len(peers) != 1 || peers[0].UserID != "u1" || peers[0].IP != "100.64.0.1" || peers[0].DNSLabel != "laptop.netbird.cloud" {
		t.Fatalf("peers %+v %v", peers, err)
	}
	users, err := c.Users(context.Background())
	if err != nil || len(users) != 1 || users[0].Email != "tim@example.com" {
		t.Fatalf("users %+v %v", users, err)
	}
	bad := &Client{BaseURL: hs.URL, Token: "wrong"}
	_, err = bad.Peers(context.Background())
	if e, ok := err.(*Error); !ok || e.Status != 401 || e.Message != "token invalid" {
		t.Fatalf("error %v", err)
	}
}

func TestBaseURLFor(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  DefaultURL,
		"https://api.netbird.io:443":        DefaultURL,
		"https://netbird.example.com":       "https://netbird.example.com",
		"https://netbird.example.com:443":   "https://netbird.example.com",
		"https://netbird.example.com:33073": "https://netbird.example.com:33073",
	} {
		if got := BaseURLFor(in); got != want {
			t.Errorf("BaseURLFor(%q) = %q, want %q", in, got, want)
		}
	}
}
