package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/tlsutil"
)

// lanListen is the listen address `bosun setup --lan` configures.
const lanListen = "0.0.0.0:7777"

// yesNo asks a yes/no question.
func (p *prompter) yesNo(label string, def bool) (bool, error) {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	for range 3 {
		s, err := p.line(label+" ["+d+"]", "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.out, "Please answer y or n.")
	}
	return false, errors.New("too many attempts")
}

// defaultTLSPaths returns $data_dir/tls/{cert,key}.pem, using cfg's data dir
// as written (it may still start with "~").
func defaultTLSPaths(dataDir string) (cert, key string) {
	dir := filepath.Join(dataDir, "tls")
	return filepath.Join(dir, tlsutil.CertFileName), filepath.Join(dir, tlsutil.KeyFileName)
}

// setLANTLS enables TLS with the generated certificate paths, keeping
// user-supplied cert_file/key_file if both are set.
func setLANTLS(cfg *config.Config) {
	cfg.Daemon.TLS.Enabled = true
	if cfg.Daemon.TLS.CertFile == "" || cfg.Daemon.TLS.KeyFile == "" {
		cfg.Daemon.TLS.CertFile, cfg.Daemon.TLS.KeyFile = defaultTLSPaths(cfg.Daemon.DataDir)
	}
}

// enableLANInConfig switches an existing config to LAN listening with TLS.
func enableLANInConfig(path string, o setupOpts) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	switch {
	case o.listenSet:
		cfg.Daemon.Listen = o.listen
	default:
		host, _, err := net.SplitHostPort(cfg.Daemon.Listen)
		if ip := net.ParseIP(host); err != nil || host == "localhost" || (ip != nil && ip.IsLoopback()) {
			cfg.Daemon.Listen = lanListen
		}
	}
	setLANTLS(cfg)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid settings: %w", err)
	}
	return config.Save(path, cfg)
}

// ensureTLSCert generates the self-signed certificate if cfg uses the
// default paths and it is missing (or regen is set), then prints its
// fingerprint and how to connect from another device.
func ensureTLSCert(out io.Writer, cfg *config.Config, regen bool) error {
	certPath, keyPath := cfg.Daemon.TLS.CertFile, cfg.Daemon.TLS.KeyFile
	defCert, defKey := defaultTLSPaths(cfg.Daemon.DataDir)
	generated := certPath == defCert && keyPath == defKey
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	missing := errors.Is(certErr, fs.ErrNotExist) || errors.Is(keyErr, fs.ErrNotExist)

	if generated && (missing || regen) {
		if regen {
			for _, p := range []string{certPath, keyPath} {
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("remove old certificate: %w", err)
				}
			}
		} else if errors.Is(certErr, fs.ErrNotExist) != errors.Is(keyErr, fs.ErrNotExist) {
			return fmt.Errorf("only one of %s and %s exists; remove it or pass --regenerate-cert", certPath, keyPath)
		}
		sans, err := tlsutil.HostSANs()
		if err != nil {
			fmt.Fprintf(out, "warning: %v; the certificate covers only %v\n", err, sans.DNSNames)
		}
		if _, _, err := tlsutil.GenerateSelfSigned(filepath.Dir(certPath), sans, time.Now()); err != nil {
			return err
		}
		fmt.Fprintf(out, "Generated self-signed TLS certificate %s (key %s, mode 0600)\n", certPath, keyPath)
	} else if missing {
		return fmt.Errorf("daemon.tls: %s or %s does not exist", certPath, keyPath)
	} else if regen {
		fmt.Fprintln(out, "Not regenerating: daemon.tls points at your own certificate files.")
	}

	c, err := tlsutil.LoadCertificate(certPath)
	if err != nil {
		return err
	}
	names := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	fp := tlsutil.Fingerprint(c.Raw)
	_, port, _ := net.SplitHostPort(cfg.Daemon.Listen)
	host := "<this-host>"
	if len(c.DNSNames) > 0 {
		host = c.DNSNames[0]
	}
	fmt.Fprintf(out, "TLS certificate valid for: %s (until %s)\n", strings.Join(names, ", "), c.NotAfter.Format(time.DateOnly))
	fmt.Fprintf(out, "SHA-256 fingerprint: %s\n", fp)
	fmt.Fprintln(out, "From another device, check the fingerprint matches, then:")
	fmt.Fprintf(out, "  bosun --server https://%s --insecure-fingerprint %s login\n", net.JoinHostPort(host, port), fp)
	return nil
}
