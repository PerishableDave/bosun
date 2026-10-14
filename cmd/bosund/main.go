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
	"syscall"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/store"
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

// sweepSessions periodically deletes expired sessions.
func sweepSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.DeleteExpiredSessions(ctx); err != nil && ctx.Err() == nil {
			log.Warn("sweep sessions", "err", err)
		} else if n > 0 {
			log.Debug("swept expired sessions", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
