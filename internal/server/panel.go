package server

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/store"
)

// The web panel is served on the server's port in the mesh. Its users are
// identified by their NetBird IPv4 address and bound to the NetBird peer
// (WireGuard public key) behind it; the first peer that opens the panel
// becomes its first admin.
//
// Because access depends only on where a request comes from, the panel
// API is protected against requests a foreign web page makes through the
// user's browser: the Host header must name this server (against DNS
// rebinding), every API request must carry the X-Platen-Panel header,
// which browsers do not send cross-origin without a CORS preflight that
// the panel never grants, and a state-changing request with an Origin
// header must come from this server.

//go:embed panel
var panelFiles embed.FS

// panelHeader must be present on every panel API request.
const panelHeader = "X-Platen-Panel"

// lastSeenInterval limits how often the last visit of a user is stored.
const lastSeenInterval = time.Minute

type panelCtxKey struct{}

type panelSession struct {
	user       *store.PanelUser
	firstAdmin bool
}

func panelSessionFrom(ctx context.Context) *panelSession {
	s, _ := ctx.Value(panelCtxKey{}).(*panelSession)
	return s
}

func apiPanelUser(u *store.PanelUser) api.PanelUser {
	return api.PanelUser{
		IP: u.IP, Name: u.Name, Role: string(u.Role), PeerID: u.PeerID, PeerFQDN: u.PeerFQDN,
		Bound: u.PeerID != "", CreatedAt: u.CreatedAt, LastSeen: u.LastSeen,
	}
}

// hostAllowed reports whether the request is addressed to this server.
func (s *Server) hostAllowed(host string) bool {
	if s.opts.PanelHosts == nil {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, allowed := range s.opts.PanelHosts() {
		if host == strings.TrimSuffix(strings.ToLower(allowed), ".") {
			return true
		}
	}
	return false
}

func remoteIPv4(remoteAddr string) (string, bool) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", false
	}
	ip := ap.Addr().Unmap()
	if !ip.Is4() {
		return "", false
	}
	return ip.String(), true
}

// panelAuth identifies the panel user and checks the role. Viewers may use
// safe methods, everything else needs an admin.
func (s *Server) panelAuth(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeError(w, errForbidden("the panel must be opened by this server's NetBird name or IP"))
			return
		}
		if r.Header.Get(panelHeader) == "" {
			writeError(w, errForbidden("missing %s header", panelHeader))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			u, err := url.Parse(origin)
			if err != nil || !s.hostAllowed(u.Host) {
				writeError(w, errForbidden("request from a foreign origin"))
				return
			}
		}
		ip, ok := remoteIPv4(r.RemoteAddr)
		peer, known := s.opts.Peers.PeerForAddr(r.RemoteAddr)
		if !ok || !known || peer.ID == "" {
			writeError(w, errForbidden("the panel is only available to NetBird peers over IPv4"))
			return
		}
		ctx := r.Context()
		now := s.opts.Now()
		sess := &panelSession{}
		u, err := s.store.GetPanelUser(ctx, ip)
		switch {
		case errors.Is(err, store.ErrNotFound):
			first := &store.PanelUser{IP: ip, PeerID: peer.ID, PeerFQDN: peer.FQDN, Role: store.PanelAdmin, CreatedAt: now, UpdatedAt: now, LastSeen: &now}
			if err := s.store.ClaimFirstPanelAdmin(ctx, first); err == nil {
				s.log.Warn("panel: first admin claimed", "ip", ip, "peer", peer.ID, "fqdn", peer.FQDN)
				u, sess.firstAdmin = first, true
				break
			} else if !errors.Is(err, store.ErrConflict) {
				s.fail(w, r, err)
				return
			}
			s.log.Info("panel: access by unknown peer refused", "ip", ip, "peer", peer.ID, "fqdn", peer.FQDN)
			writeError(w, errForbidden("%s is not a panel user; ask a panel admin to add this NetBird IP", ip))
			return
		case err != nil:
			s.fail(w, r, err)
			return
		case u.PeerID == "":
			u.PeerID, u.PeerFQDN, u.UpdatedAt = peer.ID, peer.FQDN, now
			if err := s.store.UpdatePanelUser(ctx, u); err != nil {
				s.fail(w, r, err)
				return
			}
			s.log.Info("panel: user bound to peer", "ip", ip, "peer", peer.ID, "fqdn", peer.FQDN)
		case u.PeerID != peer.ID:
			s.log.Warn("panel: access from a different peer refused", "ip", ip, "bound", u.PeerID, "peer", peer.ID)
			writeError(w, errForbidden("%s belongs to a different NetBird peer than when access was granted; ask a panel admin to reset it", ip))
			return
		}
		if admin && u.Role != store.PanelAdmin {
			writeError(w, errForbidden("this action needs the admin role"))
			return
		}
		if u.LastSeen == nil || now.Sub(*u.LastSeen) >= lastSeenInterval {
			if err := s.store.TouchPanelUser(ctx, ip, now); err == nil {
				u.LastSeen = &now
			}
		}
		sess.user = u
		h(w, r.WithContext(context.WithValue(ctx, panelCtxKey{}, sess)))
	}
}

func (s *Server) panelRoutes(mux *http.ServeMux) {
	static, err := fs.Sub(panelFiles, "panel")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(static)
	serveStatic := func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeError(w, errForbidden("the panel must be opened by this server's NetBird name or IP"))
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}
	mux.HandleFunc("GET /{$}", serveStatic)
	mux.HandleFunc("GET /panel/", func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/panel")
		serveStatic(w, r2)
	})

	view := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.panelAuth(false, h)) }
	edit := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.panelAuth(true, h)) }

	view("GET /panel/v1/me", s.panelMe)
	view("GET /panel/v1/backends", s.panelBackends)

	// The same handlers as the admin API.
	view("GET /panel/v1/workers", s.adminListWorkers)
	edit("POST /panel/v1/workers", s.adminCreateWorker)
	view("GET /panel/v1/workers/{id}", s.adminGetWorker)
	edit("PATCH /panel/v1/workers/{id}", s.adminUpdateWorker)
	edit("DELETE /panel/v1/workers/{id}", s.adminDeleteWorker)
	edit("POST /panel/v1/workers/{id}/enrollment-tokens", s.adminCreateEnrollmentToken)
	view("GET /panel/v1/printers", s.adminListPrinters)
	edit("POST /panel/v1/printers", s.adminCreatePrinter)
	view("GET /panel/v1/printers/{id}", s.adminGetPrinter)
	edit("PATCH /panel/v1/printers/{id}", s.adminUpdatePrinter)
	edit("DELETE /panel/v1/printers/{id}", s.adminDeletePrinter)
	view("GET /panel/v1/app-clients", s.adminListAppClients)
	edit("POST /panel/v1/app-clients", s.adminCreateAppClient)
	view("GET /panel/v1/app-clients/{id}", s.adminGetAppClient)
	edit("PATCH /panel/v1/app-clients/{id}", s.adminUpdateAppClient)
	edit("DELETE /panel/v1/app-clients/{id}", s.adminDeleteAppClient)
	edit("POST /panel/v1/app-clients/{id}/rotate-token", s.adminRotateAppClientToken)
	edit("POST /panel/v1/printers/{id}/jobs", s.adminSubmitJob)
	view("GET /panel/v1/jobs", s.adminListJobs)
	view("GET /panel/v1/jobs/{id}", s.adminGetJob)
	edit("POST /panel/v1/jobs/{id}/cancel", s.adminCancelJob)

	// Panel users. Viewers do not see the user list.
	edit("GET /panel/v1/users", s.listPanelUsers)
	edit("POST /panel/v1/users", s.createPanelUser)
	edit("PATCH /panel/v1/users/{ip}", s.updatePanelUser)
	edit("DELETE /panel/v1/users/{ip}", s.deletePanelUser)
	edit("POST /panel/v1/users/{ip}/reset-peer", s.resetPanelUserPeer)
}

func (s *Server) panelMe(w http.ResponseWriter, r *http.Request) {
	sess := panelSessionFrom(r.Context())
	writeJSON(w, http.StatusOK, api.PanelMe{PanelUser: apiPanelUser(sess.user), FirstAdmin: sess.firstAdmin, Version: s.opts.Version})
}

func (s *Server) panelBackends(w http.ResponseWriter, r *http.Request) {
	out := api.List[api.Backend]{Items: []api.Backend{}}
	for _, name := range s.opts.BackendNames {
		b := api.Backend{Name: name, ContentTypes: []string{}}
		if s.opts.Backends != nil {
			if ct, ok := s.opts.Backends(name); ok {
				b.ContentTypes = ct
			}
		}
		out.Items = append(out.Items, b)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- panel users (shared by the panel and the admin API) ----

func parsePanelIP(s string) (string, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !ip.Unmap().Is4() {
		return "", errBadRequest("%q is not an IPv4 address", s)
	}
	return ip.Unmap().String(), nil
}

func parsePanelRole(s string) (store.PanelRole, error) {
	switch r := store.PanelRole(strings.ToLower(s)); r {
	case store.PanelAdmin, store.PanelViewer:
		return r, nil
	}
	return "", errBadRequest("role must be %q or %q", store.PanelAdmin, store.PanelViewer)
}

func (s *Server) listPanelUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListPanelUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.List[api.PanelUser]{Items: []api.PanelUser{}}
	for i := range users {
		out.Items = append(out.Items, apiPanelUser(&users[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPanelUser(w http.ResponseWriter, r *http.Request) {
	var req api.PanelUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ip, err := parsePanelIP(req.IP)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	role := store.PanelViewer
	if req.Role != nil {
		if role, err = parsePanelRole(*req.Role); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	now := s.opts.Now()
	u := &store.PanelUser{IP: ip, Role: role, CreatedAt: now, UpdatedAt: now}
	if req.Name != nil {
		u.Name = strings.TrimSpace(*req.Name)
	}
	if err := s.store.CreatePanelUser(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("panel: user added", "ip", ip, "role", role, "by", panelActor(r))
	writeJSON(w, http.StatusCreated, apiPanelUser(u))
}

func (s *Server) updatePanelUser(w http.ResponseWriter, r *http.Request) {
	var req api.PanelUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.store.GetPanelUser(r.Context(), r.PathValue("ip"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.IP != "" && req.IP != u.IP {
		writeError(w, errBadRequest("the IP of a panel user cannot be changed"))
		return
	}
	if req.Name != nil {
		u.Name = strings.TrimSpace(*req.Name)
	}
	if req.Role != nil {
		if u.Role, err = parsePanelRole(*req.Role); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	u.UpdatedAt = s.opts.Now()
	if err := s.store.UpdatePanelUser(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("panel: user updated", "ip", u.IP, "role", u.Role, "by", panelActor(r))
	writeJSON(w, http.StatusOK, apiPanelUser(u))
}

func (s *Server) deletePanelUser(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if err := s.store.DeletePanelUser(r.Context(), ip); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("panel: user removed", "ip", ip, "by", panelActor(r))
	w.WriteHeader(http.StatusNoContent)
}

// resetPanelUserPeer clears the peer binding; the next visit from the IP
// binds the peer again.
func (s *Server) resetPanelUserPeer(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetPanelUser(r.Context(), r.PathValue("ip"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u.PeerID, u.PeerFQDN, u.UpdatedAt = "", "", s.opts.Now()
	if err := s.store.UpdatePanelUser(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("panel: peer binding reset", "ip", u.IP, "by", panelActor(r))
	writeJSON(w, http.StatusOK, apiPanelUser(u))
}

// panelActor names who made a change, for the log.
func panelActor(r *http.Request) string {
	if sess := panelSessionFrom(r.Context()); sess != nil {
		return "panel user " + sess.user.IP
	}
	return "admin API"
}
