package server

import (
	"context"
	"testing"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/store"
)

func (e *env) client(method, path string, body any, header ...string) resp {
	e.t.Helper()
	return e.do(method, "/client/v1"+path, "", body, append([]string{"X-Platen-Client", "1"}, header...)...)
}

func TestClientPrintAccess(t *testing.T) {
	e, nb, url := newSyncEnv(t)
	e.expect(e.do("POST", "/admin/v1/workers", e.admin, map[string]any{"id": "w1"}), 201)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "p1", "worker_id": "w1", "address": "10.0.0.1", "backend": "pdf"}), 201)

	// Neither a panel user nor (without the sync) a NetBird user's device.
	e.expect(e.client("GET", "/printers", nil), 403)

	// With the sync, the device of a NetBird user may print on all printers.
	e.expect(e.do("PUT", "/admin/v1/netbird", e.admin, map[string]string{"api_url": url, "token": "nbp_secret"}), 200)
	var me api.ClientMe
	e.expect(e.client("GET", "/me", nil), 200).decode(t, &me)
	if me.IP != "127.0.0.1" || me.NetBirdUser != "Tim <tim@example.com>" || me.PanelRole != "admin" {
		t.Fatalf("me %+v", me)
	}
	var printers api.PrinterList
	e.expect(e.client("GET", "/printers", nil), 200).decode(t, &printers)
	if len(printers.Printers) != 1 || printers.Printers[0].ContentTypes[0] != "application/pdf" {
		t.Fatalf("printers %+v", printers)
	}
	var job api.Job
	e.expect(e.client("POST", "/printers/p1/jobs?title=Report", []byte("%PDF-1.4"), "Content-Type", "application/pdf"), 202).decode(t, &job)
	e.expect(e.client("GET", "/jobs/"+job.ID, nil), 200)
	var aj api.AdminJob
	e.expect(e.do("GET", "/admin/v1/jobs/"+job.ID, e.admin, nil), 200).decode(t, &aj)
	if aj.AppClientID != "device:127.0.0.1" || aj.Title != "Report" {
		t.Fatalf("job %+v", aj)
	}

	// A NetBird user without any panel role still may print.
	if err := e.store.DeletePanelUser(context.Background(), "100.64.0.1"); err == nil {
		t.Fatal("deleted the last admin")
	}
	nb.mu.Lock()
	nb.users = append(nb.users, nbUser("u2", "Anna"))
	nb.peers[1].UserID = "u2"
	nb.mu.Unlock()
	e.expect(e.do("POST", "/admin/v1/netbird/sync", e.admin, nil), 200)
	var anna api.ClientMe
	e.expect(e.client("GET", "/me", nil), 200).decode(t, &anna)
	if anna.PanelRole != "" || anna.NetBirdUser != "Anna" {
		t.Fatalf("user without role %+v", anna)
	}
	e.expect(e.client("POST", "/printers/p1/jobs", []byte("%PDF-1.4"), "Content-Type", "application/pdf"), 202)

	// Blocked users and foreign peers may not.
	nb.mu.Lock()
	nb.users[1].IsBlocked = true
	nb.mu.Unlock()
	e.expect(e.do("POST", "/admin/v1/netbird/sync", e.admin, nil), 200)
	e.expect(e.client("GET", "/printers", nil), 403)
}

func TestClientRequestChecks(t *testing.T) {
	e, _, _ := newSyncEnv(t)
	now := time.Now()
	if err := e.store.CreatePanelUser(context.Background(), &store.PanelUser{IP: "127.0.0.1", Role: store.PanelViewer, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// A viewer may print.
	e.expect(e.client("GET", "/printers", nil), 200)
	e.expect(e.do("GET", "/client/v1/printers", "", nil), 403)
	e.expect(e.client("GET", "/printers", nil, "Host", "evil.example"), 403)
	// Jobs of other submitters are not visible.
	e.expect(e.do("POST", "/admin/v1/workers", e.admin, map[string]any{"id": "w1"}), 201)
	e.expect(e.do("POST", "/admin/v1/printers", e.admin, map[string]any{"id": "p1", "worker_id": "w1", "address": "10.0.0.1", "backend": "raw"}), 201)
	var job api.Job
	e.expect(e.do("POST", "/admin/v1/printers/p1/jobs", e.admin, []byte("x"), "Content-Type", "text/plain"), 202).decode(t, &job)
	e.expect(e.client("GET", "/jobs/"+job.ID, nil), 404)
}
