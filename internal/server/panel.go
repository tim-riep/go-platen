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
	"github.com/tim-riep/go-platen/internal/auth"
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
//
// A NetBird reverse proxy publishes the panel under its own name and
// names the user by e-mail address in X-NetBird-User. Since any peer could
// send that header, it is trusted only together with a proxy key that an
// admin generated in the panel and set up as a header in the proxy.

//go:embed panel
var panelFiles embed.FS

// panelHeader must be present on every panel API request.
const panelHeader = "X-Platen-Panel"

// A NetBird reverse proxy that is set up to send a proxy key names the
// user it forwards a request for by e-mail address.
const (
	proxyKeyHeader  = "X-Platen-Proxy-Key"
	proxyUserHeader = "X-NetBird-User"
)

// lastSeenInterval limits how often the last visit of a user is stored.
const lastSeenInterval = time.Minute

type panelCtxKey struct{}

type panelSession struct {
	user       *store.PanelUser
	firstAdmin bool
	// via names the NetBird user whose other device grants the role.
	via string
	// proxy names the proxy key of a request from a NetBird proxy.
	proxy string
}

func panelSessionFrom(ctx context.Context) *panelSession {
	s, _ := ctx.Value(panelCtxKey{}).(*panelSession)
	return s
}

// panelUserView adds the NetBird user of the device, if known.
func (s *Server) panelUserView(u *store.PanelUser) api.PanelUser {
	out := apiPanelUser(u)
	if u.IsEmail() {
		if nu, ok := s.nb.userByEmail(u.IP); ok {
			out.NetBirdUser = nbUserLabel(nu)
		}
	} else if dev, ok := s.nb.device(u.IP); ok {
		out.NetBirdUser = nbUserLabel(dev.User)
	}
	return out
}

func apiPanelUser(u *store.PanelUser) api.PanelUser {
	out := api.PanelUser{
		IP: u.IP, Name: u.Name, Role: string(u.Role), PeerID: u.PeerID, PeerFQDN: u.PeerFQDN,
		Bound: u.PeerID != "", CreatedAt: u.CreatedAt, LastSeen: u.LastSeen,
	}
	if u.IsEmail() {
		out.IP, out.Email = "", u.IP
	}
	return out
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
		proxy, err := s.panelProxy(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if r.Header.Get(panelHeader) == "" {
			writeError(w, errForbidden("missing %s header", panelHeader))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			u, err := url.Parse(origin)
			if err != nil || !s.originAllowed(u.Host, r.Host, proxy != nil) {
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
		var sess *panelSession
		if proxy != nil {
			sess, err = s.panelProxyUser(r, proxy, peer)
		} else {
			sess, err = s.panelPeerUser(r, ip, peer)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if admin && sess.user.Role != store.PanelAdmin {
			writeError(w, errForbidden("this action needs the admin role"))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), panelCtxKey{}, sess)))
	}
}

// panelProxy checks the proxy key of a request. A request without the
// header returns nil and must pass the host check; one with an unknown key
// is refused instead of being treated as a direct request.
func (s *Server) panelProxy(r *http.Request) (*store.ProxyKey, error) {
	key := r.Header.Get(proxyKeyHeader)
	if key == "" {
		if !s.hostAllowed(r.Host) {
			return nil, errForbidden("the panel must be opened by this server's NetBird name or IP")
		}
		return nil, nil
	}
	k, err := s.store.GetProxyKeyByHash(r.Context(), auth.Hash(key))
	if errors.Is(err, store.ErrNotFound) {
		s.log.Warn("panel: unknown proxy key refused", "remote", r.RemoteAddr)
		return nil, errForbidden("unknown %s", proxyKeyHeader)
	}
	return k, err
}

// originAllowed checks the Origin of a state-changing request. Behind a
// proxy the browser uses the proxy's public name, so the origin must be
// the host the request is addressed to.
func (s *Server) originAllowed(origin, host string, proxied bool) bool {
	if proxied {
		return origin != "" && strings.EqualFold(origin, host)
	}
	return s.hostAllowed(origin)
}

// panelProxyUser identifies the user a NetBird reverse proxy forwards a
// request for by the e-mail address in X-NetBird-User. Such a user needs a
// panel user entry for the address or, with the NetBird user sync, a
// NetBird user with that address whose devices have a role. A proxy never
// claims the first admin.
func (s *Server) panelProxyUser(r *http.Request, k *store.ProxyKey, peer Peer) (*panelSession, error) {
	ctx := r.Context()
	now := s.opts.Now()
	if k.LastUsed == nil || now.Sub(*k.LastUsed) >= lastSeenInterval || k.LastPeer != peer.FQDN {
		_ = s.store.TouchProxyKey(ctx, k.ID, now, peer.FQDN)
	}
	email, err := parsePanelEmail(r.Header.Get(proxyUserHeader))
	if err != nil {
		return nil, errForbidden("the proxy %q did not send a valid e-mail address in %s", k.Name, proxyUserHeader)
	}
	sess := &panelSession{proxy: k.Name}
	inherited, via, err := s.inheritedRoleByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	u, err := s.store.GetPanelUser(ctx, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if inherited == "" {
			s.log.Info("panel: access by unknown proxy user refused", "email", email, "proxy", k.Name, "peer", peer.FQDN)
			return nil, errForbidden("%s is not a panel user; ask a panel admin to add this e-mail address", email)
		}
		sess.user, sess.via = &store.PanelUser{IP: email, Role: inherited, CreatedAt: now, UpdatedAt: now}, via
		return sess, nil
	case err != nil:
		return nil, err
	}
	if roleRank(inherited) > roleRank(u.Role) {
		elevated := *u
		elevated.Role, sess.via = inherited, via
		u = &elevated
	}
	if u.LastSeen == nil || now.Sub(*u.LastSeen) >= lastSeenInterval {
		if err := s.store.TouchPanelUser(ctx, email, now); err == nil {
			u.LastSeen = &now
		}
	}
	sess.user = u
	return sess, nil
}

// panelPeerUser identifies the user of a direct request by NetBird IP and
// peer.
func (s *Server) panelPeerUser(r *http.Request, ip string, peer Peer) (*panelSession, error) {
	ctx := r.Context()
	now := s.opts.Now()
	sess := &panelSession{}
	// The role inherited from other devices of the same NetBird user.
	inherited, via, err := s.inheritedRole(ctx, ip, peer)
	if err != nil {
		return nil, err
	}
	viaNetBird := func() *store.PanelUser {
		sess.via = via
		return &store.PanelUser{IP: ip, PeerID: peer.ID, PeerFQDN: peer.FQDN, Role: inherited, CreatedAt: now, UpdatedAt: now}
	}
	explicit := true
	u, err := s.store.GetPanelUser(ctx, ip)
	switch {
	case errors.Is(err, store.ErrNotFound):
		first := &store.PanelUser{IP: ip, PeerID: peer.ID, PeerFQDN: peer.FQDN, Role: store.PanelAdmin, CreatedAt: now, UpdatedAt: now, LastSeen: &now}
		if err := s.store.ClaimFirstPanelAdmin(ctx, first); err == nil {
			s.log.Warn("panel: first admin claimed", "ip", ip, "peer", peer.ID, "fqdn", peer.FQDN)
			u, sess.firstAdmin = first, true
			break
		} else if !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
		if inherited != "" {
			u, explicit = viaNetBird(), false
			break
		}
		s.log.Info("panel: access by unknown peer refused", "ip", ip, "peer", peer.ID, "fqdn", peer.FQDN)
		return nil, errForbidden("%s is not a panel user; ask a panel admin to add this NetBird IP", ip)
	case err != nil:
		return nil, err
	case u.PeerID == "":
		u.PeerID, u.PeerFQDN, u.UpdatedAt = peer.ID, peer.FQDN, now
		if err := s.store.UpdatePanelUser(ctx, u); err != nil {
			return nil, err
		}
		s.log.Info("panel: user bound to peer", "ip", ip, "peer", peer.ID, "fqdn", peer.FQDN)
	case u.PeerID != peer.ID:
		if inherited != "" {
			u, explicit = viaNetBird(), false
			break
		}
		s.log.Warn("panel: access from a different peer refused", "ip", ip, "bound", u.PeerID, "peer", peer.ID)
		return nil, errForbidden("%s belongs to a different NetBird peer than when access was granted; ask a panel admin to reset it", ip)
	}
	if explicit && roleRank(inherited) > roleRank(u.Role) {
		// Another device of the same NetBird user has a higher role.
		elevated := *u
		elevated.Role, sess.via = inherited, via
		u = &elevated
	}
	if explicit && (u.LastSeen == nil || now.Sub(*u.LastSeen) >= lastSeenInterval) {
		if err := s.store.TouchPanelUser(ctx, ip, now); err == nil {
			u.LastSeen = &now
		}
	}
	sess.user = u
	return sess, nil
}

func (s *Server) panelRoutes(mux *http.ServeMux) {
	static, err := fs.Sub(panelFiles, "panel")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(static)
	serveStatic := func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.panelProxy(r); err != nil {
			s.fail(w, r, err)
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
	edit("POST /panel/v1/jobs/{id}/retry", s.adminRetryJob)
	edit("DELETE /panel/v1/jobs/{id}", s.adminDeleteJob)

	// Panel users. Viewers do not see the user list.
	edit("GET /panel/v1/users", s.listPanelUsers)
	edit("POST /panel/v1/users", s.createPanelUser)
	edit("PATCH /panel/v1/users/{ip}", s.updatePanelUser)
	edit("DELETE /panel/v1/users/{ip}", s.deletePanelUser)
	edit("POST /panel/v1/users/{ip}/reset-peer", s.resetPanelUserPeer)

	// Keys of NetBird reverse proxies.
	edit("GET /panel/v1/proxy-keys", s.listProxyKeys)
	edit("POST /panel/v1/proxy-keys", s.createProxyKey)
	edit("DELETE /panel/v1/proxy-keys/{id}", s.deleteProxyKey)

	// NetBird user sync.
	edit("GET /panel/v1/netbird", s.getNetBirdSync)
	edit("PUT /panel/v1/netbird", s.putNetBirdSync)
	edit("DELETE /panel/v1/netbird", s.deleteNetBirdSync)
	edit("POST /panel/v1/netbird/sync", s.syncNetBirdNow)
}

func (s *Server) panelMe(w http.ResponseWriter, r *http.Request) {
	sess := panelSessionFrom(r.Context())
	writeJSON(w, http.StatusOK, api.PanelMe{PanelUser: s.panelUserView(sess.user), FirstAdmin: sess.firstAdmin, Version: s.opts.Version, Via: sess.via, Proxy: sess.proxy})
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

// parsePanelEmail checks the e-mail address of a panel user and returns it
// in lower case.
func parsePanelEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || len(s) > 254 ||
		strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", errBadRequest("%q is not an e-mail address", s)
	}
	return s, nil
}

// parsePanelUserID returns the key of a new panel user: a NetBird IPv4
// address or, for users who come through a NetBird proxy, an e-mail
// address.
func parsePanelUserID(req *api.PanelUserRequest) (string, error) {
	switch {
	case req.IP != "" && req.Email != "":
		return "", errBadRequest("give either ip or email, not both")
	case req.Email != "":
		return parsePanelEmail(req.Email)
	case req.IP == "":
		return "", errBadRequest("ip or email is required")
	}
	return parsePanelIP(req.IP)
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
		out.Items = append(out.Items, s.panelUserView(&users[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPanelUser(w http.ResponseWriter, r *http.Request) {
	var req api.PanelUserRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ip, err := parsePanelUserID(&req)
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
	writeJSON(w, http.StatusCreated, s.panelUserView(u))
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
	if (req.IP != "" && req.IP != u.IP) || (req.Email != "" && !strings.EqualFold(req.Email, u.IP)) {
		writeError(w, errBadRequest("the IP or e-mail address of a panel user cannot be changed"))
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
	if u.IsEmail() {
		writeError(w, errBadRequest("users identified by e-mail address have no peer binding"))
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
		if sess.proxy != "" {
			return "panel user " + sess.user.IP + " via proxy " + sess.proxy
		}
		return "panel user " + sess.user.IP
	}
	return "admin API"
}

// ---- proxy keys (shared by the panel and the admin API) ----

func apiProxyKey(k *store.ProxyKey) api.ProxyKey {
	return api.ProxyKey{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt, LastUsed: k.LastUsed, LastPeer: k.LastPeer}
}

func (s *Server) listProxyKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListProxyKeys(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.List[api.ProxyKey]{Items: []api.ProxyKey{}}
	for i := range keys {
		out.Items = append(out.Items, apiProxyKey(&keys[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createProxyKey(w http.ResponseWriter, r *http.Request) {
	var req api.ProxyKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, errBadRequest("name is required"))
		return
	}
	key := auth.NewToken(auth.PrefixProxy)
	k := &store.ProxyKey{ID: auth.NewID(), Name: name, Hash: auth.Hash(key), CreatedAt: s.opts.Now()}
	if err := s.store.CreateProxyKey(r.Context(), k); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("panel: proxy key created", "id", k.ID, "name", name, "by", panelActor(r))
	writeJSON(w, http.StatusCreated, api.ProxyKeyCreated{ProxyKey: apiProxyKey(k), Header: proxyKeyHeader, Key: key})
}

func (s *Server) deleteProxyKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteProxyKey(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("panel: proxy key deleted", "id", id, "by", panelActor(r))
	w.WriteHeader(http.StatusNoContent)
}
