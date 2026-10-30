// Package mesh wraps the embedded NetBird client that carries all traffic
// between platen components. The server listens only on the mesh listener
// and workers reach the server only through the mesh; neither opens a port
// on a host network interface.
package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/netbirdio/netbird/client/embed"
)

// Network is the small interface the rest of platen uses. The NetBird
// implementation is the only one in production code; tests substitute a
// loopback implementation.
type Network interface {
	// Listen listens on the port of the node's mesh address.
	Listen(port int) (net.Listener, error)
	// DialContext connects to an address in the mesh.
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	// HTTPClient returns a client whose connections run over the mesh.
	HTTPClient() *http.Client
	// PeerForAddr identifies the mesh peer behind a remote address of an
	// incoming connection.
	PeerForAddr(remoteAddr string) (peerID, fqdn string, ok bool)
	// Stop disconnects from the mesh.
	Stop(ctx context.Context) error
}

// Options configures the NetBird client.
type Options struct {
	// ManagementURL is the NetBird management server (self-hosted or
	// cloud). Empty uses NetBird's default.
	ManagementURL string
	// ConfigPath persists the peer identity (WireGuard key) so the peer
	// keeps its identity and IP across restarts. Required.
	ConfigPath string
	// SetupKey registers the peer. It is only needed for the first start;
	// later starts reuse the identity stored at ConfigPath.
	SetupKey string
	// Hostname is the device name in the mesh.
	Hostname string
	// BlockInbound rejects all inbound connections from peers (workers).
	BlockInbound bool
	// LogLevel is the NetBird log level (panic, fatal, error, warn, info,
	// debug, trace). Default warn.
	LogLevel string
	Logger   *slog.Logger
	// StartTimeout bounds the initial connection. Default 2 minutes.
	StartTimeout time.Duration
}

// NetBird is the Network implementation backed by the embedded NetBird
// client in userspace mode (no root required).
type NetBird struct {
	c   *embed.Client
	log *slog.Logger
}

var _ Network = (*NetBird)(nil)

// Start creates the NetBird client and connects to the mesh.
func Start(ctx context.Context, o Options) (*NetBird, error) {
	if o.ConfigPath == "" {
		return nil, errors.New("mesh: netbird config_path is required for a persistent peer identity")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.LogLevel == "" {
		o.LogLevel = "warn"
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = 2 * time.Minute
	}
	if err := os.MkdirAll(filepath.Dir(o.ConfigPath), 0o700); err != nil {
		return nil, fmt.Errorf("mesh: create config directory: %w", err)
	}

	opts := embed.Options{
		DeviceName:          o.Hostname,
		ManagementURL:       o.ManagementURL,
		ConfigPath:          o.ConfigPath,
		StatePath:           strings.TrimSuffix(o.ConfigPath, filepath.Ext(o.ConfigPath)) + ".state.json",
		LogOutput:           newLogWriter(o.Logger.With("component", "netbird")),
		LogLevel:            o.LogLevel,
		BlockInbound:        o.BlockInbound,
		DisableClientRoutes: true,
	}
	switch {
	case o.SetupKey != "":
		opts.SetupKey = o.SetupKey
	default:
		key, err := storedPrivateKey(o.ConfigPath)
		if err != nil {
			return nil, err
		}
		opts.PrivateKey = key
	}

	c, err := embed.New(opts)
	if err != nil {
		return nil, fmt.Errorf("mesh: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, o.StartTimeout)
	defer cancel()
	if err := c.Start(sctx); err != nil {
		return nil, fmt.Errorf("mesh: connect to netbird: %w", err)
	}
	n := &NetBird{c: c, log: o.Logger.With("component", "mesh")}
	if st, err := c.Status(); err == nil {
		n.log.Info("connected to netbird", "ip", st.LocalPeerState.IP, "fqdn", st.LocalPeerState.FQDN)
	}
	return n, nil
}

// storedPrivateKey reads the peer key from an existing NetBird config file,
// so that restarts do not need the setup key again.
func storedPrivateKey(configPath string) (string, error) {
	b, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("mesh: no setup key given and no existing netbird config found; a setup key is required for the first start")
	}
	if err != nil {
		return "", fmt.Errorf("mesh: read netbird config: %w", err)
	}
	var cfg struct {
		PrivateKey string
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.PrivateKey == "" {
		return "", errors.New("mesh: existing netbird config has no peer key; provide a setup key")
	}
	return cfg.PrivateKey, nil
}

// Listen listens on the port of this peer's mesh address.
func (n *NetBird) Listen(port int) (net.Listener, error) {
	return n.c.ListenTCP(":" + strconv.Itoa(port))
}

// DialContext connects through the mesh.
func (n *NetBird) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return n.c.DialContext(ctx, network, address)
}

// HTTPClient returns an HTTP client that uses the mesh.
func (n *NetBird) HTTPClient() *http.Client {
	return n.c.NewHTTPClient()
}

// PeerForAddr maps the mesh IP of a connection to the peer's WireGuard
// public key, using the client's peer status.
func (n *NetBird) PeerForAddr(remoteAddr string) (string, string, bool) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", "", false
	}
	key, fqdn, ok := n.c.IdentityForIP(ap.Addr().Unmap())
	if !ok || key == "" {
		return "", "", false
	}
	return key, fqdn, true
}

// Stop disconnects from the mesh; ctx bounds the wait.
func (n *NetBird) Stop(ctx context.Context) error {
	return n.c.Stop(ctx)
}

// Monitor logs changes of the connection to the NetBird management and
// signal services until ctx is done. The client reconnects on its own;
// this only makes outages visible.
func (n *NetBird) Monitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	mgmt, signal := true, true
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := n.c.Status()
		if err != nil {
			n.log.Warn("netbird status unavailable", "err", err)
			continue
		}
		if st.ManagementState.Connected != mgmt {
			mgmt = st.ManagementState.Connected
			if mgmt {
				n.log.Info("netbird management connection restored")
			} else {
				n.log.Warn("netbird management connection lost", "err", st.ManagementState.Error)
			}
		}
		if st.SignalState.Connected != signal {
			signal = st.SignalState.Connected
			if signal {
				n.log.Info("netbird signal connection restored")
			} else {
				n.log.Warn("netbird signal connection lost", "err", st.SignalState.Error)
			}
		}
		connected := 0
		for _, p := range st.Peers {
			if p.ConnStatus == embed.PeerStatusConnected {
				connected++
			}
		}
		n.log.Debug("netbird status", "peers", len(st.Peers), "connected", connected)
	}
}
