package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/tim-riep/go-platen/internal/desktop"
)

// The Windows service runs as LocalSystem for all users: it manages the
// printers, receives and spools the jobs and serves the settings page.
// Users need no administrator rights.

const serviceName = "Platen"

// serviceDir holds configuration, spool and log. Only SYSTEM and
// administrators may enter it: the spool holds every user's documents.
func serviceDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Platen")
}

// uiInfoPath is the file that tells the users' tray icons where the
// settings page is; signed-in users may read it.
func uiInfoPath() string { return filepath.Join(serviceDir(), "public", "ui.json") }

const (
	privateDACL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	publicDACL  = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;AU)"
)

// secureDir creates dir and replaces its permissions with sddl; the
// entries are inherited by everything inside.
func secureDir(dir, sddl string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func prepareDirs() error {
	if err := secureDir(serviceDir(), privateDACL); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if err := secureDir(filepath.Dir(uiInfoPath()), publicDACL); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	return nil
}

func runService() error {
	if ok, err := svc.IsWindowsService(); err != nil || !ok {
		return errors.New("the service is started by Windows; install it with install-service")
	}
	return svc.Run(serviceName, handler{})
}

type handler struct{}

func (handler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	if err := prepareDirs(); err != nil {
		return true, 1
	}
	log, closeLog := newLogger(serviceDir())
	defer closeLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := startApp(ctx, serviceDir(), uiInfoPath(), log); err != nil {
		log.Error("service start failed", "err", err)
		return true, 2
	}
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			s <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			log.Info("service stopping")
			s <- svc.Status{State: svc.StopPending}
			cancel()
			// Let the page and the pipes close.
			time.Sleep(200 * time.Millisecond)
			return false, 0
		}
	}
	return false, 0
}

// installService creates or updates the service and (re)starts it.
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := prepareDirs(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err == nil {
		stopService(s)
		cfg, err := s.Config()
		if err != nil {
			s.Close()
			return err
		}
		cfg.BinaryPathName = `"` + exe + `" service`
		cfg.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(cfg); err != nil {
			s.Close()
			return err
		}
	} else {
		s, err = m.CreateService(serviceName, exe, mgr.Config{
			DisplayName: "Platen",
			Description: "Installs the printers of the platen print server and sends their jobs to it.",
			StartType:   mgr.StartAutomatic,
		}, "service")
		if err != nil {
			return err
		}
	}
	defer s.Close()
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Minute},
	}, uint32((24 * time.Hour).Seconds()))
	if err != nil {
		return err
	}
	return s.Start()
}

// stopService stops s and waits up to 30 seconds.
func stopService(s *mgr.Service) {
	st, err := s.Query()
	if err != nil || st.State == svc.Stopped {
		return
	}
	s.Control(svc.Stop)
	for i := 0; i < 60; i++ {
		if st, err := s.Query(); err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// uninstallService stops and deletes the service and removes its printers.
func uninstallService(ctx context.Context) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		stopService(s)
		err = s.Delete()
		s.Close()
		if err != nil {
			return err
		}
	}
	installed, err := desktop.InstalledPrinters(ctx)
	if err != nil {
		return err
	}
	return desktop.ApplyPrinters(ctx, desktop.PlanPrinters(installed, nil), "")
}

// setServer stores the server for the service; a running service is
// restarted to use it.
func setServer(cfg desktop.Config) error {
	if err := prepareDirs(); err != nil {
		return err
	}
	if err := desktop.WriteConfig(serviceDir(), cfg); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil // not installed yet
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State == svc.Running {
		stopService(s)
		return s.Start()
	}
	return nil
}
