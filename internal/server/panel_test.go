package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/store"
)

func newPanelEnv(t *testing.T) *env {
	e := newEnv(t, func(o *Options) {
		o.Panel = true
		o.PanelHosts = func() []string { return []string{"127.0.0.1", "platen-server.mesh.test"} }
		o.BackendNames = []string{"pdf", "raw"}
	})
	e.peers.set("peer-a")
	return e
}

// panel calls the panel API like the panel's own page does.
func (e *env) panel(method, path string, body any, header ...string) resp {
	e.t.Helper()
	return e.do(method, "/panel/v1"+path, "", body, append([]string{"X-Platen-Panel", "1"}, header...)...)
}

func TestPanelFirstAdmin(t *testing.T) {
	e := newPanelEnv(t)
	var me api.PanelMe
	e.expect(e.panel("GET", "/me", nil), 200).decode(t, &me)
	if !me.FirstAdmin || me.Role != "admin" || me.IP != "127.0.0.1" || me.PeerID != "peer-a" || !me.Bound {
		t.Fatalf("first visit: %+v", me)
	}
	var again api.PanelMe
	e.expect(e.panel("GET", "/me", nil), 200).decode(t, &again)
	if again.FirstAdmin || again.Role != "admin" {
		t.Fatalf("second visit: %+v", again)
	}

	// The same IP from a different NetBird peer is refused.
	e.peers.set("peer-b")
	if code := e.expect(e.panel("GET", "/me", nil), 403).errCode(t); code != api.CodeForbidden {
		t.Fatal(code)
	}
	// The admin API resets the binding; the next visit binds peer-b.
	e.expect(e.do("POST", "/admin/v1/panel-users/127.0.0.1/reset-peer", e.admin, nil), 200)
	var rebound api.PanelMe
	e.expect(e.panel("GET", "/me", nil), 200).decode(t, &rebound)
	if rebound.PeerID != "peer-b" || rebound.FirstAdmin {
		t.Fatalf("after reset: %+v", rebound)
	}
}

func TestPanelUnknownPeer(t *testing.T) {
	e := newPanelEnv(t)
	now := time.Now()
	if err := e.store.CreatePanelUser(context.Background(), &store.PanelUser{IP: "100.64.0.9", Role: store.PanelAdmin, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	r := e.expect(e.panel("GET", "/me", nil), 403)
	if !strings.Contains(string(r.body), "not a panel user") {
		t.Fatalf("body %s", r.body)
	}
	// Peers outside the mesh (not resolvable) never get in.
	e.peers.set("")
	e.expect(e.panel("GET", "/me", nil), 403)
}

func TestPanelRequestChecks(t *testing.T) {
	e := newPanelEnv(t)
	// Missing custom header: a plain cross-site request.
	e.expect(e.do("GET", "/panel/v1/me", "", nil), 403)
	// Wrong Host: DNS rebinding.
	e.expect(e.panel("GET", "/me", nil, "Host", "evil.example"), 403)
	e.expect(e.do("GET", "/", "", nil, "Host", "evil.example"), 403)
	// Foreign origin on a state-changing request.
	e.expect(e.panel("GET", "/me", nil), 200)
	e.expect(e.panel("POST", "/workers", map[string]any{"id": "w9"}, "Origin", "http://evil.example"), 403)
	e.expect(e.panel("POST", "/workers", map[string]any{"id": "w9"}, "Origin", "http://platen-server.mesh.test"), 201)
	// Without Origin header (same-origin GET-like tools) it works too.
	e.expect(e.panel("PATCH", "/workers/w9", map[string]any{"name": "Nine"}), 200)

	// The page itself is served with a restrictive policy.
	page := e.expect(e.do("GET", "/", "", nil), 200)
	if !strings.Contains(string(page.body), "/panel/app.js") || !strings.Contains(page.header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("page: %v %s", page.header, page.body)
	}
	if js := e.expect(e.do("GET", "/panel/app.js", "", nil), 200); !strings.Contains(string(js.body), "X-Platen-Panel") {
		t.Fatal("app.js not served")
	}
}

func TestPanelRoles(t *testing.T) {
	e := newPanelEnv(t)
	now := time.Now()
	if err := e.store.CreatePanelUser(context.Background(), &store.PanelUser{IP: "100.64.0.1", Role: store.PanelAdmin, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreatePanelUser(context.Background(), &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var me api.PanelMe
	e.expect(e.panel("GET", "/me", nil), 200).decode(t, &me)
	if me.Role != "viewer" || me.FirstAdmin || !me.Bound {
		t.Fatalf("viewer: %+v", me)
	}
	e.expect(e.panel("GET", "/printers", nil), 200)
	e.expect(e.panel("GET", "/jobs?limit=5", nil), 200)
	var backends api.List[api.Backend]
	e.expect(e.panel("GET", "/backends", nil), 200).decode(t, &backends)
	if len(backends.Items) != 2 || backends.Items[0].Name != "pdf" {
		t.Fatalf("backends %+v", backends)
	}
	e.expect(e.panel("POST", "/workers", map[string]any{"id": "w9"}), 403)
	e.expect(e.panel("GET", "/users", nil), 403)
	e.expect(e.panel("POST", "/printers/p1/jobs", []byte("x"), "Content-Type", "text/plain"), 403)
}

func TestPanelAdminActions(t *testing.T) {
	e := newPanelEnv(t)
	e.expect(e.panel("GET", "/me", nil), 200)

	// The panel reuses the admin handlers.
	e.expect(e.panel("POST", "/workers", map[string]any{"id": "site1", "name": "Site 1"}), 201)
	var tok api.EnrollmentToken
	e.expect(e.panel("POST", "/workers/site1/enrollment-tokens", map[string]any{"ttl_seconds": 600}), 201).decode(t, &tok)
	if tok.Token == "" {
		t.Fatal("no enrollment token")
	}
	e.expect(e.panel("POST", "/printers", map[string]any{"id": "p1", "worker_id": "site1", "address": "10.0.0.1", "backend": "raw",
		"options": map[string]string{"dither": "halftone"}}), 201)
	var client api.AppClient
	e.expect(e.panel("POST", "/app-clients", map[string]any{"id": "erp", "printers": []string{"p1"}}), 201).decode(t, &client)
	if client.Token == "" {
		t.Fatal("no app token")
	}

	// Printing a file from the panel.
	var job api.Job
	e.expect(e.panel("POST", "/printers/p1/jobs?copies=2&title=test.pdf", []byte("%PDF-1.4"), "Content-Type", "application/pdf"), 202).decode(t, &job)
	if job.Status != "queued" || job.Copies != 2 || job.Title != "test.pdf" {
		t.Fatalf("panel job %+v", job)
	}
	var aj api.AdminJob
	e.expect(e.panel("GET", "/jobs/"+job.ID, nil), 200).decode(t, &aj)
	if aj.AppClientID != "panel:127.0.0.1" {
		t.Fatalf("submitter %q", aj.AppClientID)
	}
	e.expect(e.panel("POST", "/printers/nope/jobs", []byte("x"), "Content-Type", "text/plain"), 404)
	// Retry and delete.
	e.expect(e.panel("POST", "/jobs/"+job.ID+"/retry", nil), 409)
	e.expect(e.panel("POST", "/jobs/"+job.ID+"/cancel", nil), 200)
	var retried api.AdminJob
	e.expect(e.panel("POST", "/jobs/"+job.ID+"/retry", nil), 200).decode(t, &retried)
	if retried.Status != "queued" {
		t.Fatalf("retried %+v", retried)
	}
	e.expect(e.panel("DELETE", "/jobs/"+job.ID, nil), 204)
	e.expect(e.panel("GET", "/jobs/"+job.ID, nil), 404)
	// The document is gone with the job.
	if entries, _ := e.srv.spool.List(); len(entries) != 0 {
		t.Fatalf("spool after delete: %+v", entries)
	}
	// The admin API prints too.
	e.expect(e.do("POST", "/admin/v1/printers/p1/jobs", e.admin, []byte("^XA^XZ"), "Content-Type", "text/plain"), 202)

	// Users.
	var u api.PanelUser
	e.expect(e.panel("POST", "/users", map[string]any{"ip": "100.64.0.7", "name": "Ops", "role": "viewer"}), 201).decode(t, &u)
	if u.Bound || u.Role != "viewer" {
		t.Fatalf("new user %+v", u)
	}
	e.expect(e.panel("POST", "/users", map[string]any{"ip": "fd00::1"}), 400)
	e.expect(e.panel("POST", "/users", map[string]any{"ip": "100.64.0.8", "role": "root"}), 400)
	e.expect(e.panel("POST", "/users", map[string]any{"ip": "100.64.0.7"}), 409)
	e.expect(e.panel("PATCH", "/users/100.64.0.7", map[string]any{"role": "admin"}), 200)
	var users api.List[api.PanelUser]
	e.expect(e.panel("GET", "/users", nil), 200).decode(t, &users)
	if len(users.Items) != 2 {
		t.Fatalf("users %+v", users)
	}
	// The last admin cannot remove or demote themselves.
	e.expect(e.panel("DELETE", "/users/100.64.0.7", nil), 204)
	e.expect(e.panel("PATCH", "/users/127.0.0.1", map[string]any{"role": "viewer"}), 409)
	e.expect(e.panel("DELETE", "/users/127.0.0.1", nil), 409)

	// The admin API manages panel users too.
	var list api.List[api.PanelUser]
	e.expect(e.do("GET", "/admin/v1/panel-users", e.admin, nil), 200).decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].IP != "127.0.0.1" {
		t.Fatalf("admin API list %+v", list)
	}
	// Panel users have no access to the admin API, and vice versa.
	e.expect(e.do("GET", "/admin/v1/panel-users", "", nil, "X-Platen-Panel", "1"), 401)
}

func TestPanelDisabled(t *testing.T) {
	e := newEnv(t)
	e.peers.set("peer-a")
	e.expect(e.do("GET", "/", "", nil), 404)
	e.expect(e.panel("GET", "/me", nil), 404)
}
