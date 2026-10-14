// Package doctor implements the health checks behind `bosun doctor` and
// GET /v1/doctor. Each check is a small function returning a Check; the CLI
// runs the local ones when the daemon is down and asks the daemon for the
// rest when it is up.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/tlsutil"
	"github.com/perishabledave/bosun/internal/vault"
)

// Status is a check outcome.
type Status string

// Check outcomes, from best to worst.
const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check is one line of the doctor report.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Worst returns the worst status among checks (OK when empty).
func Worst(checks []Check) Status {
	w := OK
	for _, c := range checks {
		switch {
		case c.Status == Fail:
			return Fail
		case c.Status == Warn:
			w = Warn
		}
	}
	return w
}

func ok(name, format string, a ...any) Check {
	return Check{Name: name, Status: OK, Detail: fmt.Sprintf(format, a...)}
}
func warn(name, format string, a ...any) Check {
	return Check{Name: name, Status: Warn, Detail: fmt.Sprintf(format, a...)}
}
func fail(name, format string, a ...any) Check {
	return Check{Name: name, Status: Fail, Detail: fmt.Sprintf(format, a...)}
}

// DataDir checks that dir exists, is a directory, and is mode 0700.
func DataDir(dir string) Check {
	const name = "data dir"
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return fail(name, "%s does not exist (run `bosun setup`)", dir)
	}
	if err != nil {
		return fail(name, "%s: %v", dir, err)
	}
	if !fi.IsDir() {
		return fail(name, "%s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return warn(name, "%s has mode %04o; run `chmod 700 %s`", dir, perm, dir)
	}
	return ok(name, "%s (0700)", dir)
}

// MasterKey checks the vault master key source without creating anything:
// a file key must be 32 bytes with mode 0600; a keychain key must be
// readable. detect returns the OS keychain (nil if none); tests pass a fake.
func MasterKey(ctx context.Context, cfg *config.Config, detect func() vault.Keychain) Check {
	const name = "master key"
	dir := cfg.Daemon.DataDir
	keyFile := filepath.Join(dir, vault.MasterKeyFile)
	marked := false
	if b, err := os.ReadFile(filepath.Join(dir, vault.KeySourceMarkerFile)); err == nil && strings.TrimSpace(string(b)) == vault.KeyModeKeychain {
		marked = true
	}
	mode := cfg.Daemon.MasterKey
	if mode == "" {
		mode = vault.KeyModeAuto
	}
	fileCheck := func() Check {
		f, err := os.Open(keyFile)
		if errors.Is(err, os.ErrNotExist) {
			return warn(name, "file %s does not exist yet (bosund creates it on first start)", keyFile)
		}
		if err != nil {
			return fail(name, "file %s: %v", keyFile, err)
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return fail(name, "file %s: %v", keyFile, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return fail(name, "file %s has mode %04o; bosund refuses it (want 0600)", keyFile, fi.Mode().Perm())
		}
		if fi.Size() != vault.KeySize {
			return fail(name, "file %s is %d bytes, want %d", keyFile, fi.Size(), vault.KeySize)
		}
		return ok(name, "file %s (0600, readable)", keyFile)
	}
	if mode == vault.KeyModeFile || (mode == vault.KeyModeAuto && !marked) {
		return fileCheck()
	}
	kc := detect()
	if kc == nil {
		return fail(name, "the master key is in the OS keychain but no keychain tool is available")
	}
	_, err := kc.Get(ctx, vault.DefaultKeychainService, vault.DefaultKeychainAccount)
	switch {
	case errors.Is(err, vault.ErrKeychainItemNotFound):
		if !marked {
			return warn(name, "%s has no bosun master key yet (bosund creates it on first start)", kc.Name())
		}
		return fail(name, "%s has no bosun master key, but the data dir says it lives there; stored credentials cannot be decrypted", kc.Name())
	case err != nil:
		return fail(name, "%s: %v", kc.Name(), err)
	}
	if _, err := os.Stat(keyFile); err == nil {
		// bosund imports an existing key file into the keychain but leaves
		// the file in place; anyone who can read it can decrypt the vault.
		return warn(name, "%s holds the key, but %s still has a copy: delete it once bosund has started with the keychain", kc.Name(), keyFile)
	}
	return ok(name, "%s (service %s, account %s; readable)", kc.Name(), vault.DefaultKeychainService, vault.DefaultKeychainAccount)
}

// TLS checks the serving certificate's expiry and reports its fingerprint.
func TLS(cfg *config.Config, now time.Time) Check {
	const name = "tls"
	if !cfg.Daemon.TLS.Enabled {
		if cfg.Daemon.BehindProxy {
			return ok(name, "terminated by the reverse proxy (daemon.behind_proxy)")
		}
		return ok(name, "not enabled (loopback listen)")
	}
	c, err := tlsutil.LoadCertificate(cfg.Daemon.TLS.CertFile)
	if err != nil {
		return fail(name, "%v", err)
	}
	fp := tlsutil.Fingerprint(c.Raw)
	left := c.NotAfter.Sub(now)
	exp := c.NotAfter.UTC().Format(time.DateOnly)
	switch {
	case left <= 0:
		return fail(name, "certificate expired %s (regenerate with `bosun setup --lan --regenerate-cert`); sha256 %s", exp, fp)
	case left < 30*24*time.Hour:
		return warn(name, "certificate expires %s (%d days); sha256 %s", exp, int(left.Hours()/24), fp)
	}
	return ok(name, "certificate valid until %s (%d days); sha256 %s", exp, int(left.Hours()/24), fp)
}

// Disk thresholds for the data dir's filesystem.
const (
	DiskFailBytes = 100 << 20
	DiskWarnBytes = 1 << 30
)

// Disk checks the free space on the data dir's filesystem.
func Disk(dir string) Check {
	const name = "disk space"
	free, err := freeBytes(dir)
	if err != nil {
		return warn(name, "%v", err)
	}
	s := humanBytes(free)
	switch {
	case free < DiskFailBytes:
		return fail(name, "%s free on the data dir's filesystem", s)
	case free < DiskWarnBytes:
		return warn(name, "%s free on the data dir's filesystem", s)
	}
	return ok(name, "%s free", s)
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Proxy checks daemon.behind_proxy for common mistakes. ignored is how many
// requests carried X-Forwarded-* headers that were not believed, and lastIP
// the last such peer (0 and "" when unknown, e.g. the daemon is down).
func Proxy(cfg *config.Config, ignored int64, lastIP string) Check {
	const name = "reverse proxy"
	d := cfg.Daemon
	if !d.BehindProxy {
		if ignored > 0 {
			return warn(name, "%d request(s) carried X-Forwarded-* headers (last from %s) but daemon.behind_proxy is off, so they were ignored; set it if a reverse proxy fronts bosund", ignored, lastIP)
		}
		return ok(name, "not behind a proxy")
	}
	var problems []string
	if len(d.TrustedProxies) == 0 {
		problems = append(problems, "daemon.trusted_proxies is unset, so every loopback and private address is trusted; list your proxy's address")
	}
	if host, _, err := net.SplitHostPort(d.Listen); err == nil {
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			problems = append(problems, fmt.Sprintf("listening on %s serves plain HTTP to the network; if the proxy runs on this host, listen on 127.0.0.1", d.Listen))
		}
	}
	if ignored > 0 {
		problems = append(problems, fmt.Sprintf("%d request(s) with X-Forwarded-* headers came from untrusted %s; add the proxy's address to daemon.trusted_proxies", ignored, lastIP))
	}
	if len(problems) > 0 {
		return warn(name, "%s", strings.Join(problems, "; "))
	}
	return ok(name, "behind a proxy; trusting %s", strings.Join(d.TrustedProxies, ", "))
}

// Routines reports the scheduler state and routine count.
func Routines(running bool, total, enabled int) Check {
	const name = "routines"
	if !running {
		return fail(name, "scheduler is not running (%d routines, %d enabled)", total, enabled)
	}
	return ok(name, "scheduler running; %d routines (%d enabled)", total, enabled)
}

// Integration converts one integration's reported status into a check.
func Integration(name, state, message string, tools int) Check {
	label := "integration " + name
	if name == "channels" {
		label = "channels"
	}
	detail := message
	if name != "channels" && name != "oauth" && name != "llm" {
		t := fmt.Sprintf("%d tools", tools)
		if detail == "" {
			detail = t
		} else {
			detail = t + "; " + detail
		}
	}
	switch state {
	case "ok", "":
		return Check{Name: label, Status: OK, Detail: detail}
	case "degraded":
		return Check{Name: label, Status: Warn, Detail: detail}
	default:
		return Check{Name: label, Status: Fail, Detail: detail}
	}
}

// KeySource describes where an LLM provider's API key comes from.
type KeySource struct {
	// Source is "vault", "env", or "" (none).
	Source string
	// Env is the api_key_env variable name, when configured.
	Env string
}

func (k KeySource) String() string {
	switch k.Source {
	case "vault":
		return "key from the vault (llm shared credential)"
	case "env":
		return "key from $" + k.Env
	case "keyless":
		return "no key needed"
	}
	return "no key"
}

// LLM checks one provider: its key source and a cheap reachability probe
// (Anthropic and OpenAI: list models; Ollama: /api/tags). required is true
// for providers used by llm.default or memory.extract_model: their
// problems fail, others only warn. key is "" when there is none or it could
// not be read; probe=false skips the network check (reporting the key
// source only).
func LLM(ctx context.Context, hc *http.Client, name string, pc config.ProviderConfig, key string, src KeySource, required, probe bool) Check {
	label := "llm " + name
	bad := warn
	if required {
		bad = fail
	}
	var req *http.Request
	var err error
	switch name {
	case "anthropic":
		if src.Source == "" {
			return bad(label, "no API key: run `bosun credentials set llm anthropic` (or set api_key_env)")
		}
		base := strings.TrimRight(pc.BaseURL, "/")
		if base == "" {
			base = "https://api.anthropic.com"
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models?limit=1", nil)
		if err == nil {
			req.Header.Set("anthropic-version", "2023-06-01")
			if key != "" {
				req.Header.Set("x-api-key", key)
			}
		}
	case "openai":
		base := strings.TrimRight(pc.BaseURL, "/")
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		if src.Source == "" && strings.Contains(base, "api.openai.com") {
			return bad(label, "no API key: run `bosun credentials set llm openai` (or set api_key_env)")
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
		if err == nil && key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	case "ollama":
		if src.Source == "" {
			src.Source = "keyless"
		}
		base := strings.TrimRight(pc.BaseURL, "/")
		if base == "" {
			base = "http://localhost:11434"
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	default:
		return ok(label, "%s; no reachability check for this provider", src)
	}
	if err != nil {
		return bad(label, "%s; bad base_url: %v", src, err)
	}
	if !probe {
		return ok(label, "%s; reachability not checked", src)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return bad(label, "%s; unreachable: %v", src, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return bad(label, "%s; the key was rejected (HTTP %d)", src, resp.StatusCode)
	case resp.StatusCode/100 != 2:
		return bad(label, "%s; HTTP %d from %s", src, resp.StatusCode, req.URL.Host)
	}
	if name == "ollama" {
		var tags struct {
			Models []json.RawMessage `json:"models"`
		}
		if json.Unmarshal(body, &tags) == nil {
			return ok(label, "reachable at %s; %d models pulled", req.URL.Host, len(tags.Models))
		}
	}
	return ok(label, "%s; reachable", src)
}

// RequiredProviders returns the providers used by llm.default and
// memory.extract_model.
func RequiredProviders(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	if p, _, ok := strings.Cut(cfg.LLM.Default, "/"); ok {
		out[p] = true
	}
	if cfg.Memory.IsEnabled() {
		if p, _, ok := strings.Cut(cfg.ExtractModelRef(), "/"); ok {
			out[p] = true
		}
	}
	return out
}
