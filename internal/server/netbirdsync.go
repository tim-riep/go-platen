package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/netbirdapi"
	"github.com/tim-riep/go-platen/internal/store"
)

// NetBird user sync: with a NetBird API token, the server learns which
// NetBird user each device (peer) belongs to. A device then gets the
// highest panel role that any device of the same NetBird user has, so a
// user's phone has the same rights as their laptop.

const (
	settingNetBirdToken = "netbird.api_token"
	settingNetBirdURL   = "netbird.api_url"
	// nbStaleAfter is how long synced data is used after syncs fail, e.g.
	// because the token was revoked.
	nbStaleAfter = 10 * time.Minute
)

type nbDevice struct {
	netbirdapi.Peer
	User netbirdapi.User
}

// nbDirectory holds the result of the last successful sync.
type nbDirectory struct {
	mu       sync.RWMutex
	byIP     map[string]nbDevice
	users    int
	lastSync time.Time
	lastErr  string
}

func (d *nbDirectory) device(ip string) (nbDevice, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	dev, ok := d.byIP[ip]
	return dev, ok
}

// userByEmail finds a NetBird user that owns a device by e-mail address.
func (d *nbDirectory) userByEmail(email string) (netbirdapi.User, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, dev := range d.byIP {
		if dev.User.Email != "" && strings.EqualFold(dev.User.Email, email) {
			return dev.User, true
		}
	}
	return netbirdapi.User{}, false
}

// belongsTo reports whether the panel user is a device or the e-mail
// address of the NetBird user.
func (d *nbDirectory) belongsTo(row *store.PanelUser, u netbirdapi.User) bool {
	if row.IsEmail() {
		return u.Email != "" && strings.EqualFold(row.IP, u.Email)
	}
	dev, ok := d.device(row.IP)
	return ok && dev.UserID == u.ID
}

func (d *nbDirectory) clear() {
	d.mu.Lock()
	d.byIP, d.users, d.lastSync, d.lastErr = nil, 0, time.Time{}, ""
	d.mu.Unlock()
}

func nbUserLabel(u netbirdapi.User) string {
	switch {
	case u.Name != "" && u.Email != "" && u.Name != u.Email:
		return u.Name + " <" + u.Email + ">"
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return u.ID
}

// netbirdClient returns the API client for the stored token, or nil if
// none is stored.
func (s *Server) netbirdClient(ctx context.Context) (*netbirdapi.Client, error) {
	token, err := s.store.GetSetting(ctx, settingNetBirdToken)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	base, err := s.store.GetSetting(ctx, settingNetBirdURL)
	if errors.Is(err, store.ErrNotFound) || base == "" {
		base, err = s.opts.NetBirdAPIURL, nil
	}
	if err != nil {
		return nil, err
	}
	return &netbirdapi.Client{BaseURL: base, Token: token, HTTP: s.opts.NetBirdHTTP}, nil
}

// fetchDirectory reads peers and users from the NetBird API.
func fetchDirectory(ctx context.Context, c *netbirdapi.Client) (map[string]nbDevice, int, error) {
	peers, err := c.Peers(ctx)
	if err != nil {
		return nil, 0, err
	}
	users, err := c.Users(ctx)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[string]netbirdapi.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	byIP := make(map[string]nbDevice, len(peers))
	for _, p := range peers {
		if p.IP == "" || p.UserID == "" {
			// Peers registered with a setup key belong to no user.
			continue
		}
		u, ok := byID[p.UserID]
		if !ok {
			u = netbirdapi.User{ID: p.UserID}
		}
		byIP[p.IP] = nbDevice{Peer: p, User: u}
	}
	return byIP, len(users), nil
}

// SyncNetBird refreshes the NetBird user data if a token is stored.
func (s *Server) SyncNetBird(ctx context.Context) error {
	c, err := s.netbirdClient(ctx)
	if err != nil {
		return err
	}
	if c == nil {
		s.nb.clear()
		return nil
	}
	byIP, users, err := fetchDirectory(ctx, c)
	now := s.opts.Now()
	s.nb.mu.Lock()
	defer s.nb.mu.Unlock()
	if err != nil {
		s.nb.lastErr = err.Error()
		if !s.nb.lastSync.IsZero() && now.Sub(s.nb.lastSync) > nbStaleAfter {
			s.nb.byIP, s.nb.users = nil, 0
		}
		return err
	}
	s.nb.byIP, s.nb.users, s.nb.lastSync, s.nb.lastErr = byIP, users, now, ""
	return nil
}

func (s *Server) netbirdLoop(ctx context.Context) {
	t := time.NewTicker(s.opts.NetBirdSyncInterval)
	defer t.Stop()
	failing := false
	for {
		err := s.SyncNetBird(ctx)
		switch {
		case err != nil && !failing:
			s.log.Warn("netbird user sync failed", "err", err)
			failing = true
		case err == nil && failing:
			s.log.Info("netbird user sync works again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func roleRank(r store.PanelRole) int {
	switch r {
	case store.PanelAdmin:
		return 2
	case store.PanelViewer:
		return 1
	}
	return 0
}

// inheritedRole returns the highest panel role among the devices of the
// NetBird user that owns ip, and that user's label. The NetBird name of
// the device in the API must match the connecting peer, so that synced
// data about a reused IP never grants access.
func (s *Server) inheritedRole(ctx context.Context, ip string, peer Peer) (store.PanelRole, string, error) {
	dev, ok := s.nb.device(ip)
	if !ok || dev.User.IsBlocked {
		return "", "", nil
	}
	if !strings.EqualFold(strings.TrimSuffix(dev.DNSLabel, "."), strings.TrimSuffix(peer.FQDN, ".")) {
		return "", "", nil
	}
	role, err := s.nbUserRole(ctx, dev.User)
	return role, nbUserLabel(dev.User), err
}

// inheritedRoleByEmail returns the highest panel role of the NetBird user
// with the e-mail address, for users who come through a NetBird proxy.
func (s *Server) inheritedRoleByEmail(ctx context.Context, email string) (store.PanelRole, string, error) {
	u, ok := s.nb.userByEmail(email)
	if !ok || u.IsBlocked {
		return "", "", nil
	}
	role, err := s.nbUserRole(ctx, u)
	return role, nbUserLabel(u), err
}

// nbUserRole returns the highest role of the panel users that belong to
// the NetBird user: the user's devices and the user's e-mail address.
func (s *Server) nbUserRole(ctx context.Context, u netbirdapi.User) (store.PanelRole, error) {
	rows, err := s.store.ListPanelUsers(ctx)
	if err != nil {
		return "", err
	}
	var role store.PanelRole
	for _, row := range rows {
		if s.nb.belongsTo(&row, u) && roleRank(row.Role) > roleRank(role) {
			role = row.Role
		}
	}
	return role, nil
}

// ---- API ----

func (s *Server) netbirdStatus(ctx context.Context) (*api.NetBirdSync, error) {
	out := &api.NetBirdSync{APIURL: s.opts.NetBirdAPIURL, Devices: []api.NetBirdDevice{}}
	if base, err := s.store.GetSetting(ctx, settingNetBirdURL); err == nil && base != "" {
		out.APIURL = base
	}
	token, err := s.store.GetSetting(ctx, settingNetBirdToken)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if token != "" {
		out.Configured = true
		if len(token) > 4 {
			out.TokenHint = "…" + token[len(token)-4:]
		}
	}
	rows, err := s.store.ListPanelUsers(ctx)
	if err != nil {
		return nil, err
	}
	explicit := map[string]store.PanelRole{}
	for _, r := range rows {
		explicit[r.IP] = r.Role
	}
	s.nb.mu.RLock()
	defer s.nb.mu.RUnlock()
	if !s.nb.lastSync.IsZero() {
		t := s.nb.lastSync
		out.LastSync = &t
	}
	out.Error, out.Users = s.nb.lastErr, s.nb.users
	// The highest role per NetBird user.
	userRole := map[string]store.PanelRole{}
	emailUser := map[string]string{}
	for _, dev := range s.nb.byIP {
		if dev.User.Email != "" {
			emailUser[strings.ToLower(dev.User.Email)] = dev.UserID
		}
	}
	for key, role := range explicit {
		uid := emailUser[key]
		if dev, ok := s.nb.byIP[key]; ok {
			uid = dev.UserID
		}
		if uid != "" && roleRank(role) > roleRank(userRole[uid]) {
			userRole[uid] = role
		}
	}
	for ip, dev := range s.nb.byIP {
		d := api.NetBirdDevice{IP: ip, Name: dev.Name, DNSLabel: dev.DNSLabel, OS: dev.OS, Connected: dev.Connected, User: nbUserLabel(dev.User)}
		role, source := explicit[ip], "user"
		if r := userRole[dev.UserID]; roleRank(r) > roleRank(role) && !dev.User.IsBlocked {
			role, source = r, "netbird"
		}
		if role != "" {
			d.Role, d.Source = string(role), source
		}
		out.Devices = append(out.Devices, d)
	}
	sortDevices(out.Devices)
	return out, nil
}

func sortDevices(d []api.NetBirdDevice) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && (d[j].User < d[j-1].User || (d[j].User == d[j-1].User && d[j].Name < d[j-1].Name)); j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

func (s *Server) getNetBirdSync(w http.ResponseWriter, r *http.Request) {
	st, err := s.netbirdStatus(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// putNetBirdSync stores the API access after checking it against the API.
func (s *Server) putNetBirdSync(w http.ResponseWriter, r *http.Request) {
	var req api.NetBirdSyncRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	token := strings.TrimSpace(req.Token)
	if token == "" {
		stored, err := s.store.GetSetting(ctx, settingNetBirdToken)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, errBadRequest("a NetBird API token is required"))
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		token = stored
	}
	base := strings.TrimSuffix(strings.TrimSpace(req.APIURL), "/")
	if base == "" {
		base = s.opts.NetBirdAPIURL
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		writeError(w, errBadRequest("the API URL must start with https://"))
		return
	}
	c := &netbirdapi.Client{BaseURL: base, Token: token, HTTP: s.opts.NetBirdHTTP}
	if _, _, err := fetchDirectory(ctx, c); err != nil {
		writeError(w, errBadRequest("the NetBird API rejected the request: %v", err))
		return
	}
	now := s.opts.Now()
	if err := s.store.SetSetting(ctx, settingNetBirdToken, token, now); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.SetSetting(ctx, settingNetBirdURL, base, now); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("netbird user sync configured", "api", base, "by", panelActor(r))
	_ = s.SyncNetBird(ctx)
	s.getNetBirdSync(w, r)
}

func (s *Server) deleteNetBirdSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.store.DeleteSetting(ctx, settingNetBirdToken); err != nil {
		s.fail(w, r, err)
		return
	}
	s.nb.clear()
	s.log.Info("netbird user sync removed", "by", panelActor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncNetBirdNow(w http.ResponseWriter, r *http.Request) {
	_ = s.SyncNetBird(r.Context())
	s.getNetBirdSync(w, r)
}
