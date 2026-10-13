// Package mcp is the MCP bridge: an integration that connects to external
// Model Context Protocol servers (stdio subprocesses or streamable HTTP
// endpoints) and exposes their tools as Bosun tools named
// "mcp.<server>.<tool>".
//
// Every bridged tool requires approval by default (RiskWrite) unless the
// server annotates it readOnlyHint, and every result is Untrusted. Only the
// tools part of MCP is implemented; resources, prompts and sampling are not.
//
// This is the one integration allowed to import os/exec (see
// internal/integrations/integrations_test.go): stdio MCP servers are
// subprocesses by definition. Children get a minimal environment (PATH, HOME,
// LANG and explicitly mapped variables), never the daemon's full environment.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// Settings is the "mcp" section of bosun.yaml.
type Settings struct {
	Servers []ServerConfig `json:"servers"`
}

// ServerConfig configures one MCP server. Exactly one of Command (stdio) or
// URL (streamable HTTP) is set.
type ServerConfig struct {
	// Name becomes the capability prefix "mcp.<name>."; [a-z0-9_-], at most
	// 32 characters, no "__".
	Name string `json:"name"`

	// Command is argv of a stdio server.
	Command []string `json:"command,omitempty"`
	// Env maps a child environment variable name to the daemon environment
	// variable whose value it receives. The child otherwise only gets PATH,
	// HOME and LANG.
	Env map[string]string `json:"env,omitempty"`
	// Cwd is the child's working directory (default: the daemon's).
	Cwd string `json:"cwd,omitempty"`

	// URL is the endpoint of a streamable-HTTP server.
	URL string `json:"url,omitempty"`
	// Credential is the vault credential kind holding a bearer token for
	// URL: "bearer" or "bearer:<suffix>" (use a suffix when several HTTP
	// servers need different tokens). Empty means no Authorization header.
	Credential string `json:"credential,omitempty"`

	// Timeout bounds each tools/call (Go duration, default 60s).
	Timeout string `json:"timeout,omitempty"`
}

func (s ServerConfig) transportName() string {
	if s.URL != "" {
		return "http"
	}
	return "stdio"
}

// Server states reported by Status.
const (
	StateReady  = "ready"
	StateFailed = "failed" // never connected
	StateExited = "exited" // connected, then the server went away
	StateClosed = "closed"
)

// ServerStatus describes one configured server.
type ServerStatus struct {
	Name            string `json:"name"`
	Transport       string `json:"transport"` // "stdio" | "http"
	State           string `json:"state"`
	Error           string `json:"error,omitempty"`
	Tools           int    `json:"tools"`
	ServerName      string `json:"server_name,omitempty"`
	ServerVersion   string `json:"server_version,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
}

const (
	defaultCallTimeout    = 60 * time.Second
	defaultConnectTimeout = 30 * time.Second
	defaultKillGrace      = 3 * time.Second
	maxDescription        = 2000
)

// Option configures an Integration.
type Option func(*Integration)

// WithLogger sets the logger (default: discard).
func WithLogger(l *slog.Logger) Option { return func(i *Integration) { i.log = l } }

// WithClientVersion sets the clientInfo.version sent in initialize.
func WithClientVersion(v string) Option { return func(i *Integration) { i.clientVersion = v } }

// Integration implements integration.Integration.
//
// v1 is ScopeShared: one connection per server, shared by every user, and the
// HTTP bearer token is the household-wide credential. Per-user MCP servers
// (e.g. each user's own GitHub token) would need ScopePerUser and a
// connection per (server, user), opened lazily on first call.
type Integration struct {
	log           *slog.Logger
	clientVersion string

	// Test hooks.
	getenv         func(string) string
	connectTimeout time.Duration
	killGrace      time.Duration

	mu        sync.RWMutex
	servers   []*server
	tools     []integration.Tool
	caps      []integration.Capability
	credKinds []string
	closed    bool
}

type server struct {
	cfg     ServerConfig
	timeout time.Duration
	t       transport // nil if connecting failed
	initErr error
	tools   int
	info    initializeResult
	// originals maps the sanitized segment back to the server's tool name.
	originals map[string]string
	remote    []remoteTool // discovered tools, consumed by Init
	// TODO: a stdio server that exits is not restarted; calls fail with
	// "mcp server <name> is not running" until the daemon restarts.
}

var _ integration.Integration = (*Integration)(nil)
var _ integration.StatusReporter = (*Integration)(nil)

// New returns an uninitialised MCP bridge.
func New(opts ...Option) *Integration {
	i := &Integration{
		log: slog.New(slog.DiscardHandler), clientVersion: "dev",
		getenv: os.Getenv, connectTimeout: defaultConnectTimeout, killGrace: defaultKillGrace,
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

const configSchema = `{"type":"object","additionalProperties":false,"properties":{"servers":{"type":"array","items":{` +
	`"type":"object","additionalProperties":false,"required":["name"],"properties":{` +
	`"name":{"type":"string","pattern":"^[a-z0-9][a-z0-9_-]*$","maxLength":32},` +
	`"command":{"type":"array","items":{"type":"string"},"minItems":1},` +
	`"env":{"type":"object","additionalProperties":{"type":"string"}},` +
	`"cwd":{"type":"string"},` +
	`"url":{"type":"string"},` +
	`"credential":{"type":"string","pattern":"^bearer(:[a-z0-9_-]+)?$"},` +
	`"timeout":{"type":"string","description":"Go duration bounding each tool call, default 60s"}}}}}}`

// Manifest implements integration.Integration. Capabilities are empty until
// Init has discovered the servers' tools.
func (i *Integration) Manifest() integration.Manifest {
	i.mu.RLock()
	defer i.mu.RUnlock()
	kinds := i.credKinds
	if kinds == nil {
		kinds = []string{"bearer"}
	}
	return integration.Manifest{
		Name:            "mcp",
		Version:         "0.1.0",
		Scope:           integration.ScopeShared,
		Capabilities:    slices.Clone(i.caps),
		ConfigSchema:    json.RawMessage(configSchema),
		CredentialKinds: slices.Clone(kinds),
	}
}

// Tools implements integration.Integration.
func (i *Integration) Tools() []integration.Tool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return slices.Clone(i.tools)
}

var (
	envNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	credentialRe = regexp.MustCompile(`^bearer(:[a-z0-9_-]+)?$`)
)

// parseSettings decodes and validates the config strictly.
func parseSettings(cfg integration.Config) (Settings, error) {
	var s Settings
	if len(cfg.Settings) > 0 {
		dec := json.NewDecoder(bytes.NewReader(cfg.Settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return Settings{}, fmt.Errorf("mcp: decode config: %w", err)
		}
	}
	var errs []error
	seen := map[string]bool{}
	for idx, sc := range s.Servers {
		where := fmt.Sprintf("mcp.servers[%d]", idx)
		if sc.Name != "" {
			where = "mcp.servers." + sc.Name
		}
		add := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf(where+": "+format, args...))
		}
		if !validServerName(sc.Name) {
			add("name %q must match [a-z0-9][a-z0-9_-]*, be at most %d characters and not contain \"__\"", sc.Name, maxServerName)
		}
		if seen[sc.Name] {
			add("duplicate server name")
		}
		seen[sc.Name] = true
		switch {
		case len(sc.Command) > 0 && sc.URL != "":
			add("set either command or url, not both")
		case len(sc.Command) == 0 && sc.URL == "":
			add("one of command or url is required")
		case len(sc.Command) > 0:
			if sc.Command[0] == "" {
				add("command[0] is empty")
			}
			if sc.Credential != "" {
				add("credential applies to url servers only; pass secrets to stdio servers with env")
			}
			for k, v := range sc.Env {
				if !envNameRe.MatchString(k) || !envNameRe.MatchString(v) {
					add("env %q: %q: both must be environment variable names", k, v)
				}
			}
		default:
			if len(sc.Env) > 0 || sc.Cwd != "" {
				add("env and cwd apply to command servers only")
			}
			u, err := url.Parse(sc.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("url must be an http(s) URL")
			} else if sc.Credential != "" && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
				add("refusing to send a bearer token over plain http to a non-loopback host")
			}
			if sc.Credential != "" && !credentialRe.MatchString(sc.Credential) {
				add("credential %q must be \"bearer\" or \"bearer:<suffix>\"", sc.Credential)
			}
		}
		if sc.Timeout != "" {
			if d, err := time.ParseDuration(sc.Timeout); err != nil || d <= 0 {
				add("timeout %q must be a positive Go duration like 30s", sc.Timeout)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Init implements integration.Integration. Invalid config is an error; a
// server that cannot be started or handshaken is logged, skipped, and
// reported by Status.
func (i *Integration) Init(ctx context.Context, cfg integration.Config, creds integration.CredentialSource) error {
	s, err := parseSettings(cfg)
	if err != nil {
		return err
	}
	servers := make([]*server, len(s.Servers))
	var wg sync.WaitGroup
	for idx, sc := range s.Servers {
		srv := &server{cfg: sc, timeout: defaultCallTimeout}
		if sc.Timeout != "" {
			srv.timeout, _ = time.ParseDuration(sc.Timeout)
		}
		servers[idx] = srv
		wg.Add(1)
		go func() {
			defer wg.Done()
			i.connect(ctx, srv, creds)
		}()
	}
	wg.Wait()

	var (
		tools []integration.Tool
		caps  []integration.Capability
		kinds []string
	)
	for _, srv := range servers {
		if srv.cfg.Credential != "" && !slices.Contains(kinds, srv.cfg.Credential) {
			kinds = append(kinds, srv.cfg.Credential)
		}
		if srv.initErr != nil {
			i.log.Warn("mcp server unavailable; skipping", "server", srv.cfg.Name, "transport", srv.cfg.transportName(), "err", srv.initErr)
			continue
		}
		ts := i.buildTools(srv, srv.remote)
		srv.remote = nil
		srv.tools = len(ts)
		for _, t := range ts {
			tools = append(tools, t)
			caps = append(caps, t.Capability)
		}
		i.log.Info("mcp server ready", "server", srv.cfg.Name, "transport", srv.cfg.transportName(),
			"server_name", srv.info.ServerInfo.Name, "protocol", srv.info.ProtocolVersion, "tools", len(ts))
	}
	if kinds == nil {
		kinds = []string{}
	}

	i.mu.Lock()
	i.servers, i.tools, i.caps, i.credKinds = servers, tools, caps, kinds
	i.mu.Unlock()
	return nil
}

// connect starts the transport, handshakes and lists tools, recording the
// outcome on srv.
func (i *Integration) connect(ctx context.Context, srv *server, creds integration.CredentialSource) {
	ctx, cancel := context.WithTimeout(ctx, i.connectTimeout)
	defer cancel()
	sc := srv.cfg
	var t transport
	if sc.URL != "" {
		var token func(context.Context) (string, error)
		if sc.Credential != "" {
			kind := sc.Credential
			token = func(ctx context.Context) (string, error) {
				if creds == nil {
					return "", fmt.Errorf("no credential source for %s", kind)
				}
				c, err := creds.Get(ctx, kind)
				if err != nil {
					return "", fmt.Errorf("bearer token (vault kind %q for integration mcp): %w", kind, err)
				}
				return c.Secret, nil
			}
		}
		t = newHTTPTransport(sc.Name, sc.URL, token, i.log)
	} else {
		dir, err := expandHome(sc.Cwd)
		if err != nil {
			srv.initErr = err
			return
		}
		st, err := startStdio(sc.Name, sc.Command, i.childEnv(sc), dir, i.killGrace, i.log)
		if err != nil {
			srv.initErr = err
			return
		}
		t = st
	}
	info, err := handshake(ctx, t, i.clientVersion)
	if err == nil {
		srv.info = info
		srv.remote, err = listTools(ctx, t)
	}
	if err != nil {
		t.close()
		srv.initErr = err
		return
	}
	srv.t = t
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// childEnv builds the minimal environment for a stdio server.
func (i *Integration) childEnv(sc ServerConfig) []string {
	env := []string{}
	for _, k := range []string{"PATH", "HOME", "LANG"} {
		if _, mapped := sc.Env[k]; mapped {
			continue
		}
		if v := i.getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	keys := make([]string, 0, len(sc.Env))
	for k := range sc.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := i.getenv(sc.Env[k])
		if v == "" {
			i.log.Warn("mcp: mapped environment variable is unset in the daemon", "server", sc.Name, "child_var", k, "daemon_var", sc.Env[k])
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

// riskFor maps tool annotations to a Risk: read only when the server says
// readOnlyHint, destructive when it says destructiveHint, otherwise write (so
// unannotated tools need approval under the default policy).
func riskFor(rt remoteTool) integration.Risk {
	a := rt.Annotations
	switch {
	case a != nil && a.ReadOnlyHint != nil && *a.ReadOnlyHint:
		return integration.RiskRead
	case a != nil && a.DestructiveHint != nil && *a.DestructiveHint:
		return integration.RiskDestructive
	default:
		return integration.RiskWrite
	}
}

func (i *Integration) buildTools(srv *server, remote []remoteTool) []integration.Tool {
	namer := newToolNamer(srv.cfg.Name)
	srv.originals = map[string]string{}
	var out []integration.Tool
	for _, rt := range remote {
		if rt.Name == "" {
			continue
		}
		seg := namer.name(rt.Name)
		srv.originals[seg] = rt.Name
		full := "mcp." + srv.cfg.Name + "." + seg
		desc := rt.Description
		if desc == "" {
			desc = rt.Title
		}
		if len(desc) > maxDescription {
			desc = desc[:maxDescription] + "..."
		}
		desc = fmt.Sprintf("[third-party MCP server %q, tool %q] %s", srv.cfg.Name, rt.Name, desc)
		out = append(out, integration.Tool{
			Name:        full,
			Description: strings.TrimSpace(desc),
			Capability:  integration.Capability(full),
			Risk:        riskFor(rt),
			InputSchema: inputSchema(rt.InputSchema),
			Handler:     i.handler(srv, rt.Name),
		})
	}
	return out
}

// inputSchema passes the server's schema through if it is a JSON object, else
// accepts any object.
func inputSchema(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

func (i *Integration) handler(srv *server, original string) integration.Handler {
	return func(ctx context.Context, in json.RawMessage) (integration.Result, error) {
		args := bytes.TrimSpace(in)
		if len(args) == 0 || bytes.Equal(args, []byte("null")) {
			args = []byte("{}")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(args, &obj); err != nil {
			return integration.Result{}, fmt.Errorf("arguments must be a JSON object: %w", err)
		}
		if err := srv.t.err(); err != nil {
			return integration.Result{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, srv.timeout)
		defer cancel()
		res, err := callTool(ctx, srv.t, original, args)
		if err != nil {
			return integration.Result{}, err
		}
		text := renderContent(res)
		if res.IsError {
			if text == "" {
				text = "tool reported an error"
			}
			return integration.Result{}, fmt.Errorf("mcp server %s tool %s: %s", srv.cfg.Name, original, text)
		}
		r := integration.Result{Content: text, Untrusted: true}
		if len(res.StructuredContent) > 0 {
			r.Data = res.StructuredContent
		}
		return r, nil
	}
}

// Servers reports every configured server's state, in config order.
func (i *Integration) Servers() []ServerStatus {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]ServerStatus, 0, len(i.servers))
	for _, srv := range i.servers {
		st := ServerStatus{Name: srv.cfg.Name, Transport: srv.cfg.transportName(), Tools: srv.tools,
			ServerName: srv.info.ServerInfo.Name, ServerVersion: srv.info.ServerInfo.Version,
			ProtocolVersion: srv.info.ProtocolVersion}
		switch {
		case srv.initErr != nil:
			st.State, st.Error = StateFailed, srv.initErr.Error()
		case i.closed:
			st.State = StateClosed
		case srv.t.err() != nil:
			st.State, st.Error = StateExited, srv.t.err().Error()
		default:
			st.State = StateReady
		}
		out = append(out, st)
	}
	return out
}

// Status implements integration.StatusReporter: "ok" when every server is
// ready, "degraded" when some are not, "down" when none are.
func (i *Integration) Status() integration.Status {
	servers := i.Servers()
	ready := 0
	var bad []string
	for _, s := range servers {
		if s.State == StateReady {
			ready++
		} else {
			bad = append(bad, s.Name+" "+s.State)
		}
	}
	st := integration.Status{State: integration.StatusOK, Details: servers}
	switch {
	case len(servers) == 0:
		st.Message = "no MCP servers configured"
	case ready == len(servers):
		st.Message = fmt.Sprintf("%d server(s) ready", ready)
	case ready == 0:
		st.State, st.Message = integration.StatusDown, strings.Join(bad, ", ")
	default:
		st.State, st.Message = integration.StatusDegraded, strings.Join(bad, ", ")
	}
	return st
}

// Close implements integration.Integration: subprocesses get SIGTERM, then
// SIGKILL after 3s; HTTP sessions are ended.
func (i *Integration) Close() error {
	i.mu.Lock()
	servers := i.servers
	i.closed = true
	i.mu.Unlock()
	var wg sync.WaitGroup
	for _, srv := range servers {
		if srv.t == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.t.close()
		}()
	}
	wg.Wait()
	return nil
}
