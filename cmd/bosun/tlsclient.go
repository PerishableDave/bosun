package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/perishabledave/bosun/internal/tlsutil"
)

// ServerFileName holds the server the CLI logged in to and how to verify its
// TLS certificate. It lives next to the CLI token in the data dir.
const ServerFileName = "cli-server.json"

// savedServer is the content of ServerFileName.
type savedServer struct {
	// Server is the --server URL given to `bosun login` ("" = from config).
	Server string `json:"server,omitempty"`
	// Fingerprint pins the server's certificate (SHA-256, colon hex).
	Fingerprint string `json:"fingerprint,omitempty"`
	// CAFile is a PEM bundle trusted for the server.
	CAFile string `json:"ca_file,omitempty"`
}

func (g *globals) serverFilePath() (string, error) {
	tok, err := g.tokenPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(tok), ServerFileName), nil
}

// loadSavedServer returns the saved server settings, or a zero value if
// there are none (or no config).
func (g *globals) loadSavedServer() savedServer {
	var s savedServer
	path, err := g.serverFilePath()
	if err != nil {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, &s)
	return s
}

// saveServer records s (mode 0600), or removes the file if s is empty.
func (g *globals) saveServer(s savedServer) error {
	path, err := g.serverFilePath()
	if err != nil {
		return err
	}
	if s == (savedServer{}) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// origin returns scheme://host:port of a URL, with the default port filled
// in, for comparing servers.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(host) + ":" + port
}

// clients resolves the TLS settings for each server the CLI talks to:
//
//  1. --insecure-fingerprint: accept only the certificate with that SHA-256;
//  2. --ca-file: trust that PEM bundle (plus system roots);
//  3. the fingerprint or CA file saved by `bosun login` for that server;
//  4. the daemon's own daemon.tls.cert_file from bosun.yaml, when readable
//     (the CLI on the daemon's machine);
//  5. system roots.
//
// There is deliberately no option to skip verification.
type clients struct {
	g *globals

	mu         sync.Mutex
	transports map[string]*http.Transport
}

type clientsKey struct{}

func withClients(ctx context.Context, g *globals) context.Context {
	return context.WithValue(ctx, clientsKey{}, &clients{g: g, transports: map[string]*http.Transport{}})
}

// tlsConfigFor returns the client TLS config for an https origin, or nil
// for Go's defaults.
func (c *clients) tlsConfigFor(o string) (*tls.Config, error) {
	g := c.g
	if g.fingerprint != "" {
		fp, err := tlsutil.ParseFingerprint(g.fingerprint)
		if err != nil {
			return nil, err
		}
		return tlsutil.PinnedConfig(fp), nil
	}
	if g.caFile != "" {
		return tlsutil.CAConfig(g.caFile)
	}
	if s := g.loadSavedServer(); s.Server != "" && origin(s.Server) == o || s.Server == "" && g.server == "" {
		if s.Fingerprint != "" {
			fp, err := tlsutil.ParseFingerprint(s.Fingerprint)
			if err != nil {
				return nil, fmt.Errorf("saved server fingerprint: %w (run `bosun login` again)", err)
			}
			return tlsutil.PinnedConfig(fp), nil
		}
		if s.CAFile != "" {
			return tlsutil.CAConfig(s.CAFile)
		}
	}
	if cfg, err := g.loadConfig(); err == nil && cfg.Daemon.TLS.Enabled && cfg.Daemon.TLS.CertFile != "" {
		if _, err := os.Stat(cfg.Daemon.TLS.CertFile); err == nil {
			return tlsutil.CAConfig(cfg.Daemon.TLS.CertFile)
		}
	}
	return nil, nil
}

// transportFor returns the transport for u, or nil to use the client's
// default.
func transportFor(ctx context.Context, u *url.URL) (http.RoundTripper, error) {
	c, _ := ctx.Value(clientsKey{}).(*clients)
	if c == nil {
		return nil, nil
	}
	if u.Scheme != "https" {
		if c.g.fingerprint != "" || c.g.caFile != "" {
			return nil, fmt.Errorf("--insecure-fingerprint and --ca-file apply only to https:// servers (got %s)", u.Scheme)
		}
		return nil, nil
	}
	o := origin(u.String())
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.transports[o]; ok {
		return t, nil
	}
	cfg, err := c.tlsConfigFor(o)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = cfg
	c.transports[o] = t
	return t, nil
}

// doHTTP sends req with base, swapping in the TLS transport for its server,
// and annotates certificate errors with what to do about them.
func doHTTP(base *http.Client, req *http.Request) (*http.Response, error) {
	rt, err := transportFor(req.Context(), req.URL)
	if err != nil {
		return nil, err
	}
	c := base
	if rt != nil {
		cc := *base
		cc.Transport = rt
		c = &cc
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, tlsHint(err)
	}
	return resp, nil
}

// rememberServer saves --server and the TLS verification flags after a
// successful login so later commands reuse them. Without --server the
// previously saved server (which baseURL used) and its verification settings
// are kept unless new TLS flags are given.
func (g *globals) rememberServer(out io.Writer) error {
	prev := g.loadSavedServer()
	s := savedServer{Server: g.server}
	if g.server == "" {
		s.Server = prev.Server
	}
	if g.fingerprint == "" && g.caFile == "" && origin(s.Server) == origin(prev.Server) {
		s.Fingerprint, s.CAFile = prev.Fingerprint, prev.CAFile
	}
	if g.fingerprint != "" {
		fp, err := tlsutil.NormalizeFingerprint(g.fingerprint)
		if err != nil {
			return err
		}
		s.Fingerprint = fp
	}
	if g.caFile != "" {
		abs, err := filepath.Abs(g.caFile)
		if err != nil {
			return err
		}
		s.CAFile = abs
	}
	if err := g.saveServer(s); err != nil {
		return err
	}
	switch {
	case s.Fingerprint != "":
		fmt.Fprintf(out, "Pinned server certificate SHA-256 %s; later commands reuse it.\n", s.Fingerprint)
	case s.CAFile != "":
		fmt.Fprintf(out, "Trusting %s for this server; later commands reuse it.\n", s.CAFile)
	}
	if s.Server != "" {
		fmt.Fprintf(out, "Later commands use %s unless --server is given.\n", s.Server)
	}
	return nil
}

// errTLSHint wraps certificate failures with guidance.
type errTLSHint struct {
	err  error
	hint string
}

func (e *errTLSHint) Error() string { return e.err.Error() + "\n" + e.hint }
func (e *errTLSHint) Unwrap() error { return e.err }

func tlsHint(err error) error {
	var unknown x509.UnknownAuthorityError
	var verr *tls.CertificateVerificationError
	var herr x509.HostnameError
	switch {
	case errors.Is(err, tlsutil.ErrFingerprintMismatch):
		return &errTLSHint{err, "The server's certificate is not the pinned one: it was regenerated, or this is a different server. " +
			"Check the sha256_fingerprint bosund logs at startup before trusting a new one."}
	case errors.As(err, &unknown), errors.As(err, &verr), errors.As(err, &herr):
		return &errTLSHint{err, "The daemon's certificate is not trusted. Compare the sha256_fingerprint bosund logs at startup " +
			"(or `bosun setup` printed) and pass --insecure-fingerprint <fingerprint>, or pass --ca-file <cert.pem>."}
	}
	return err
}
