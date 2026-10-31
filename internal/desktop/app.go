package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	Printers PrinterManager
	Login    LoginItem
	Log      *slog.Logger
	// SyncInterval is the time between printer syncs; default 5 minutes.
	SyncInterval time.Duration
	// OnChange is called after the state changed, e.g. to update a tray menu.
	OnChange func()

	mu          sync.Mutex
	cfg         Config
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
	recv     *tcpReceiver
}

// MaxDocument is the largest document the app accepts from a printer.
const MaxDocument = 512 << 20

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

// Config is stored as config.json in the data directory.
type Config struct {
	Address string `json:"address"`
	// Locked keeps users from changing the address; set by an administrator
	// (Windows installer option /LOCK).
	Locked bool `json:"locked,omitempty"`
}

// ErrLocked is returned when users may not change the server.
var ErrLocked = errors.New("the print server was set by your administrator and cannot be changed")

func (a *App) configPath() string { return ConfigPath(a.Dir) }

// ConfigPath returns the configuration file of the data directory dir.
func ConfigPath(dir string) string { return filepath.Join(dir, "config.json") }

// WriteConfig stores cfg in the data directory dir.
func WriteConfig(dir string, cfg Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	tmp := ConfigPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ConfigPath(dir))
}

// Start opens the spool, starts receiving from the local printers and the
// background loops, which end when ctx is canceled.
func (a *App) Start(ctx context.Context) error {
	if a.Log == nil {
		a.Log = slog.Default()
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

	a.recv = newTCPReceiver(ctx, a)
	// Receive for the installed printers at once, even while the server
	// is unreachable, and move printers whose port is taken now.
	go func() {
		a.syncMu.Lock()
		defer a.syncMu.Unlock()
		installed, err := a.Printers.Installed(ctx)
		if err != nil {
			return
		}
		want := map[string]listenSpec{}
		for _, p := range installed {
			if id, port, ok := p.Managed(); ok {
				want[id] = listenSpec{Name: p.Name, Port: port}
			}
		}
		ports := a.listen(want)
		if moves := PlanMoves(installed, ports); len(moves) > 0 {
			if err := a.Printers.Apply(ctx, moves, ""); err != nil {
				a.Log.Error("moving printers to new ports failed", "err", err)
				return
			}
			a.Log.Info("printers moved to new ports", "printers", len(moves))
		}
	}()
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

// Receive stores a document a local printer delivers and starts sending it.
// It reads r right away: the spooler may wait for the reader while holding
// the printer, so the title (the document name in the print queue) comes
// from title only after the document was read; it may return "".
func (a *App) Receive(printerID string, title func() string, r io.Reader) error {
	f, err := os.CreateTemp(a.incoming, "job-*")
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, MaxDocument+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > MaxDocument {
		err = fmt.Errorf("the document is larger than %d MB", MaxDocument>>20)
	}
	if err == nil && n == 0 {
		err = errors.New("the document is empty")
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	name := title()
	if name == "" {
		name = "Untitled"
	}
	job, err := a.spool.Add(printerID, name, 1, f.Name(), time.Now())
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	a.Log.Info("job received", "printer", job.PrinterID, "title", job.Title, "size", n)
	a.kickForwarder()
	a.changed()
	return nil
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
	a.mu.Lock()
	locked := a.cfg.Locked && a.cfg.Address != address
	a.mu.Unlock()
	if locked {
		return ErrLocked
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
	ports := a.listen(listenSpecs(remote, installed))
	actions := PlanPrinters(installed, remote, ports)
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
	// The receiver looks up job titles by the printers' current names.
	a.listen(listenSpecs(remote, installed))
	a.fwd.SetClient(c)
	a.kickForwarder()
	return nil
}

// listen makes the receiver listen for exactly the printers in want and
// returns their ports.
func (a *App) listen(want map[string]listenSpec) map[string]int {
	ports, msg := a.recv.listen(want)
	a.setReceiverErr(msg)
	return ports
}

// listenSpecs describes the wanted printers with their current local name
// and port.
func listenSpecs(remote []api.Printer, installed []LocalPrinter) map[string]listenSpec {
	local := map[string]listenSpec{}
	for _, p := range installed {
		if id, port, ok := p.Managed(); ok {
			local[id] = listenSpec{Name: p.Name, Port: port}
		}
	}
	want := map[string]listenSpec{}
	for _, r := range WantedPrinters(remote) {
		want[r.ID] = local[r.ID]
	}
	return want
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
	a.mu.Lock()
	locked := a.cfg.Locked
	a.mu.Unlock()
	if locked {
		return ErrLocked
	}
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
	actions := PlanPrinters(installed, nil, nil)
	if err := a.Printers.Apply(ctx, actions, ""); err != nil {
		return err
	}
	a.mu.Lock()
	a.local = nil
	a.mu.Unlock()
	if a.recv != nil {
		a.listen(nil)
	}
	return nil
}

func (a *App) saveConfig(cfg Config) error { return WriteConfig(a.Dir, cfg) }

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
	a.mu.Lock()
	same := a.receiverErr == msg
	a.mu.Unlock()
	if same {
		return
	}
	if msg != "" {
		a.Log.Error(msg)
	}
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
	Locked        bool           `json:"locked,omitempty"`
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
		Busy: a.busy, Locked: a.cfg.Locked, Printers: []PrinterState{}}
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
