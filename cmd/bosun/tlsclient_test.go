package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/tlsutil"
)

// tlsDaemon is a fake bosund over HTTPS with a generated self-signed cert.
func tlsDaemon(t *testing.T) (srv *httptest.Server, certPath, fp string) {
	t.Helper()
	certPath, keyPath, err := tlsutil.GenerateSelfSigned(filepath.Join(t.TempDir(), "tls"),
		tlsutil.SANs{DNSNames: []string{"localhost"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","version":"test"}`)
	})
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"bsn_remote","user":{"id":1,"username":"dave","role":"admin"}}`)
	})
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bsn_remote" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"id":1,"username":"dave","role":"admin"}`)
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	leaf, err := tlsutil.LoadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	return srv, certPath, tlsutil.Fingerprint(leaf.Raw)
}

func writeCfg(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	cfgPath = filepath.Join(dataDir, "bosun.yaml")
	if err := os.WriteFile(cfgPath, fmt.Appendf(nil, "daemon: { data_dir: %q }\n", dataDir), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

func TestPinnedLoginIsReused(t *testing.T) {
	srv, certPath, fp := tlsDaemon(t)
	cfgPath, dataDir := writeCfg(t)

	// Unverified: fails with guidance, never silently connects.
	_, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "status")
	if err == nil || !strings.Contains(err.Error(), "--insecure-fingerprint") {
		t.Fatalf("unpinned status: %v", err)
	}
	// Wrong pin.
	wrong := strings.Repeat("00:", 31) + "00"
	if _, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "--insecure-fingerprint", wrong, "status"); err == nil ||
		!strings.Contains(err.Error(), "pinned") {
		t.Fatalf("wrong pin: %v", err)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "--insecure-fingerprint", "nothex", "status"); err == nil {
		t.Fatal("malformed fingerprint accepted")
	}
	// Pinned login (lowercase, no colons is accepted too).
	pin := strings.ToLower(strings.ReplaceAll(fp, ":", ""))
	out, err := runCLI(t, "pw\n", "--config", cfgPath, "--server", srv.URL, "--insecure-fingerprint", pin, "login", "-u", "dave")
	if err != nil || !strings.Contains(out, "Pinned server certificate SHA-256 "+fp) {
		t.Fatalf("login: %v\n%s", err, out)
	}
	fi, err := os.Stat(filepath.Join(dataDir, ServerFileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("server file: %v %v", fi, err)
	}
	var saved savedServer
	b, _ := os.ReadFile(filepath.Join(dataDir, ServerFileName))
	json.Unmarshal(b, &saved)
	if saved.Server != srv.URL || saved.Fingerprint != fp {
		t.Errorf("saved = %+v", saved)
	}
	// Later commands reuse the server and the pin without flags.
	out, err = runCLI(t, "", "--config", cfgPath, "status")
	if err != nil || !strings.Contains(out, "logged in as dave") || !strings.Contains(out, srv.URL) {
		t.Fatalf("status after login: %v\n%s", err, out)
	}
	// A re-login without flags keeps the server and pin.
	if out, err := runCLI(t, "pw\n", "--config", cfgPath, "login", "-u", "dave"); err != nil {
		t.Fatalf("re-login: %v\n%s", err, out)
	}
	if out, err := runCLI(t, "", "--config", cfgPath, "status"); err != nil || !strings.Contains(out, "logged in as dave") {
		t.Fatalf("status after re-login: %v\n%s", err, out)
	}

	// --ca-file also works, and flags are exclusive.
	if out, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "--ca-file", certPath, "status"); err != nil {
		t.Fatalf("ca-file: %v\n%s", err, out)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "--ca-file", certPath, "--insecure-fingerprint", fp, "status"); err == nil {
		t.Fatal("both --ca-file and --insecure-fingerprint accepted")
	}
	// TLS flags make no sense for http://.
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	if _, err := runCLI(t, "", "--config", cfgPath, "--server", plain.URL, "--insecure-fingerprint", fp, "status"); err == nil ||
		!strings.Contains(err.Error(), "https") {
		t.Fatalf("pin with http: %v", err)
	}
}

// TestLocalDaemonCertTrusted: on the daemon's machine the CLI trusts the
// cert named by daemon.tls.cert_file without extra flags.
func TestLocalDaemonCertTrusted(t *testing.T) {
	srv, certPath, _ := tlsDaemon(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bosun.yaml")
	if err := os.WriteFile(cfgPath, fmt.Appendf(nil, "daemon: { data_dir: %q, tls: { enabled: true, cert_file: %q, key_file: /unused } }\n",
		dir, certPath), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "status"); err != nil || !strings.Contains(out, "bosund ok") {
		t.Fatalf("status: %v\n%s", err, out)
	}
}

func TestSetupLAN(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bosun.yaml")
	dataDir := filepath.Join(dir, "data")
	out, err := runCLI(t, "dave\nlongenough\nlongenough\n", "--config", cfgPath, "setup", "--data-dir", dataDir, "--lan")
	if err != nil {
		t.Fatalf("setup --lan: %v\n%s", err, out)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Listen != "0.0.0.0:7777" || !cfg.Daemon.TLS.Enabled ||
		cfg.Daemon.TLS.CertFile != filepath.Join(dataDir, "tls", "cert.pem") || cfg.Daemon.TLS.KeyFile != filepath.Join(dataDir, "tls", "key.pem") {
		t.Fatalf("daemon config = %+v", cfg.Daemon)
	}
	fi, err := os.Stat(cfg.Daemon.TLS.KeyFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key: %v %v", fi, err)
	}
	c, err := tlsutil.LoadCertificate(cfg.Daemon.TLS.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	fp := tlsutil.Fingerprint(c.Raw)
	if !strings.Contains(out, "SHA-256 fingerprint: "+fp) || !strings.Contains(out, "--insecure-fingerprint "+fp+" login") {
		t.Errorf("setup output:\n%s", out)
	}
	if err := c.VerifyHostname("localhost"); err != nil {
		t.Error(err)
	}

	// Re-running keeps the cert; --regenerate-cert replaces it.
	out, err = runCLI(t, "", "--config", cfgPath, "setup")
	if err != nil || !strings.Contains(out, fp) {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	out, err = runCLI(t, "", "--config", cfgPath, "setup", "--lan", "--regenerate-cert")
	if err != nil || strings.Contains(out, fp) || !strings.Contains(out, "Generated self-signed") {
		t.Fatalf("regenerate: %v\n%s", err, out)
	}
}

func TestSetupLANInteractiveAndExisting(t *testing.T) {
	// Interactive "y".
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bosun.yaml")
	out, err := runCLI(t, "y\ndave\nlongenough\nlongenough\n", "--config", cfgPath, "setup", "--data-dir", filepath.Join(dir, "data"))
	if err != nil || !strings.Contains(out, "Allow other devices") {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || cfg.Daemon.Listen != lanListen || !cfg.Daemon.TLS.Enabled {
		t.Fatalf("config %+v %v", cfg, err)
	}

	// Interactive default "no" keeps loopback.
	dir = t.TempDir()
	cfgPath = filepath.Join(dir, "bosun.yaml")
	if out, err := runCLI(t, "\n\ndave\nlongenough\nlongenough\n", "--config", cfgPath, "setup", "--data-dir", filepath.Join(dir, "data")); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if cfg, _ := config.Load(cfgPath); cfg.Daemon.Listen != config.DefaultListen || cfg.Daemon.TLS.Enabled {
		t.Fatalf("config %+v", cfg.Daemon)
	}
	// --lan on an existing loopback config switches it.
	out, err = runCLI(t, "", "--config", cfgPath, "setup", "--lan")
	if err != nil || !strings.Contains(out, "Updated") || !strings.Contains(out, "SHA-256 fingerprint") {
		t.Fatalf("setup --lan existing: %v\n%s", err, out)
	}
	if cfg, _ := config.Load(cfgPath); cfg.Daemon.Listen != lanListen || !cfg.Daemon.TLS.Enabled {
		t.Fatalf("config %+v", cfg.Daemon)
	}
}
