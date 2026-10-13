// Package config loads, defaults, and validates bosun.yaml, the single
// daemon-level configuration file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of bosun.yaml.
type Config struct {
	Daemon       DaemonConfig                 `yaml:"daemon"`
	LLM          LLMConfig                    `yaml:"llm"`
	Integrations map[string]IntegrationConfig `yaml:"integrations,omitempty"`
	Policy       PolicyConfig                 `yaml:"policy"`
	Users        map[string]UserSeed          `yaml:"users,omitempty"`
	Routines     []RoutineConfig              `yaml:"routines,omitempty"`
	Hooks        HooksConfig                  `yaml:"hooks"`
	Audit        AuditConfig                  `yaml:"audit"`
	Telemetry    TelemetryConfig              `yaml:"telemetry"`
	Memory       MemoryConfig                 `yaml:"memory,omitempty"`
	// OAuth configures the OAuth2 clients behind `bosun connect <provider>`,
	// keyed by provider name ("google", "microsoft").
	OAuth map[string]OAuthProviderConfig `yaml:"oauth,omitempty"`

	// Channels configures chat-app frontends (Telegram, ...).
	Channels ChannelsConfig `yaml:"channels,omitempty"`
}

// OAuthProviderConfig is one OAuth2 client registration. The client secret is
// never written in bosun.yaml: it comes from the environment variable named
// by ClientSecretEnv, else from the shared vault credential
// oauth/client_secret:<provider>. Public clients (Microsoft) need none.
type OAuthProviderConfig struct {
	ClientID        string `yaml:"client_id"`
	ClientSecretEnv string `yaml:"client_secret_env,omitempty"`
	// Endpoint overrides (advanced: e.g. a single-tenant Microsoft
	// authority, or a test identity provider). Empty means the built-in.
	AuthURL     string `yaml:"auth_url,omitempty"`
	TokenURL    string `yaml:"token_url,omitempty"`
	UserinfoURL string `yaml:"userinfo_url,omitempty"`
	RevokeURL   string `yaml:"revoke_url,omitempty"`
}

// DaemonConfig controls how bosund listens and where it keeps data.
type DaemonConfig struct {
	Listen  string    `yaml:"listen"`
	DataDir string    `yaml:"data_dir"`
	TLS     TLSConfig `yaml:"tls"`
	// WebUI serves the browser UI at / (default true).
	WebUI *bool `yaml:"web_ui,omitempty"`

	// Timezone is the IANA zone (e.g. "Europe/Madrid") routine cron
	// schedules are evaluated in; empty means the system's local zone.
	Timezone string `yaml:"timezone,omitempty"`

	// MasterKey selects where the vault master key lives: "auto" (default:
	// the OS keychain if available, else $data_dir/master.key), "file", or
	// "keychain".
	MasterKey string `yaml:"master_key,omitempty"`
}

// Location returns the time zone named by daemon.timezone (time.Local when
// empty).
func (d DaemonConfig) Location() (*time.Location, error) {
	if d.Timezone == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return nil, fmt.Errorf("daemon.timezone %q: %w", d.Timezone, err)
	}
	return loc, nil
}

// WebUIEnabled reports whether the web UI is mounted.
func (d DaemonConfig) WebUIEnabled() bool { return d.WebUI == nil || *d.WebUI }

// TLSConfig configures HTTPS. TLS is required when listening on a
// non-loopback address.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

// LLMConfig selects the default model and configures providers.
type LLMConfig struct {
	// Default is "<provider>/<model>", e.g. "anthropic/claude-sonnet-5-5".
	Default   string                    `yaml:"default"`
	Providers map[string]ProviderConfig `yaml:"providers,omitempty"`
}

// ProviderConfig holds settings for one LLM provider. Unknown keys are kept in
// Options for provider-specific use.
type ProviderConfig struct {
	APIKeyEnv string         `yaml:"api_key_env,omitempty"`
	BaseURL   string         `yaml:"base_url,omitempty"`
	Options   map[string]any `yaml:",inline"`
}

// IntegrationConfig is the admin-level configuration of one integration. An
// integration section that omits "enabled" is treated as enabled. All other
// keys are integration-specific and kept in Settings.
type IntegrationConfig struct {
	Enabled  *bool          `yaml:"enabled,omitempty"`
	Settings map[string]any `yaml:",inline"`
}

// IsEnabled reports whether the integration is enabled.
func (c IntegrationConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// SettingsJSON returns Settings encoded as JSON, suitable for
// integration.Config and validation against a Manifest.ConfigSchema.
func (c IntegrationConfig) SettingsJSON() (json.RawMessage, error) {
	if len(c.Settings) == 0 {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(c.Settings)
	if err != nil {
		return nil, fmt.Errorf("encode integration settings: %w", err)
	}
	return b, nil
}

// PolicyConfig holds risk-level defaults and the approval timeout.
type PolicyConfig struct {
	Defaults        PolicyDefaults `yaml:"defaults"`
	ApprovalTimeout time.Duration  `yaml:"approval_timeout"`
}

// PolicyDefaults maps risk levels to "allow" | "ask" | "deny".
type PolicyDefaults struct {
	Read        string `yaml:"read"`
	Write       string `yaml:"write"`
	Destructive string `yaml:"destructive"`
}

// UserSeed is the config-file form of per-user policy rules for an
// admin-seeded user. The DB remains the source of truth once seeded.
type UserSeed struct {
	Rules map[string]string `yaml:"rules,omitempty"`
}

// RoutineConfig declares a routine in config form.
type RoutineConfig struct {
	Name    string         `yaml:"name"`
	Owner   string         `yaml:"owner"`
	Trigger RoutineTrigger `yaml:"trigger"`
	Prompt  string         `yaml:"prompt"`
	Model   string         `yaml:"model,omitempty"`
	Deliver []string       `yaml:"deliver,omitempty"`
}

// RoutineTrigger is either a cron expression or an event name.
type RoutineTrigger struct {
	Cron  string `yaml:"cron,omitempty"`
	Event string `yaml:"event,omitempty"`
}

// HooksConfig configures hooks.
type HooksConfig struct {
	ScriptHooks ScriptHooksConfig `yaml:"script_hooks"`
	// RedactSecrets registers the built-in pre_model_call hook that masks
	// API keys and tokens in outgoing model requests (default true).
	RedactSecrets *bool `yaml:"redact_secrets,omitempty"`
}

// RedactSecretsEnabled reports whether the built-in secret redaction hook is
// on.
func (h HooksConfig) RedactSecretsEnabled() bool { return h.RedactSecrets == nil || *h.RedactSecrets }

// ScriptHooksConfig gates user-supplied executable hooks (off by default).
// Scripts are config-only: there is no API to add them.
type ScriptHooksConfig struct {
	Enabled bool               `yaml:"enabled"`
	Scripts []ScriptHookConfig `yaml:"scripts,omitempty"`
}

// ScriptHookConfig declares one script hook.
type ScriptHookConfig struct {
	// Name is optional; the default is derived from the command.
	Name string `yaml:"name,omitempty"`
	// Point is pre_tool_call | post_tool_call | pre_model_call | on_approval.
	Point string `yaml:"point"`
	// Command is the argv; Command[0] must be an absolute path (no PATH
	// lookup, no shell).
	Command []string `yaml:"command"`
	// Timeout bounds one run (default 5s, max 60s).
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// Match is an optional glob (path.Match syntax) on the tool or
	// capability name; tool hook points only.
	Match string `yaml:"match,omitempty"`
}

// Script hook timeouts.
const (
	DefaultScriptHookTimeout = 5 * time.Second
	MaxScriptHookTimeout     = 60 * time.Second
)

var validScriptPoint = map[string]bool{"pre_tool_call": true, "post_tool_call": true, "pre_model_call": true, "on_approval": true}

// AuditConfig configures the audit log.
type AuditConfig struct {
	// LogContent records LLM request/response content, not just metadata.
	LogContent bool `yaml:"log_content"`
}

// MemoryConfig configures per-user memory (recall into the system prompt and
// extraction after each turn). The whole section is optional.
type MemoryConfig struct {
	// Enabled turns memory recall and extraction on (default true).
	Enabled *bool `yaml:"enabled,omitempty"`
	// ExtractModel is the "<provider>/<model>" used for extraction; empty
	// means llm.default.
	ExtractModel string `yaml:"extract_model,omitempty"`
	// RecallLimit is how many relevance-ranked memories are recalled per
	// turn (default 8).
	RecallLimit int `yaml:"recall_limit,omitempty"`
}

// IsEnabled reports whether memory is enabled.
func (m MemoryConfig) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// ExtractModelRef returns the model reference used for extraction.
func (c *Config) ExtractModelRef() string {
	if c.Memory.ExtractModel != "" {
		return c.Memory.ExtractModel
	}
	return c.LLM.Default
}

// TelemetryConfig is opt-in telemetry (off by default).
type TelemetryConfig struct {
	Enabled bool `yaml:"enabled"`
}

// Defaults used by Default and applyDefaults.
const (
	DefaultListen          = "127.0.0.1:7777"
	DefaultDataDir         = "~/.bosun"
	DefaultModel           = "anthropic/claude-sonnet-5-5"
	DefaultApprovalTimeout = 15 * time.Minute
	DefaultRecallLimit     = 8
	FileName               = "bosun.yaml"
)

// Default returns a complete configuration with sensible defaults. Paths are
// left unexpanded (e.g. "~/.bosun") so that it round-trips to a portable file.
func Default() *Config {
	return &Config{
		Daemon: DaemonConfig{Listen: DefaultListen, DataDir: DefaultDataDir},
		LLM: LLMConfig{
			Default: DefaultModel,
			Providers: map[string]ProviderConfig{
				"anthropic": {APIKeyEnv: "ANTHROPIC_API_KEY"},
				"ollama":    {BaseURL: "http://localhost:11434"},
			},
		},
		Integrations: map[string]IntegrationConfig{},
		Policy: PolicyConfig{
			Defaults:        PolicyDefaults{Read: "allow", Write: "ask", Destructive: "ask"},
			ApprovalTimeout: DefaultApprovalTimeout,
		},
	}
}

// DefaultPath returns the default config file location, honouring
// $BOSUN_CONFIG, else ~/.bosun/bosun.yaml.
func DefaultPath() string {
	if p := os.Getenv("BOSUN_CONFIG"); p != "" {
		return p
	}
	p, err := ExpandPath(filepath.Join(DefaultDataDir, FileName))
	if err != nil {
		return FileName
	}
	return p
}

// Load reads, defaults, expands, and validates the config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes YAML bytes, applies defaults, expands paths, and validates.
func Parse(b []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	// An empty file decodes to io.EOF and means "all defaults".
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.expandPaths(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes cfg as YAML to path with mode 0600, creating parent dirs (0700).
func Save(path string, cfg *Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Daemon.Listen == "" {
		c.Daemon.Listen = d.Daemon.Listen
	}
	if c.Daemon.DataDir == "" {
		c.Daemon.DataDir = d.Daemon.DataDir
	}
	if c.LLM.Default == "" {
		c.LLM.Default = d.LLM.Default
	}
	if len(c.LLM.Providers) == 0 {
		c.LLM.Providers = d.LLM.Providers
	}
	if c.Integrations == nil {
		c.Integrations = map[string]IntegrationConfig{}
	}
	if c.Policy.Defaults.Read == "" {
		c.Policy.Defaults.Read = d.Policy.Defaults.Read
	}
	if c.Policy.Defaults.Write == "" {
		c.Policy.Defaults.Write = d.Policy.Defaults.Write
	}
	if c.Policy.Defaults.Destructive == "" {
		c.Policy.Defaults.Destructive = d.Policy.Defaults.Destructive
	}
	if c.Policy.ApprovalTimeout == 0 {
		c.Policy.ApprovalTimeout = d.Policy.ApprovalTimeout
	}
	if c.Memory.RecallLimit == 0 {
		c.Memory.RecallLimit = DefaultRecallLimit
	}
	for i := range c.Hooks.ScriptHooks.Scripts {
		if c.Hooks.ScriptHooks.Scripts[i].Timeout == 0 {
			c.Hooks.ScriptHooks.Scripts[i].Timeout = DefaultScriptHookTimeout
		}
	}
}

// pathKey reports whether an integration setting key holds a filesystem path.
func pathKey(k string) bool {
	return k == "dir" || k == "path" || k == "file" ||
		strings.HasSuffix(k, "_dir") || strings.HasSuffix(k, "_path") || strings.HasSuffix(k, "_file")
}

func (c *Config) expandPaths() error {
	var err error
	if c.Daemon.DataDir, err = ExpandPath(c.Daemon.DataDir); err != nil {
		return err
	}
	if c.Daemon.TLS.CertFile, err = ExpandPath(c.Daemon.TLS.CertFile); err != nil {
		return err
	}
	if c.Daemon.TLS.KeyFile, err = ExpandPath(c.Daemon.TLS.KeyFile); err != nil {
		return err
	}
	for i, sc := range c.Hooks.ScriptHooks.Scripts {
		if len(sc.Command) > 0 {
			if sc.Command[0], err = ExpandPath(sc.Command[0]); err != nil {
				return fmt.Errorf("hooks.script_hooks.scripts[%d].command: %w", i, err)
			}
			c.Hooks.ScriptHooks.Scripts[i] = sc
		}
	}
	for name, ic := range c.Integrations {
		for k, v := range ic.Settings {
			s, ok := v.(string)
			if !ok || !pathKey(k) {
				continue
			}
			if ic.Settings[k], err = ExpandPath(s); err != nil {
				return fmt.Errorf("integrations.%s.%s: %w", name, k, err)
			}
		}
	}
	return nil
}

// ExpandPath replaces a leading "~" or "~/" with the user's home directory.
func ExpandPath(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

var (
	nameRe        = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	validDecision = map[string]bool{"allow": true, "ask": true, "deny": true}
	validDeliver  = map[string]bool{"web": true, "cli": true}
)

// Validate checks the config for consistency. It assumes defaults have been
// applied. All problems are reported together.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	host, port, err := net.SplitHostPort(c.Daemon.Listen)
	if err != nil {
		add("daemon.listen %q: %w", c.Daemon.Listen, err)
	} else {
		if port == "" {
			add("daemon.listen %q: missing port", c.Daemon.Listen)
		}
		if !isLoopback(host) && !c.Daemon.TLS.Enabled {
			add("daemon.listen %q: non-loopback address requires daemon.tls.enabled", c.Daemon.Listen)
		}
	}
	if c.Daemon.DataDir == "" {
		add("daemon.data_dir: required")
	}
	if c.Daemon.TLS.Enabled && (c.Daemon.TLS.CertFile == "" || c.Daemon.TLS.KeyFile == "") {
		add("daemon.tls: cert_file and key_file are required when enabled (run `bosun setup --lan` to generate a self-signed pair)")
	}
	switch c.Daemon.MasterKey {
	case "", "auto", "file", "keychain":
	default:
		add("daemon.master_key %q: must be auto, file, or keychain", c.Daemon.MasterKey)
	}
	if _, err := c.Daemon.Location(); err != nil {
		errs = append(errs, err)
	}

	prov, model, ok := strings.Cut(c.LLM.Default, "/")
	if !ok || prov == "" || model == "" {
		add("llm.default %q: must be <provider>/<model>", c.LLM.Default)
	} else if _, ok := c.LLM.Providers[prov]; !ok {
		add("llm.default %q: provider %q is not configured under llm.providers", c.LLM.Default, prov)
	}
	if c.Memory.ExtractModel != "" {
		prov, model, ok := strings.Cut(c.Memory.ExtractModel, "/")
		if !ok || prov == "" || model == "" {
			add("memory.extract_model %q: must be <provider>/<model>", c.Memory.ExtractModel)
		} else if _, ok := c.LLM.Providers[prov]; !ok {
			add("memory.extract_model %q: provider %q is not configured under llm.providers", c.Memory.ExtractModel, prov)
		}
	}
	if c.Memory.RecallLimit < 0 {
		add("memory.recall_limit: must not be negative")
	}
	for name := range c.LLM.Providers {
		if !nameRe.MatchString(name) {
			add("llm.providers.%s: invalid provider name", name)
		}
	}

	for name := range c.Integrations {
		if !nameRe.MatchString(name) {
			add("integrations.%s: invalid integration name", name)
		}
	}

	for name, oc := range c.OAuth {
		if !nameRe.MatchString(name) {
			add("oauth.%s: invalid provider name", name)
		}
		if oc.ClientID == "" {
			add("oauth.%s.client_id: required", name)
		}
	}

	for risk, v := range map[string]string{
		"read": c.Policy.Defaults.Read, "write": c.Policy.Defaults.Write, "destructive": c.Policy.Defaults.Destructive,
	} {
		if !validDecision[v] {
			add("policy.defaults.%s %q: must be allow, ask, or deny", risk, v)
		}
	}
	if c.Policy.ApprovalTimeout < 0 {
		add("policy.approval_timeout: must be positive")
	}

	for user, seed := range c.Users {
		for capName, d := range seed.Rules {
			if !validDecision[d] {
				add("users.%s.rules.%s %q: must be allow, ask, or deny", user, capName, d)
			}
		}
	}

	seen := map[string]bool{}
	for i, r := range c.Routines {
		where := fmt.Sprintf("routines[%d]", i)
		if r.Name == "" {
			add("%s.name: required", where)
		} else {
			where = "routines." + r.Name
			key := r.Owner + "/" + r.Name
			if seen[key] {
				add("%s: duplicate routine for owner %q", where, r.Owner)
			}
			seen[key] = true
		}
		if r.Owner == "" {
			add("%s.owner: required", where)
		}
		if r.Prompt == "" {
			add("%s.prompt: required", where)
		}
		if (r.Trigger.Cron == "") == (r.Trigger.Event == "") {
			add("%s.trigger: exactly one of cron or event is required", where)
		}
		for _, d := range r.Deliver {
			if !validDeliver[d] {
				add("%s.deliver %q: must be web or cli", where, d)
			}
		}
	}
	errs = append(errs, c.validateScriptHooks()...)
	errs = append(errs, c.validateChannels()...)
	return errors.Join(errs...)
}

func (c *Config) validateScriptHooks() []error {
	var errs []error
	names := map[string]bool{}
	for i, s := range c.Hooks.ScriptHooks.Scripts {
		where := fmt.Sprintf("hooks.script_hooks.scripts[%d]", i)
		add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(where+format, args...)) }
		if !validScriptPoint[s.Point] {
			add(".point %q: must be pre_tool_call, post_tool_call, pre_model_call, or on_approval", s.Point)
		}
		if len(s.Command) == 0 || s.Command[0] == "" {
			add(".command: required")
		} else if !filepath.IsAbs(s.Command[0]) {
			add(".command %q: the executable must be an absolute path", s.Command[0])
		}
		if s.Timeout < 0 || s.Timeout > MaxScriptHookTimeout {
			add(".timeout %s: must be between 0 and %s", s.Timeout, MaxScriptHookTimeout)
		}
		if s.Match != "" {
			if s.Point != "pre_tool_call" && s.Point != "post_tool_call" {
				add(".match: only allowed for tool hook points")
			} else if _, err := path.Match(s.Match, ""); err != nil {
				add(".match %q: %v", s.Match, err)
			}
		}
		if s.Name != "" {
			if !nameRe.MatchString(s.Name) {
				add(".name %q: invalid", s.Name)
			} else if names[s.Name] {
				add(".name %q: duplicate", s.Name)
			}
			names[s.Name] = true
		}
	}
	return errs
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DBPath returns the SQLite database path inside the data dir.
func (c *Config) DBPath() string { return filepath.Join(c.Daemon.DataDir, "bosun.db") }

// CLITokenPath returns the CLI token file path inside the data dir.
func (c *Config) CLITokenPath() string { return filepath.Join(c.Daemon.DataDir, "cli-token") }

// BaseURL returns the URL local clients use to reach the daemon. A wildcard
// listen host (0.0.0.0, ::, or empty) is replaced with the loopback address.
func (c *Config) BaseURL() string {
	scheme := "http"
	if c.Daemon.TLS.Enabled {
		scheme = "https"
	}
	addr := c.Daemon.Listen
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return scheme + "://" + addr
}
