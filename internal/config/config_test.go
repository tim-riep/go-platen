package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExampleConfigs(t *testing.T) {
	s, err := LoadServer("../../deploy/config/server.toml")
	if err != nil {
		t.Fatal(err)
	}
	if s.NetBird.Port != 8080 || s.Server.JobRetention.Duration != 168*time.Hour || !s.LocalWorker.Enabled {
		t.Fatalf("server: %+v", s)
	}
	w, err := LoadWorker("../../deploy/config/worker.toml")
	if err != nil {
		t.Fatal(err)
	}
	if w.Worker.ServerURL != "http://platen-server.netbird.selfhosted:8080" {
		t.Fatalf("worker: %+v", w)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(p, []byte("[netbird]\nconfig_path = \"x\"\nsetup_key = \"plain\"\n[server]\ndatabase = \"d\"\nspool_dir = \"s\"\n"), 0o600)
	if _, err := LoadServer(p); err == nil {
		t.Fatal("setup_key in TOML accepted")
	}
}

func TestSetupKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	os.WriteFile(keyFile, []byte(" from-file \n"), 0o600)
	n := NetBird{SetupKeyFile: keyFile}
	if k, _ := n.SetupKey(); k != "from-file" {
		t.Fatalf("key %q", k)
	}
	t.Setenv(EnvSetupKey, "from-env")
	if k, _ := n.SetupKey(); k != "from-env" {
		t.Fatalf("key %q", k)
	}
	t.Setenv(EnvSetupKey, "")
	n.SetupKeyFile = filepath.Join(dir, "missing")
	if k, err := n.SetupKey(); k != "" || err != nil {
		t.Fatalf("missing key file: %q %v", k, err)
	}
}
