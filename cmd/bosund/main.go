// Command bosund is the Bosun daemon: it owns the agent, memory, schedules,
// and the HTTP API that the bosun CLI and web UI talk to.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/store"
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

	if err := ensureDataDir(cfg.Daemon.DataDir, log); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	ver, err := st.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Info("database ready", "path", cfg.DBPath(), "schema_version", ver)

	authSvc, err := auth.New(st, auth.Options{})
	if err != nil {
		return err
	}
	if ok, err := authSvc.HasUsers(ctx); err != nil {
		return err
	} else if !ok {
		log.Warn("no users exist; run `bosun setup` to create the first admin")
	}

	bus := events.NewBus()
	apiSrv, err := api.New(api.Options{Auth: authSvc, Bus: bus, Logger: log, Version: version})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Daemon.Listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams are long-lived. Request contexts derive
		// from ctx so streams end on shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", cfg.Daemon.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Daemon.Listen, err)
	}

	go sweepSessions(ctx, st, log)

	errc := make(chan error, 1)
	go func() {
		log.Info("bosund listening", "addr", ln.Addr().String(), "tls", cfg.Daemon.TLS.Enabled, "version", version)
		if cfg.Daemon.TLS.Enabled {
			errc <- srv.ServeTLS(ln, cfg.Daemon.TLS.CertFile, cfg.Daemon.TLS.KeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
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
