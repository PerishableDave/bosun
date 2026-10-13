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
	"slices"
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
	"github.com/dperedo/bosun/internal/integrations/mcp"
	"github.com/dperedo/bosun/internal/integrations/notes"
	"github.com/dperedo/bosun/internal/integrations/websearch"
	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/providers"
	"github.com/dperedo/bosun/internal/memory"
	"github.com/dperedo/bosun/internal/policy"
	"github.com/dperedo/bosun/internal/routines"
	"github.com/dperedo/bosun/internal/store"
	"github.com/dperedo/bosun/internal/vault"
	"github.com/dperedo/bosun/internal/webui"
)

// builtins are the first-party integrations that can be enabled under
// `integrations:` in bosun.yaml.
var builtins = map[string]func(log *slog.Logger) integration.Integration{
	"websearch": func(*slog.Logger) integration.Integration { return websearch.New() },
	"mcp": func(log *slog.Logger) integration.Integration {
		return mcp.New(mcp.WithLogger(log), mcp.WithClientVersion(version))
	},
	"notes": func(*slog.Logger) integration.Integration { return notes.New() },
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
	// RoutineTick overrides how often due cron routines are checked.
	RoutineTick time.Duration
}

// daemon is a fully wired bosund instance.
type daemon struct {
	cfg          *config.Config
	log          *slog.Logger
	store        *store.Store
	auth         *auth.Service
	bus          *events.Bus
	integrations []integration.Integration
	agent        *agent.Agent
	handler      http.Handler

	// Routines: hooks carries on_event subscriptions (integrations emit
	// events with hooks.EmitEvent); sched runs routines.
	hooks    *hooks.Registry
	routines *routines.Store
	sched    *routines.Scheduler
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
	keys, keyDesc, err := vault.SelectKeySource(ctx, vault.KeySourceOptions{
		Mode: cfg.Daemon.MasterKey, DataDir: cfg.Daemon.DataDir, Detect: detectKeychain, Log: log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("vault master key ready", "source", keyDesc)
	vlt := vault.New(d.store, keys)
	oauthMgr, err := d.setupOAuth(vlt, auditLog)
	if err != nil {
		return nil, err
	}
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

	// Hooks.
	hk, err := buildHooks(cfg, auditLog, log)
	if err != nil {
		return nil, err
	}

	// Approvals.
	d.bus = events.NewBus()
	appr := approval.NewManager(d.store, d.bus, approval.Options{
		Timeout: cfg.Policy.ApprovalTimeout, Audit: auditLog, Log: log, OnResolve: onApprovalHooks(hk),
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
	d.hooks = hk
	disp := dispatch.New(tools, engine, appr, auditLog, hk, d.bus, dispatch.WithLogger(log))
	agentOpts := []agent.Option{agent.WithAudit(auditLog), agent.WithLogger(log), agent.WithBaseContext(ctx), agent.WithHooks(hk)}
	mem := memory.NewStore(d.store)
	if cfg.Memory.IsEnabled() {
		extProv, extModel, err := llms.Resolve(cfg.ExtractModelRef())
		if err != nil {
			return nil, fmt.Errorf("memory.extract_model: %w", err)
		}
		ext := memory.NewExtractor(mem, extProv, extModel,
			memory.WithExtractLogger(log), memory.WithExtractAudit(auditLog))
		agentOpts = append(agentOpts, agent.WithMemory(mem, ext), agent.WithRecallLimit(cfg.Memory.RecallLimit))
		log.Info("memory enabled", "extract_model", cfg.ExtractModelRef(), "recall_limit", cfg.Memory.RecallLimit)
	} else {
		log.Info("memory disabled; stored memories stay editable through the API")
	}
	d.agent = agent.New(llms, cfg.LLM.Default, disp, tools, d.store, d.bus, agentOpts...)
	if err := d.setupRoutines(ctx, dp); err != nil {
		return nil, err
	}

	var webHandler http.Handler
	if cfg.Daemon.WebUIEnabled() {
		webHandler = webui.Handler()
	}
	initialised := slices.Clone(d.integrations) // Close mutates d.integrations
	apiSrv, err := api.New(api.Options{
		Auth: d.auth, Bus: d.bus, Logger: log, Version: version,
		Conversations: d.store, Agent: d.agent, Approvals: appr, Policy: engine, PolicyDefaults: defaults,
		Tools: tools, Audit: auditLog, Memories: mem, WebUI: webHandler,
		Integrations: func() []integration.Integration { return initialised },
		Routines:     routines.Service{Store: d.routines, Scheduler: d.sched},
		Hooks:        hk,
		Credentials:  vlt,
		OAuth:        oauthMgr,
	})
	if err != nil {
		return nil, err
	}
	d.handler = apiSrv.Handler()
	go d.sched.Run(ctx)
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
		in := mk(d.log)
		if n, ok := in.(*notes.Integration); ok {
			n.SetUserDirFunc(d.username) // per-user notes live in <dir>/<username>/
		}
		out = append(out, in)
	}
	return append(out, dp.Integrations...)
}

// username returns a user's username (immutable and path-safe; see
// auth.ValidateUsername).
func (d *daemon) username(ctx context.Context, userID int64) (string, error) {
	u, err := d.store.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	return u.Username, nil
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
	if len(in.Tools()) == 0 && len(in.Manifest().Capabilities) == 0 {
		// Integrations that discover tools at runtime (the MCP bridge) may
		// end up with none, e.g. every server failed to start. That is not
		// fatal; it stays listed by GET /v1/integrations with its status.
		d.log.Warn("integration has no tools; not registering", "integration", m.Name)
		return nil
	}
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
	if d.cfg.Daemon.TLS.Enabled {
		if err := logTLSCert(d.log, d.cfg.Daemon.TLS.CertFile); err != nil {
			return err
		}
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

// Close cancels and waits for the agent's background work (memory
// extraction), then releases integrations and the store. It is safe on a
// partially built daemon.
func (d *daemon) Close() {
	if d.sched != nil {
		// In-flight routine runs get a few seconds to finish, then are
		// canceled (and recorded as canceled).
		d.sched.Close(routineShutdownGrace)
		d.sched = nil
	}
	if d.agent != nil {
		d.agent.Close()
		d.agent = nil
	}
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
