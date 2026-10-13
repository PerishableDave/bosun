package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const example = `
daemon:
  listen: 127.0.0.1:7777
  data_dir: ~/.bosun
  tls: { enabled: false }

llm:
  default: anthropic/claude-sonnet-5-5
  providers:
    anthropic: { api_key_env: ANTHROPIC_API_KEY }
    ollama:    { base_url: http://localhost:11434 }

integrations:
  calendar:  { enabled: true, backend: caldav }
  email:     { enabled: true, backend: imap }
  notes:     { enabled: true, dir: ~/Notes }
  websearch: { enabled: true, backend: searxng, base_url: http://localhost:8080 }
  mcp:
    servers:
      - name: github
        command: ["npx", "-y", "@modelcontextprotocol/server-github"]

policy:
  defaults: { read: allow, write: ask, destructive: ask }
  approval_timeout: 15m

users:
  dave:
    rules:
      calendar.read: allow
      email.delete: deny

routines:
  - name: morning-brief
    owner: dave
    trigger: { cron: "0 7 * * 1-5" }
    prompt: "Summarize today's calendar."
    model: claude-haiku-5-5
    deliver: [web, cli]

hooks:
  script_hooks: { enabled: false }

telemetry: { enabled: false }
`

func TestParseExample(t *testing.T) {
	cfg, err := Parse([]byte(example))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".bosun"); cfg.Daemon.DataDir != want {
		t.Errorf("data_dir = %q, want %q", cfg.Daemon.DataDir, want)
	}
	if got := cfg.Integrations["notes"].Settings["dir"]; got != filepath.Join(home, "Notes") {
		t.Errorf("notes.dir = %v, want expanded", got)
	}
	if got := cfg.Integrations["websearch"].Settings["base_url"]; got != "http://localhost:8080" {
		t.Errorf("websearch.base_url = %v", got)
	}
	if !cfg.Integrations["mcp"].IsEnabled() {
		t.Error("mcp without enabled key should be enabled")
	}
	if cfg.Policy.ApprovalTimeout != 15*time.Minute {
		t.Errorf("approval_timeout = %v", cfg.Policy.ApprovalTimeout)
	}
	if cfg.LLM.Providers["ollama"].BaseURL != "http://localhost:11434" {
		t.Errorf("ollama base_url = %q", cfg.LLM.Providers["ollama"].BaseURL)
	}
	if len(cfg.Routines) != 1 || cfg.Routines[0].Trigger.Cron == "" {
		t.Errorf("routines = %+v", cfg.Routines)
	}
	js, err := cfg.Integrations["mcp"].SettingsJSON()
	if err != nil || !strings.Contains(string(js), `"servers"`) {
		t.Errorf("SettingsJSON = %s, %v", js, err)
	}
}

func TestEmptyFileUsesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.Listen != DefaultListen || cfg.LLM.Default != DefaultModel {
		t.Errorf("defaults not applied: %+v", cfg.Daemon)
	}
	if cfg.Policy.Defaults.Write != "ask" || cfg.Policy.ApprovalTimeout != DefaultApprovalTimeout {
		t.Errorf("policy defaults not applied: %+v", cfg.Policy)
	}
	if strings.HasPrefix(cfg.Daemon.DataDir, "~") {
		t.Errorf("data_dir not expanded: %q", cfg.Daemon.DataDir)
	}
}

func TestDefaultValidates(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate(): %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"lan without tls":  "daemon: { listen: 0.0.0.0:7777 }",
		"bad listen":       "daemon: { listen: nope }",
		"bad decision":     "policy: { defaults: { write: maybe } }",
		"bad model ref":    "llm: { default: claude }",
		"unknown provider": "llm: { default: openai/gpt-x }",
		"tls without cert": "daemon: { listen: 0.0.0.0:7777, tls: { enabled: true } }",
		"routine trigger":  "routines: [ { name: a, owner: b, prompt: c, trigger: {} } ]",
		"routine deliver":  "routines: [ { name: a, owner: b, prompt: c, trigger: {cron: x}, deliver: [sms] } ]",
		"unknown field":    "daemon: { lisen: 127.0.0.1:1 }",
		"bad user rule":    "users: { dave: { rules: { email.send: yes } } }",
		"negative timeout": "policy: { approval_timeout: -1m }",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y)); err == nil {
				t.Fatalf("expected error for %q", y)
			}
		})
	}
}

func TestLANWithTLSOK(t *testing.T) {
	y := "daemon: { listen: 0.0.0.0:7777, tls: { enabled: true, cert_file: ~/c.pem, key_file: ~/k.pem } }"
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if strings.HasPrefix(cfg.Daemon.TLS.CertFile, "~") {
		t.Errorf("cert_file not expanded")
	}
	if cfg.BaseURL() != "https://127.0.0.1:7777" {
		t.Errorf("BaseURL = %q", cfg.BaseURL())
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", FileName)
	if err := Save(path, Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Policy.ApprovalTimeout != DefaultApprovalTimeout {
		t.Errorf("timeout = %v", cfg.Policy.ApprovalTimeout)
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"~":        home,
		"~/x":      filepath.Join(home, "x"),
		"/abs":     "/abs",
		"rel":      "rel",
		"~other/x": "~other/x",
		"":         "",
	} {
		got, err := ExpandPath(in)
		if err != nil || got != want {
			t.Errorf("ExpandPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
