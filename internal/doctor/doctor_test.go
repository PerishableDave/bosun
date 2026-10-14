package doctor

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/tlsutil"
	"github.com/perishabledave/bosun/internal/vault"
)

func TestWorst(t *testing.T) {
	if Worst(nil) != OK || Worst([]Check{{Status: OK}, {Status: Warn}}) != Warn || Worst([]Check{{Status: Fail}, {Status: Warn}}) != Fail {
		t.Fatal("Worst")
	}
}

func TestDataDir(t *testing.T) {
	dir := t.TempDir()
	if c := DataDir(filepath.Join(dir, "missing")); c.Status != Fail {
		t.Errorf("missing: %+v", c)
	}
	os.Chmod(dir, 0o755)
	if c := DataDir(dir); c.Status != Warn {
		t.Errorf("0755: %+v", c)
	}
	os.Chmod(dir, 0o700)
	if c := DataDir(dir); c.Status != OK {
		t.Errorf("0700: %+v", c)
	}
}

type memKC map[string]string

func (m memKC) Name() string { return "test keychain" }
func (m memKC) Get(_ context.Context, s, a string) (string, error) {
	if v, ok := m[s+"/"+a]; ok {
		return v, nil
	}
	return "", vault.ErrKeychainItemNotFound
}
func (m memKC) Set(context.Context, string, string, string) error { return nil }
func (m memKC) Delete(context.Context, string, string) error      { return nil }

func TestMasterKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Daemon.DataDir = dir
	cfg.Daemon.MasterKey = "file"
	none := func() vault.Keychain { return nil }
	if c := MasterKey(ctx, cfg, none); c.Status != Warn {
		t.Errorf("no file yet: %+v", c)
	}
	if _, err := vault.NewFileKeySource(dir).Key(ctx); err != nil {
		t.Fatal(err)
	}
	if c := MasterKey(ctx, cfg, none); c.Status != OK {
		t.Errorf("file: %+v", c)
	}
	os.Chmod(filepath.Join(dir, vault.MasterKeyFile), 0o644)
	if c := MasterKey(ctx, cfg, none); c.Status != Fail {
		t.Errorf("0644 file: %+v", c)
	}

	cfg.Daemon.MasterKey = "keychain"
	if c := MasterKey(ctx, cfg, none); c.Status != Fail {
		t.Errorf("no keychain: %+v", c)
	}
	kc := memKC{}
	if c := MasterKey(ctx, cfg, func() vault.Keychain { return kc }); c.Status != Warn {
		t.Errorf("empty keychain, unmarked: %+v", c)
	}
	os.WriteFile(filepath.Join(dir, vault.KeySourceMarkerFile), []byte("keychain\n"), 0o600)
	if c := MasterKey(ctx, cfg, func() vault.Keychain { return kc }); c.Status != Fail {
		t.Errorf("empty keychain, marked: %+v", c)
	}
	kc[vault.DefaultKeychainService+"/"+vault.DefaultKeychainAccount] = base64.StdEncoding.EncodeToString(make([]byte, 32))
	// The key file from above is still there: a leftover copy is flagged.
	if c := MasterKey(ctx, cfg, func() vault.Keychain { return kc }); c.Status != Warn || !strings.Contains(c.Detail, "still has a copy") {
		t.Errorf("keychain with leftover file: %+v", c)
	}
	os.Remove(filepath.Join(dir, vault.MasterKeyFile))
	if c := MasterKey(ctx, cfg, func() vault.Keychain { return kc }); c.Status != OK || !strings.Contains(c.Detail, "test keychain") {
		t.Errorf("keychain: %+v", c)
	}
}

func TestTLS(t *testing.T) {
	cfg := config.Default()
	if c := TLS(cfg, time.Now()); c.Status != OK {
		t.Errorf("disabled: %+v", c)
	}
	dir := t.TempDir()
	now := time.Now()
	cert, key, err := tlsutil.GenerateSelfSigned(dir, tlsutil.SANs{DNSNames: []string{"localhost"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Daemon.TLS = config.TLSConfig{Enabled: true, CertFile: cert, KeyFile: key}
	if c := TLS(cfg, now); c.Status != OK || !strings.Contains(c.Detail, "sha256") {
		t.Errorf("valid: %+v", c)
	}
	if c := TLS(cfg, now.Add(tlsutil.Validity-24*time.Hour)); c.Status != Warn {
		t.Errorf("expiring: %+v", c)
	}
	if c := TLS(cfg, now.Add(tlsutil.Validity+24*time.Hour)); c.Status != Fail {
		t.Errorf("expired: %+v", c)
	}
}

func TestDisk(t *testing.T) {
	if c := Disk(t.TempDir()); c.Status == Fail {
		t.Errorf("disk: %+v", c)
	}
}

func TestProxy(t *testing.T) {
	cfg := config.Default()
	if c := Proxy(cfg, 0, ""); c.Status != OK {
		t.Errorf("off: %+v", c)
	}
	if c := Proxy(cfg, 3, "10.0.0.5"); c.Status != Warn || !strings.Contains(c.Detail, "behind_proxy is off") {
		t.Errorf("off with headers: %+v", c)
	}
	cfg.Daemon.BehindProxy = true
	if c := Proxy(cfg, 0, ""); c.Status != Warn || !strings.Contains(c.Detail, "trusted_proxies is unset") {
		t.Errorf("no list: %+v", c)
	}
	cfg.Daemon.TrustedProxies = []string{"127.0.0.1"}
	if c := Proxy(cfg, 0, ""); c.Status != OK {
		t.Errorf("good: %+v", c)
	}
	cfg.Daemon.Listen = "0.0.0.0:7777"
	if c := Proxy(cfg, 1, "192.168.1.2"); c.Status != Warn || !strings.Contains(c.Detail, "plain HTTP") || !strings.Contains(c.Detail, "192.168.1.2") {
		t.Errorf("exposed: %+v", c)
	}
}

func TestLLM(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("x-api-key") != "good" || r.Header.Get("anthropic-version") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"data":[]}`))
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"llama3"},{"name":"qwen"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	hc := srv.Client()
	pc := config.ProviderConfig{BaseURL: srv.URL, APIKeyEnv: "X"}

	if c := LLM(ctx, hc, "anthropic", pc, "", KeySource{}, true, true); c.Status != Fail || !strings.Contains(c.Detail, "credentials set llm anthropic") {
		t.Errorf("no key, required: %+v", c)
	}
	if c := LLM(ctx, hc, "anthropic", pc, "", KeySource{}, false, true); c.Status != Warn {
		t.Errorf("no key, optional: %+v", c)
	}
	if c := LLM(ctx, hc, "anthropic", pc, "bad", KeySource{Source: "env", Env: "X"}, true, true); c.Status != Fail || !strings.Contains(c.Detail, "rejected") {
		t.Errorf("bad key: %+v", c)
	}
	if c := LLM(ctx, hc, "anthropic", pc, "good", KeySource{Source: "vault"}, true, true); c.Status != OK || !strings.Contains(c.Detail, "vault") {
		t.Errorf("good key: %+v", c)
	}
	if c := LLM(ctx, hc, "ollama", config.ProviderConfig{BaseURL: srv.URL}, "", KeySource{}, false, true); c.Status != OK || !strings.Contains(c.Detail, "2 models") {
		t.Errorf("ollama: %+v", c)
	}
	if c := LLM(ctx, hc, "ollama", config.ProviderConfig{BaseURL: "http://127.0.0.1:1"}, "", KeySource{}, false, true); c.Status != Warn {
		t.Errorf("ollama down, optional: %+v", c)
	}
	if c := LLM(ctx, hc, "anthropic", pc, "", KeySource{Source: "vault"}, true, false); c.Status != OK {
		t.Errorf("no probe: %+v", c)
	}
}

func TestRequiredProviders(t *testing.T) {
	cfg := config.Default()
	cfg.Memory.ExtractModel = "ollama/llama3"
	r := RequiredProviders(cfg)
	if !r["anthropic"] || !r["ollama"] || len(r) != 2 {
		t.Errorf("required = %v", r)
	}
}
