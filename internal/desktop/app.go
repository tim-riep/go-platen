package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
)

// App is the desktop client without its user interface: it receives jobs
// from the local printers, spools and forwards them, and keeps the local
// printers in line with the server's.
type App struct {
	Dir      string // data directory (config, spool)
	Version  string
	LPDAddr  string // default 127.0.0.1:515
	Printers PrinterManager
	Login    LoginItem
	Log      *slog.Logger
	// SyncInterval is the time between printer syncs; default 5 minutes.
	SyncInterval time.Duration
	// OnChange is called after the state changed, e.g. to update a tray menu.
	OnChange func()

	mu          sync.Mutex
	cfg         config
	me          *api.ClientMe
	remote      []api.Printer
	local       []LocalPrinter
	lastSync    time.Time
	lastErr     string
	receiverErr string
	busy        bool

	syncMu   sync.Mutex
	spool    *Spool
	fwd      *Forwarder
	incoming string
	kick     chan struct{}
}

// PrinterManager changes the local printers.
type PrinterManager interface {
	Installed(ctx context.Context) ([]LocalPrinter, error)
	Apply(ctx context.Context, actions []PrinterAction, location string) error
}

// LoginItem starts the app at login.
type LoginItem interface {
	Enabled() bool
	SetEnabled(on bool) error
}

// SystemPrinters manages this computer's printers.
type SystemPrinters struct{}

func (SystemPrinters) Installed(ctx context.Context) ([]LocalPrinter, error) {
	return InstalledPrinters(ctx)
}

func (SystemPrinters) Apply(ctx context.Context, actions []PrinterAction, location string) error {
	return ApplyPrinters(ctx, actions, location)
}

type config struct {
	Address string `json:"address"`
}

func (a *App) configPath() string { return filepath.Join(a.Dir, "config.json") }

// Start opens the spool, starts the LPD receiver and the background loops.
// It returns when ctx is canceled.
func (a *App) Start(ctx context.Context) error {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	if a.LPDAddr == "" {
		a.LPDAddr = fmt.Sprintf("127.0.0.1:%d", LPDPortWin)
	}
	if a.SyncInterval == 0 {
		a.SyncInterval = 5 * time.Minute
	}
	var err error
	if a.spool, err = OpenSpool(filepath.Join(a.Dir, "spool")); err != nil {
		return err
	}
	a.incoming = filepath.Join(a.Dir, "incoming")
	if err := os.MkdirAll(a.incoming, 0o700); err != nil {
		return err
	}
	a.fwd = &Forwarder{Spool: a.spool, OnEvent: a.forwardEvent}
	a.kick = make(chan struct{}, 1)
	if data, err := os.ReadFile(a.configPath()); err == nil {
		json.Unmarshal(data, &a.cfg)
	}
	if a.cfg.Address != "" {
		if c, err := NewClient(a.cfg.Address, a.clientName()); err == nil {
			a.fwd.SetClient(c)
		}
	}

	ln, err := net.Listen("tcp", a.LPDAddr)
	if err != nil {
		a.setReceiverErr(fmt.Sprintf("The print receiver could not listen on %s: %v", a.LPDAddr, err))
	} else {
		go a.serveLPD(ctx, ln)
		go func() { <-ctx.Done(); ln.Close() }()
	}
	go a.loop(ctx)
	return nil
}

func (a *App) clientName() string { return "platen-desktop/" + a.Version }

func (a *App) loop(ctx context.Context) {
	a.Sync(ctx)
	syncT := time.NewTicker(a.SyncInterval)
	fwdT := time.NewTicker(15 * time.Second)
	defer syncT.Stop()
	defer fwdT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncT.C:
			a.Sync(ctx)
		case <-fwdT.C:
		case <-a.kick:
		}
		a.fwd.Run(ctx, time.Now())
		a.changed()
	}
}

func (a *App) kickForwarder() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *App) serveLPD(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				a.setReceiverErr("The print receiver stopped: " + err.Error())
			}
			return
		}
		go a.handleLPD(conn)
	}
}

func (a *App) handleLPD(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Minute))
	jobs, err := ReceiveLPD(conn, a.incoming, LPDLimits{})
	if err != nil {
		a.Log.Warn("lpd: receive failed", "err", err)
	}
	for _, j := range jobs {
		for _, f := range j.Files {
			job, err := a.spool.Add(j.Queue, j.Control.Title(), j.Copies, f, time.Now())
			if err != nil {
				a.Log.Error("spool: add failed", "err", err)
				os.Remove(f)
				continue
			}
			a.Log.Info("job received", "printer", job.PrinterID, "title", job.Title, "copies", job.Copies)
		}
	}
	if len(jobs) > 0 {
		a.kickForwarder()
		a.changed()
	}
}

func (a *App) forwardEvent(e ForwardEvent) {
	switch e.Kind {
	case "sent":
		a.Log.Info("job sent", "printer", e.Job.PrinterID, "title", e.Job.Title, "job", e.RemoteID)
	case "waiting":
		a.Log.Warn("job waiting", "printer", e.Job.PrinterID, "title", e.Job.Title, "err", e.Job.LastError)
	case "failed":
		a.Log.Error("job not printed", "printer", e.Job.PrinterID, "title", e.Job.Title, "err", e.Job.LastError)
	}
	a.changed()
}

// Connect connects to the server at address and installs its printers.
func (a *App) Connect(ctx context.Context, address string) error {
	if _, err := ServerURL(address); err != nil {
		return err
	}
	first := a.Address() == ""
	if err := a.sync(ctx, address); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.Address = address
	cfg := a.cfg
	a.mu.Unlock()
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	if first && a.Login != nil && !a.Login.Enabled() {
		// Printing needs the app running, so it starts at login by default.
		if err := a.Login.SetEnabled(true); err != nil {
			a.Log.Warn("start at login", "err", err)
		}
	}
	return nil
}

// Sync updates the printers from the configured server.
func (a *App) Sync(ctx context.Context) error {
	addr := a.Address()
	if addr == "" {
		return nil
	}
	return a.sync(ctx, addr)
}

func (a *App) sync(ctx context.Context, address string) (err error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.setBusy(true)
	defer func() {
		a.mu.Lock()
		a.busy = false
		if err != nil {
			a.lastErr = Describe(err)
		} else {
			a.lastErr = ""
		}
		a.mu.Unlock()
		a.changed()
	}()
	c, err := NewClient(address, a.clientName())
	if err != nil {
		return err
	}
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}
	remote, err := c.Printers(ctx)
	if err != nil {
		return err
	}
	installed, err := a.Printers.Installed(ctx)
	if err != nil {
		return err
	}
	actions := PlanPrinters(installed, remote)
	if len(actions) > 0 {
		host := c.BaseURL
		if u, err := url.Parse(c.BaseURL); err == nil {
			host = u.Hostname()
		}
		applyErr := a.Printers.Apply(ctx, actions, PrinterName("platen on "+host))
		if installed, err = a.Printers.Installed(ctx); err != nil {
			return err
		}
		if applyErr != nil {
			return applyErr
		}
		a.Log.Info("printers changed", "actions", len(actions))
	}
	a.mu.Lock()
	a.me, a.remote, a.local, a.lastSync = me, remote, managedOnly(installed), time.Now()
	a.mu.Unlock()
	a.fwd.SetClient(c)
	a.kickForwarder()
	return nil
}

func managedOnly(ps []LocalPrinter) []LocalPrinter {
	var out []LocalPrinter
	for _, p := range ps {
		if _, ok := p.PrinterID(); ok {
			out = append(out, p)
		}
	}
	return out
}

// Disconnect removes the app's printers and forgets the server.
func (a *App) Disconnect(ctx context.Context) error {
	if err := a.RemovePrinters(ctx); err != nil {
		a.mu.Lock()
		a.lastErr = Describe(err)
		a.mu.Unlock()
		a.changed()
		return err
	}
	a.mu.Lock()
	a.cfg.Address = ""
	cfg := a.cfg
	a.me, a.remote, a.local, a.lastErr = nil, nil, nil, ""
	a.mu.Unlock()
	a.fwd.SetClient(nil)
	a.changed()
	return a.saveConfig(cfg)
}

// RemovePrinters removes all printers the app installed.
func (a *App) RemovePrinters(ctx context.Context) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	installed, err := a.Printers.Installed(ctx)
	if err != nil {
		return err
	}
	actions := PlanPrinters(installed, nil)
	if err := a.Printers.Apply(ctx, actions, ""); err != nil {
		return err
	}
	a.mu.Lock()
	a.local = nil
	a.mu.Unlock()
	return nil
}

func (a *App) saveConfig(cfg config) error {
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(a.configPath(), data, 0o600)
}

// RetryNow sends all waiting jobs now.
func (a *App) RetryNow() {
	a.fwd.RetryNow(time.Now())
	a.kickForwarder()
}

// Discard removes a job from the spool.
func (a *App) Discard(id string) {
	a.spool.Remove(id)
	a.changed()
}

func (a *App) Address() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Address
}

func (a *App) setBusy(b bool) {
	a.mu.Lock()
	a.busy = b
	a.mu.Unlock()
	a.changed()
}

func (a *App) setReceiverErr(msg string) {
	a.Log.Error(msg)
	a.mu.Lock()
	a.receiverErr = msg
	a.mu.Unlock()
	a.changed()
}

func (a *App) changed() {
	if a.OnChange != nil {
		a.OnChange()
	}
}

// State is the app's state for the user interface.
type State struct {
	Version       string         `json:"version"`
	Address       string         `json:"address"`
	Me            *api.ClientMe  `json:"me,omitempty"`
	Printers      []PrinterState `json:"printers"`
	Jobs          []SpoolJob     `json:"jobs"`
	LastSync      *time.Time     `json:"last_sync,omitempty"`
	Error         string         `json:"error,omitempty"`
	ReceiverError string         `json:"receiver_error,omitempty"`
	Busy          bool           `json:"busy"`
	Login         *bool          `json:"login,omitempty"`
	Waiting       int            `json:"waiting"`
	FailedJobs    int            `json:"failed_jobs"`
}

// PrinterState is a server printer with its local printer.
type PrinterState struct {
	api.Printer
	LocalName  string `json:"local_name,omitempty"`
	AcceptsPDF bool   `json:"accepts_pdf"`
}

func (a *App) State() State {
	a.mu.Lock()
	st := State{Version: a.Version, Address: a.cfg.Address, Me: a.me, Error: a.lastErr, ReceiverError: a.receiverErr,
		Busy: a.busy, Printers: []PrinterState{}}
	if !a.lastSync.IsZero() {
		t := a.lastSync
		st.LastSync = &t
	}
	local := map[string]string{}
	for _, p := range a.local {
		id, _ := p.PrinterID()
		local[id] = p.Name
	}
	for _, p := range a.remote {
		st.Printers = append(st.Printers, PrinterState{Printer: p, LocalName: local[p.ID], AcceptsPDF: AcceptsPDF(p)})
	}
	a.mu.Unlock()
	if a.spool != nil {
		st.Jobs = a.spool.Jobs()
	}
	if st.Jobs == nil {
		st.Jobs = []SpoolJob{}
	}
	for _, j := range st.Jobs {
		if j.Failed {
			st.FailedJobs++
		} else {
			st.Waiting++
		}
	}
	if a.Login != nil {
		on := a.Login.Enabled()
		st.Login = &on
	}
	return st
}

// Describe turns an error into a message for the user.
func Describe(err error) string {
	var ae *api.Error
	var te *TransportError
	switch {
	case errors.As(err, &ae) && ae.Status == 403:
		return ae.Message + ". Ask an administrator to give this device a role in the platen panel."
	case errors.As(err, &te):
		return te.Error() + ". Is NetBird connected?"
	}
	return err.Error()
}
