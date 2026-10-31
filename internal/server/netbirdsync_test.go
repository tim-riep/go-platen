package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/netbirdapi"
	"github.com/tim-riep/go-platen/internal/store"
)

// fakeNetBird is a NetBird management API with a fixed token.
type fakeNetBird struct {
	mu    sync.Mutex
	peers []netbirdapi.Peer
	users []netbirdapi.User
}

func (f *fakeNetBird) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Token nbp_secret" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"token invalid"}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/peers":
		json.NewEncoder(w).Encode(f.peers)
	case "/api/users":
		json.NewEncoder(w).Encode(f.users)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newSyncEnv(t *testing.T) (*env, *fakeNetBird, string) {
	nb := &fakeNetBird{
		peers: []netbirdapi.Peer{
			{ID: "p1", Name: "laptop", IP: "100.64.0.1", DNSLabel: "laptop.netbird.test", UserID: "u1"},
			// The test client connects from 127.0.0.1 as peer-a.
			{ID: "p2", Name: "iphone", IP: "127.0.0.1", DNSLabel: "peer-a.netbird.test", UserID: "u1"},
			{ID: "p3", Name: "printer-gateway", IP: "100.64.0.9", DNSLabel: "gw.netbird.test"},
		},
		users: []netbirdapi.User{{ID: "u1", Name: "Tim", Email: "tim@example.com"}},
	}
	hs := httptest.NewServer(nb)
	t.Cleanup(hs.Close)
	e := newEnv(t, func(o *Options) {
		o.Panel = true
		o.PanelHosts = func() []string { return []string{"127.0.0.1"} }
		o.NetBirdAPIURL = hs.URL
	})
	e.peers.set("peer-a")
	// The laptop is the panel's admin.
	now := time.Now()
	if err := e.store.CreatePanelUser(context.Background(), &store.PanelUser{IP: "100.64.0.1", PeerID: "laptop-key", Role: store.PanelAdmin, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return e, nb, hs.URL
}

func TestNetBirdSyncInheritsRole(t *testing.T) {
	e, nb, url := newSyncEnv(t)

	// Without the sync the phone is not a panel user.
	e.expect(e.panel("GET", "/me", nil), 403)

	// A wrong token is rejected and nothing is stored.
	e.expect(e.do("PUT", "/admin/v1/netbird", e.admin, map[string]string{"api_url": url, "token": "nbp_wrong"}), 400)
	var st api.NetBirdSync
	e.expect(e.do("GET", "/admin/v1/netbird", e.admin, nil), 200).decode(t, &st)
	if st.Configured {
		t.Fatal("wrong token stored")
	}

	r := e.expect(e.do("PUT", "/admin/v1/netbird", e.admin, map[string]string{"api_url": url, "token": "nbp_secret"}), 200)
	if strings.Contains(string(r.body), "nbp_secret") {
		t.Fatal("token returned by the API")
	}
	r.decode(t, &st)
	if !st.Configured || st.TokenHint != "…cret" || st.Users != 1 || len(st.Devices) != 2 || st.LastSync == nil {
		t.Fatalf("status %+v", st)
	}
	for _, d := range st.Devices {
		want := map[string]string{"100.64.0.1": "user", "127.0.0.1": "netbird"}[d.IP]
		if d.Role != "admin" || d.Source != want || d.User != "Tim <tim@example.com>" {
			t.Fatalf("device %+v", d)
		}
	}

	// The phone now has the laptop's role.
	var me api.PanelMe
	e.expect(e.panel("GET", "/me", nil), 200).decode(t, &me)
	if me.Role != "admin" || me.Via != "Tim <tim@example.com>" || me.FirstAdmin {
		t.Fatalf("me %+v", me)
	}
	e.expect(e.panel("POST", "/workers", map[string]any{"id": "w1"}), 201)
	var users api.List[api.PanelUser]
	e.expect(e.panel("GET", "/users", nil), 200).decode(t, &users)
	if len(users.Items) != 1 || users.Items[0].NetBirdUser != "Tim <tim@example.com>" {
		t.Fatalf("users %+v", users)
	}

	// A different peer on the same IP gets nothing: the API names peer-a.
	e.peers.set("peer-b")
	e.expect(e.panel("GET", "/me", nil), 403)
	e.peers.set("peer-a")

	// A blocked NetBird user loses the inherited access with the next sync.
	nb.mu.Lock()
	nb.users[0].IsBlocked = true
	nb.mu.Unlock()
	e.expect(e.do("POST", "/admin/v1/netbird/sync", e.admin, nil), 200)
	e.expect(e.panel("GET", "/me", nil), 403)
	nb.mu.Lock()
	nb.users[0].IsBlocked = false
	nb.mu.Unlock()
	e.expect(e.do("POST", "/admin/v1/netbird/sync", e.admin, nil), 200)
	e.expect(e.panel("GET", "/me", nil), 200)

	// Removing the token ends the inheritance.
	e.expect(e.panel("DELETE", "/netbird", nil), 204)
	e.expect(e.panel("GET", "/me", nil), 403)
}

func TestNetBirdSyncElevatesViewer(t *testing.T) {
	e, _, url := newSyncEnv(t)
	now := time.Now()
	if err := e.store.CreatePanelUser(context.Background(), &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var me api.PanelMe
	e.expect(e.panel("GET", "/me", nil), 200).decode(t, &me)
	if me.Role != "viewer" {
		t.Fatalf("before sync %+v", me)
	}
	e.expect(e.do("PUT", "/admin/v1/netbird", e.admin, map[string]string{"api_url": url, "token": "nbp_secret"}), 200)
	var after api.PanelMe
	e.expect(e.panel("GET", "/me", nil), 200).decode(t, &after)
	if after.Role != "admin" || after.Via == "" {
		t.Fatalf("after sync %+v", after)
	}
	// The stored role is unchanged.
	u, _ := e.store.GetPanelUser(context.Background(), "127.0.0.1")
	if u.Role != store.PanelViewer {
		t.Fatalf("stored role changed to %s", u.Role)
	}
}

func TestNetBirdSyncStaleData(t *testing.T) {
	e, _, url := newSyncEnv(t)
	e.expect(e.do("PUT", "/admin/v1/netbird", e.admin, map[string]string{"api_url": url, "token": "nbp_secret"}), 200)
	e.expect(e.panel("GET", "/me", nil), 200)
	// The token stops working: the data is used for a while, then dropped.
	if err := e.store.SetSetting(context.Background(), settingNetBirdToken, "nbp_revoked", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.SyncNetBird(context.Background()); err == nil {
		t.Fatal("sync with a revoked token succeeded")
	}
	e.expect(e.panel("GET", "/me", nil), 200)
	e.clock.advance(nbStaleAfter + time.Minute)
	e.srv.SyncNetBird(context.Background())
	e.expect(e.panel("GET", "/me", nil), 403)
	var st api.NetBirdSync
	e.expect(e.do("GET", "/admin/v1/netbird", e.admin, nil), 200).decode(t, &st)
	if st.Error == "" {
		t.Fatal("sync error not reported")
	}
}
