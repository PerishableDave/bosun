package main

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/dperedo/bosun/internal/tlsutil"
	"github.com/dperedo/bosun/internal/vault"
)

// detectKeychain finds the OS keychain for the vault master key. Tests
// replace it so they never touch the real keychain.
var detectKeychain = func() vault.Keychain { return vault.DetectKeychain(vault.ExecRunner{}) }

// logTLSCert logs the serving certificate's SHA-256 fingerprint, so users on
// other devices can verify it (bosun --insecure-fingerprint), and warns when
// it is close to expiry.
func logTLSCert(log *slog.Logger, certFile string) error {
	c, err := tlsutil.LoadCertificate(certFile)
	if err != nil {
		return fmt.Errorf("daemon.tls.cert_file: %w", err)
	}
	names := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	fp := tlsutil.Fingerprint(c.Raw)
	log.Info("TLS certificate", "sha256_fingerprint", fp, "names", strings.Join(names, ","),
		"not_after", c.NotAfter.UTC().Format(time.DateOnly))
	if left := time.Until(c.NotAfter); left < 30*24*time.Hour {
		log.Warn("TLS certificate expires soon", "not_after", c.NotAfter, "file", certFile)
	}
	if len(c.IPAddresses) > 0 && !hasCurrentIP(c.IPAddresses) {
		log.Warn("none of this machine's current IPv4 addresses are in the TLS certificate; " +
			"clients connecting by IP will need --insecure-fingerprint (regenerate with `bosun setup --lan --regenerate-cert`)")
	}
	return nil
}

// hasCurrentIP reports whether some non-loopback IPv4 of this host is in ips
// (true if there is no non-loopback IPv4 at all).
func hasCurrentIP(ips []net.IP) bool {
	sans, err := tlsutil.HostSANs()
	if err != nil {
		return true
	}
	found := false
	for _, h := range sans.IPs {
		if h.IsLoopback() {
			continue
		}
		found = true
		for _, ip := range ips {
			if ip.Equal(h) {
				return true
			}
		}
	}
	return !found
}
