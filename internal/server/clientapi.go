package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/tim-riep/go-platen/internal/api"
	"github.com/tim-riep/go-platen/internal/store"
)

// Print API for desktop clients (e.g. the macOS app). Like the panel, it
// identifies the device by its NetBird peer. A device may print on all
// printers if it has a panel role (own or inherited through the NetBird
// user sync) or belongs to a NetBird user that is not blocked. Devices
// registered with a setup key belong to no user and need a panel role.
//
// The same protections as for the panel apply: requests must be addressed
// to this server by name or IP and carry the X-Platen-Client header, which
// a foreign web page cannot send without a CORS preflight.

const clientHeader = "X-Platen-Client"

type clientCtxKey struct{}

// clientIdentity describes the device of a desktop client.
type clientIdentity struct {
	IP        string
	FQDN      string
	User      string // NetBird user, if known
	Role      string // panel role, if any
	Submitter string
}

// printAccess decides whether the device at ip may print.
func (s *Server) printAccess(ctx context.Context, ip string, peer Peer) (*clientIdentity, error) {
	id := &clientIdentity{IP: ip, FQDN: peer.FQDN, Submitter: "device:" + ip}
	if dev, ok := s.nb.device(ip); ok && !dev.User.IsBlocked &&
		strings.EqualFold(strings.TrimSuffix(dev.DNSLabel, "."), strings.TrimSuffix(peer.FQDN, ".")) {
		id.User = nbUserLabel(dev.User)
	}
	u, err := s.store.GetPanelUser(ctx, ip)
	switch {
	case err == nil && (u.PeerID == "" || u.PeerID == peer.ID):
		id.Role = string(u.Role)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if id.Role == "" {
		inherited, _, err := s.inheritedRole(ctx, ip, peer)
		if err != nil {
			return nil, err
		}
		id.Role = string(inherited)
	}
	if id.Role == "" && id.User == "" {
		return nil, nil
	}
	return id, nil
}

func (s *Server) clientAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeError(w, errForbidden("requests must be addressed to this server's NetBird name or IP"))
			return
		}
		if r.Header.Get(clientHeader) == "" {
			writeError(w, errForbidden("missing %s header", clientHeader))
			return
		}
		ip, ok := remoteIPv4(r.RemoteAddr)
		peer, known := s.opts.Peers.PeerForAddr(r.RemoteAddr)
		if !ok || !known || peer.ID == "" {
			writeError(w, errForbidden("printing is only available to NetBird peers over IPv4"))
			return
		}
		id, err := s.printAccess(r.Context(), ip, peer)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if id == nil {
			s.log.Info("client: print access refused", "ip", ip, "peer", peer.ID, "fqdn", peer.FQDN)
			writeError(w, errForbidden("%s may not print: it is neither a panel user nor a device of a NetBird user", ip))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), clientCtxKey{}, id)))
	}
}

func (s *Server) clientRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /client/v1/me", s.clientAuth(s.clientMe))
	mux.HandleFunc("GET /client/v1/printers", s.clientAuth(s.clientPrinters))
	mux.HandleFunc("POST /client/v1/printers/{id}/jobs", s.clientAuth(s.clientSubmitJob))
	mux.HandleFunc("GET /client/v1/jobs/{id}", s.clientAuth(s.clientGetJob))
}

func (s *Server) clientMe(w http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(clientCtxKey{}).(*clientIdentity)
	writeJSON(w, http.StatusOK, api.ClientMe{IP: id.IP, FQDN: id.FQDN, NetBirdUser: id.User, PanelRole: id.Role, Version: s.opts.Version})
}

func (s *Server) clientPrinters(w http.ResponseWriter, r *http.Request) {
	printers, err := s.store.ListPrinters(r.Context(), store.PrinterFilter{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.PrinterList{Printers: []api.Printer{}}
	for _, p := range printers {
		ap := api.Printer{
			ID: p.ID, Name: p.Name, Status: string(p.Status), Reachable: p.Reachable, ReachabilityAt: p.ReachabilityAt,
			PaperSize: p.PaperSize, ResolutionDPI: p.ResolutionDPI,
		}
		if s.opts.Backends != nil {
			ap.ContentTypes, _ = s.opts.Backends(p.Backend)
		}
		out.Printers = append(out.Printers, ap)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) clientSubmitJob(w http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(clientCtxKey{}).(*clientIdentity)
	s.queueDocument(w, r, id.Submitter, r.PathValue("id"))
}

// clientGetJob returns a job the device submitted.
func (s *Server) clientGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(clientCtxKey{}).(*clientIdentity)
	j, err := s.store.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if j.AppClientID != id.Submitter {
		writeError(w, errNotFound("job %q not found", j.ID))
		return
	}
	writeJSON(w, http.StatusOK, apiJob(j))
}
