// Command platen is a print management server with distributed print workers.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tim-riep/go-platen/internal/admin"
	"github.com/tim-riep/go-platen/internal/app"
	"github.com/tim-riep/go-platen/internal/auth"
	"github.com/tim-riep/go-platen/internal/config"
	"github.com/tim-riep/go-platen/internal/mesh"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `platen - print management server with distributed print workers

Usage:
  platen server -config <file>   run the management server
  platen worker -config <file>   run a standalone worker
  platen admin  <command> ...    administer a server over NetBird
  platen hash-token [-generate]  hash an admin token read from stdin
  platen version                 print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "server":
		err = runServer(args)
	case "worker":
		err = runWorker(args)
	case "admin":
		err = runAdmin(args)
	case "hash-token":
		err = hashToken(args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "platen:", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	l, _ := config.ParseLevel(level)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
	// Libraries that use the standard log package (e.g. the QUIC transport
	// of NetBird) then log through slog as well.
	slog.SetDefault(log)
	return log
}

func configFlag(name string, args []string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "", "path to the TOML configuration file")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *path == "" {
		return "", errors.New("-config is required")
	}
	return *path, nil
}

// withMesh connects to NetBird, runs fn and disconnects afterwards. fn's
// context is canceled on SIGINT or SIGTERM.
func withMesh(nb config.NetBird, blockInbound bool, log *slog.Logger, fn func(ctx context.Context, n mesh.Network) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	key, err := nb.SetupKey()
	if err != nil {
		return err
	}
	n, err := mesh.Start(ctx, mesh.Options{
		ManagementURL: nb.ManagementURL, ConfigPath: nb.ConfigPath, SetupKey: key, Hostname: nb.Hostname,
		BlockInbound: blockInbound, LogLevel: nb.LogLevel, Logger: log,
	})
	if err != nil {
		return err
	}
	monCtx, stopMon := context.WithCancel(ctx)
	go n.Monitor(monCtx, 0)

	runErr := fn(ctx, n)

	// The application has stopped; now leave the mesh.
	stopMon()
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Stop(shCtx); err != nil {
		log.Warn("netbird shutdown", "err", err)
	}
	return runErr
}

func runServer(args []string) error {
	path, err := configFlag("server", args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadServer(path)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	return withMesh(cfg.NetBird, false, log, func(ctx context.Context, n mesh.Network) error {
		return app.RunServer(ctx, cfg, n, log, version)
	})
}

func runWorker(args []string) error {
	path, err := configFlag("worker", args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWorker(path)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	// Workers accept no connections from the mesh.
	return withMesh(cfg.NetBird, true, log, func(ctx context.Context, n mesh.Network) error {
		return app.RunWorker(ctx, cfg, n, log, version)
	})
}

func hashToken(args []string) error {
	fs := flag.NewFlagSet("hash-token", flag.ContinueOnError)
	generate := fs.Bool("generate", false, "generate a new admin token instead of reading one from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var token string
	if *generate {
		token = auth.NewToken(auth.PrefixAdmin)
		fmt.Fprintln(os.Stderr, "token (store it securely, it is not shown again):")
		fmt.Println(token)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read token from stdin: %w", err)
		}
		token = strings.TrimSpace(line)
		if token == "" {
			return errors.New("empty token")
		}
	}
	fmt.Fprintln(os.Stderr, "admin_token_hash for the server configuration:")
	fmt.Println(auth.Hash(token))
	return nil
}

func runAdmin(args []string) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	path := fs.String("config", "", "path to the admin TOML configuration file")
	fs.Usage = func() { fmt.Fprint(os.Stderr, admin.Usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || fs.NArg() < 2 {
		fs.Usage()
		return errors.New("-config and a command are required")
	}
	cfg, err := config.LoadAdmin(*path)
	if err != nil {
		return err
	}
	token, err := cfg.AdminToken()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	return withMesh(cfg.NetBird, true, log, func(ctx context.Context, n mesh.Network) error {
		c := &admin.Client{BaseURL: cfg.ServerURL, HTTP: n.HTTPClient(), Token: token}
		err := admin.Run(ctx, c, fs.Args(), os.Stdout)
		if errors.Is(err, admin.ErrUsage) {
			fmt.Fprint(os.Stderr, admin.Usage)
		}
		return err
	})
}
