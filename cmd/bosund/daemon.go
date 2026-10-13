package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dperedo/bosun/internal/agent"
	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/approval"
	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/dispatch"
	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/integrations/websearch"
	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/providers"
	"github.com/dperedo/bosun/internal/policy"
	"github.com/dperedo/bosun/internal/store"
	"github.com/dperedo/bosun/internal/vault"
)

// builtins are the first-party integrations that can be enabled under
// `integrations:` in bosun.yaml.
var builtins = map[string]func() integration.Integration{
	"websearch": func() integration.Integration { return websearch.New() },
}

// deps lets tests substitute parts of the daemon. Zero values mean "build
// from config".
type deps struct {
	// LLM replaces the provider registry built by providers.FromConfig.
	LLM *llm.Registry
	// Integrations are initialised and registered in addition to the
	// enabled built-ins (tests use this for stub tools).
	Integrations []integration.Integration
	// Argon2 overrides password hashing parameters (tests only).
	Argon2 *auth.Argon2Params
	// ApprovalSweep overrides the approval expiry sweep interval.
	ApprovalSweep time.Duration
}

// daemon is a fully wired bosund instance.
type daemon struct {
	cfg          *config.Config
	log          *slog.Logger
	store        *store.Store
	auth         *auth.Service
	bus          *events.Bus
	integrations []integration.Integration
	handler      http.Handler
}

// newDaemon opens the store and wires every component. The returned daemon
// must be closed. Background workers (approval sweep, session sweep) run
// until ctx ends.
func newDaemon(ctx context.Context, cfg *config.Config, log *slog.Logger, dp deps) (_ *daemon, err error) {
	d := &daemon{cfg: cfg, log: log}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()

	if err := ensureDataDir(cfg.Daemon.DataDir, log); err != nil {
		return nil, err
	}
	if d.store, err = store.Open(ctx, cfg.DBPath()); err != nil {
		return nil, err
	}
	ver, err := d.store.Migrate(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	log.Info("database ready", "path", cfg.DBPath(), "schema_version", ver)

	if d.auth, err = auth.New(d.store, auth.Options{Argon2: dp.Argon2}); err != nil {
		return nil, err
	}
	if ok, err := d.auth.HasUsers(ctx); err != nil {
		return nil, err
	} else if !ok {
		log.Warn("no users exist; run `bosun setup` to create the first admin")
	}

	// LLM providers. Keys are read from the environment by FromConfig and
	// never logged.
	llms := dp.LLM
	if llms == nil {
		if llms, err = providers.FromConfig(cfg); err != nil {
			return nil, err
		}
	}
	if _, _, err := llms.Resolve(cfg.LLM.Default); err != nil {
		return nil, fmt.Errorf("llm.default: %w", err)
	}
	log.Info("llm providers registered", "providers", strings.Join(llms.Names(), ","), "default_model", cfg.LLM.Default)

	// Integrations: Init, then Register.
	auditLog := audit.NewStoreLogger(d.store, audit.Options{LogContent: cfg.Audit.LogContent})
	vlt := vault.New(d.store, vault.NewFileKeySource(cfg.Daemon.DataDir))
	tools := integration.NewRegistry()
	for _, in := range d.enabledIntegrations(dp) {
		if err := d.initIntegration(ctx, tools, vlt, in); err != nil {
			return nil, err
		}
	}
	toolNames := make([]string, 0)
	for _, t := range tools.Tools() {
		toolNames = append(toolNames, t.Name)
	}
	log.Info("tools registered", "integrations", strings.Join(tools.Names(), ","), "tools", strings.Join(toolNames, ","))

	// Policy.
	defaults, err := policy.DefaultsFromConfig(cfg.Policy.Defaults)
	if err != nil {
		return nil, err
	}
	known := knownCapabilities(tools)
	engine := policy.NewEngine(d.store, policy.Options{
		Defaults: defaults,
		Known:    func(c integration.Capability) bool { return known[c] },
		Log:      log,
	})
	rep, err := policy.SeedRules(ctx, d.store, cfg.Users, policy.SeedOptions{Log: log})
	if err != nil {
		return nil, err
	}
	log.Info("policy rules seeded from config", "created", rep.Created, "unchanged", rep.Unchanged,
		"kept_db_value", rep.Kept, "missing_users", strings.Join(rep.MissingUsers, ","))

	// Approvals.
	d.bus = events.NewBus()
	appr := approval.NewManager(d.store, d.bus, approval.Options{
		Timeout: cfg.Policy.ApprovalTimeout, Audit: auditLog, Log: log,
	})
	n, err := appr.ExpireStale(ctx)
	if err != nil {
		return nil, err
	}
	log.Info("stale approvals expired", "count", n)
	sweep := dp.ApprovalSweep
	if sweep <= 0 {
		sweep = 30 * time.Second
	}
	go appr.Run(ctx, sweep)
	go sweepSessions(ctx, d.store, log)

	// Dispatcher and agent.
	disp := dispatch.New(tools, engine, appr, auditLog, hooks.NewRegistry(), d.bus, dispatch.WithLogger(log))
	ag := agent.New(llms, cfg.LLM.Default, disp, tools, d.store, d.bus,
		agent.WithAudit(auditLog), agent.WithLogger(log))

	apiSrv, err := api.New(api.Options{
		Auth: d.auth, Bus: d.bus, Logger: log, Version: version,
		Conversations: d.store, Agent: ag, Approvals: appr, Policy: engine, PolicyDefaults: defaults,
		Tools: tools, Audit: auditLog,
	})
	if err != nil {
		return nil, err
	}
	d.handler = apiSrv.Handler()
	return d, nil
}

// enabledIntegrations returns the built-ins enabled in config (sorted by
// name) followed by any test-supplied integrations.
func (d *daemon) enabledIntegrations(dp deps) []integration.Integration {
	names := make([]string, 0, len(d.cfg.Integrations))
	for name, ic := range d.cfg.Integrations {
		if ic.IsEnabled() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out []integration.Integration
	for _, name := range names {
		mk, ok := builtins[name]
		if !ok {
			d.log.Warn("integration enabled in config is not available in this build; skipping", "integration", name)
			continue
		}
		out = append(out, mk())
	}
	return append(out, dp.Integrations...)
}

func (d *daemon) initIntegration(ctx context.Context, reg *integration.Registry, vlt *vault.Vault, in integration.Integration) error {
	m := in.Manifest()
	settings, err := d.cfg.Integrations[m.Name].SettingsJSON()
	if err != nil {
		return fmt.Errorf("integration %s: %w", m.Name, err)
	}
	dir := filepath.Join(d.cfg.Daemon.DataDir, "integrations", m.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("integration %s: data dir: %w", m.Name, err)
	}
	if err := in.Init(ctx, integration.Config{Name: m.Name, Settings: settings, DataDir: dir}, vlt.SourceFor(m.Name, m.Scope)); err != nil {
		return fmt.Errorf("integration %s: init: %w", m.Name, err)
	}
	// Track it before Register so Close runs even if registration fails.
	d.integrations = append(d.integrations, in)
	if err := reg.Register(in); err != nil {
		return fmt.Errorf("integration %s: register: %w", m.Name, err)
	}
	return nil
}

func knownCapabilities(reg *integration.Registry) map[integration.Capability]bool {
	known := map[integration.Capability]bool{}
	for _, name := range reg.Names() {
		if in, ok := reg.Integration(name); ok {
			for _, c := range in.Manifest().Capabilities {
				known[c] = true
			}
		}
	}
	return known
}

// Serve runs the HTTP server on ln until ctx ends, then shuts it down
// gracefully (in-flight turns are canceled via the request contexts, which
// derive from ctx).
func (d *daemon) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           d.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams are long-lived. Request contexts derive
		// from ctx so streams and turns end on shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(d.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		d.log.Info("bosund listening", "addr", ln.Addr().String(), "tls", d.cfg.Daemon.TLS.Enabled, "version", version)
		if d.cfg.Daemon.TLS.Enabled {
			errc <- srv.ServeTLS(ln, d.cfg.Daemon.TLS.CertFile, d.cfg.Daemon.TLS.KeyFile)
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
	d.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// Close releases integrations and then the store. It is safe on a partially
// built daemon.
func (d *daemon) Close() {
	for i := len(d.integrations) - 1; i >= 0; i-- {
		in := d.integrations[i]
		if err := in.Close(); err != nil {
			d.log.Warn("integration close", "integration", in.Manifest().Name, "err", err)
		}
	}
	d.integrations = nil
	if d.store != nil {
		if err := d.store.Close(); err != nil {
			d.log.Warn("store close", "err", err)
		}
		d.store = nil
	}
}
