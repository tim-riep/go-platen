// Package config loads the TOML configuration of the server and worker.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Environment variables that override secrets from files.
const (
	EnvSetupKey   = "PLATEN_NETBIRD_SETUP_KEY"
	EnvAdminToken = "PLATEN_ADMIN_TOKEN"
)

// Duration is a time.Duration written as a string such as "30s" or "168h".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// NetBird configures the embedded NetBird client. The setup key is never
// part of the TOML file: it is read from the PLATEN_NETBIRD_SETUP_KEY
// environment variable or from SetupKeyFile.
type NetBird struct {
	ManagementURL string `toml:"management_url"`
	// ConfigPath stores the peer identity; it must be persistent.
	ConfigPath   string `toml:"config_path"`
	SetupKeyFile string `toml:"setup_key_file"`
	Hostname     string `toml:"hostname"`
	// Port is the port the server listens on in the mesh.
	Port     int    `toml:"port"`
	LogLevel string `toml:"log_level"`
}

// SetupKey returns the setup key from the environment or the key file. An
// empty key is fine once the peer is registered.
func (n NetBird) SetupKey() (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvSetupKey)); v != "" {
		return v, nil
	}
	if n.SetupKeyFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(n.SetupKeyFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read setup key: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (n *NetBird) validate(defaultHost string) error {
	n.SetupKeyFile = os.ExpandEnv(n.SetupKeyFile)
	n.ConfigPath = os.ExpandEnv(n.ConfigPath)
	if n.ConfigPath == "" {
		return errors.New("netbird.config_path is required")
	}
	if n.Hostname == "" {
		n.Hostname = defaultHost
	}
	if n.LogLevel == "" {
		n.LogLevel = "warn"
	}
	return nil
}

// Server is the configuration of "platen server".
type Server struct {
	LogLevel    string      `toml:"log_level"`
	NetBird     NetBird     `toml:"netbird"`
	Server      ServerOpts  `toml:"server"`
	LocalWorker LocalWorker `toml:"local_worker"`
}

// ServerOpts are the server settings.
type ServerOpts struct {
	Database string `toml:"database"`
	SpoolDir string `toml:"spool_dir"`
	// AdminTokenHash is "sha256:<hex>" as printed by "platen hash-token".
	AdminTokenHash  string   `toml:"admin_token_hash"`
	MaxDocumentSize int64    `toml:"max_document_bytes"`
	JobRetention    Duration `toml:"job_retention"`
	CleanupInterval Duration `toml:"cleanup_interval"`
	LeaseDuration   Duration `toml:"lease_duration"`
	MaxAttempts     int      `toml:"max_attempts"`
	EnrollmentTTL   Duration `toml:"enrollment_token_ttl"`
}

// LocalWorker configures the worker built into the server.
type LocalWorker struct {
	Enabled    bool   `toml:"enabled"`
	Name       string `toml:"name"`
	JournalDir string `toml:"journal_dir"`
	TempDir    string `toml:"temp_dir"`
}

// Worker is the configuration of "platen worker".
type Worker struct {
	LogLevel string     `toml:"log_level"`
	NetBird  NetBird    `toml:"netbird"`
	Worker   WorkerOpts `toml:"worker"`
}

// WorkerOpts are the worker settings. Printers are not configured here;
// they come from the server.
type WorkerOpts struct {
	// ServerURL is the server address in the mesh, e.g.
	// "http://platen-server.netbird.cloud:8080".
	ServerURL           string `toml:"server_url"`
	CredentialFile      string `toml:"credential_file"`
	JournalDir          string `toml:"journal_dir"`
	EnrollmentTokenFile string `toml:"enrollment_token_file"`
	TempDir             string `toml:"temp_dir"`
}

func decode(path string, v any) error {
	md, err := toml.DecodeFile(path, v)
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	return nil
}

// LoadServer reads and validates a server configuration.
func LoadServer(path string) (*Server, error) {
	c := &Server{}
	if err := decode(path, c); err != nil {
		return nil, err
	}
	if err := c.NetBird.validate("platen-server"); err != nil {
		return nil, err
	}
	if c.NetBird.Port == 0 {
		c.NetBird.Port = 8080
	}
	s := &c.Server
	if s.Database == "" || s.SpoolDir == "" {
		return nil, errors.New("server.database and server.spool_dir are required")
	}
	if s.MaxDocumentSize <= 0 {
		s.MaxDocumentSize = 64 << 20
	}
	if s.JobRetention.Duration <= 0 {
		s.JobRetention.Duration = 7 * 24 * time.Hour
	}
	if c.LocalWorker.Enabled && c.LocalWorker.JournalDir == "" {
		return nil, errors.New("local_worker.journal_dir is required when the built-in worker is enabled")
	}
	if _, err := ParseLevel(c.LogLevel); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadWorker reads and validates a worker configuration.
func LoadWorker(path string) (*Worker, error) {
	c := &Worker{}
	if err := decode(path, c); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	if err := c.NetBird.validate("platen-worker-" + host); err != nil {
		return nil, err
	}
	w := &c.Worker
	if w.ServerURL == "" || w.CredentialFile == "" || w.JournalDir == "" {
		return nil, errors.New("worker.server_url, worker.credential_file and worker.journal_dir are required")
	}
	w.ServerURL = strings.TrimSuffix(w.ServerURL, "/")
	w.EnrollmentTokenFile = os.ExpandEnv(w.EnrollmentTokenFile)
	if _, err := ParseLevel(c.LogLevel); err != nil {
		return nil, err
	}
	return c, nil
}

// Admin is the configuration of "platen admin". The admin CLI joins the
// mesh as its own peer.
type Admin struct {
	LogLevel  string  `toml:"log_level"`
	NetBird   NetBird `toml:"netbird"`
	ServerURL string  `toml:"server_url"`
	// AdminTokenFile holds the admin token; PLATEN_ADMIN_TOKEN overrides it.
	AdminTokenFile string `toml:"admin_token_file"`
}

// LoadAdmin reads and validates an admin CLI configuration.
func LoadAdmin(path string) (*Admin, error) {
	c := &Admin{}
	if err := decode(path, c); err != nil {
		return nil, err
	}
	if err := c.NetBird.validate("platen-admin"); err != nil {
		return nil, err
	}
	if c.ServerURL == "" {
		return nil, errors.New("server_url is required")
	}
	c.ServerURL = strings.TrimSuffix(c.ServerURL, "/")
	c.AdminTokenFile = os.ExpandEnv(c.AdminTokenFile)
	if c.LogLevel == "" {
		c.LogLevel = "warn"
	}
	if _, err := ParseLevel(c.LogLevel); err != nil {
		return nil, err
	}
	return c, nil
}

// AdminToken returns the admin token from the environment or the file.
func (c *Admin) AdminToken() (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvAdminToken)); v != "" {
		return v, nil
	}
	if c.AdminTokenFile == "" {
		return "", errors.New("no admin token: set " + EnvAdminToken + " or admin_token_file")
	}
	b, err := os.ReadFile(c.AdminTokenFile)
	if err != nil {
		return "", fmt.Errorf("read admin token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// ParseLevel parses a log level name; empty means info.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if s == "" {
		return slog.LevelInfo, nil
	}
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("invalid log_level %q", s)
	}
	return l, nil
}
