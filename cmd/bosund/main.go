// Command bosund is the Bosun daemon: it owns the agent, memory, schedules,
// and the HTTP API that the bosun CLI and web UI talk to.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/envfile"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bosund:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultPath(), "path to bosun.yaml (env BOSUN_CONFIG)")
	debug := flag.Bool("debug", false, "enable debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("%w (run `bosun setup` to create one)", err)
	}
	if err := startupChecks(cfg, log); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := newDaemon(ctx, cfg, log, deps{})
	if err != nil {
		return err
	}
	defer d.Close()
	ln, err := net.Listen("tcp", cfg.Daemon.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Daemon.Listen, err)
	}
	return d.Serve(ctx, ln)
}

// startupChecks loads $data_dir/env into the environment (so service
// definitions never hold secrets) and warns about risky proxy settings.
func startupChecks(cfg *config.Config, log *slog.Logger) error {
	set, err := envfile.Load(cfg.EnvFilePath())
	if err != nil {
		return err
	}
	if len(set) > 0 {
		sort.Strings(set)
		log.Info("loaded environment file", "file", cfg.EnvFilePath(), "variables", strings.Join(set, ","))
	}
	if cfg.Daemon.BehindProxy {
		if len(cfg.Daemon.TrustedProxies) == 0 {
			log.Warn("daemon.behind_proxy is set without daemon.trusted_proxies: X-Forwarded-* headers are trusted from every loopback and private address; list your proxy's address",
				"default", strings.Join(config.DefaultTrustedProxies, ","))
		}
		if host, _, err := net.SplitHostPort(cfg.Daemon.Listen); err == nil {
			if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				log.Warn("daemon.behind_proxy with a non-loopback listen address serves plain HTTP to the network; listen on 127.0.0.1 if the proxy runs on this host",
					"listen", cfg.Daemon.Listen)
			}
		}
	}
	return nil
}

// ensureDataDir creates the data dir with 0700 and warns if it is accessible
// to other users.
func ensureDataDir(dir string, log *slog.Logger) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir %s: %w", dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat data dir: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		log.Warn("data dir is accessible to other users; run `chmod 700` on it", "dir", dir, "mode", fi.Mode().Perm().String())
	}
	return nil
}
