package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGenerateSelfSigned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	now := time.Now()
	sans := SANs{DNSNames: []string{"bosun-host", "localhost"}, IPs: []net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("127.0.0.1")}}
	certPath, keyPath, err := GenerateSelfSigned(dir, sans, now)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %v, want 0600", fi.Mode().Perm())
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	c, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.DNSNames, sans.DNSNames) {
		t.Errorf("DNSNames = %v", c.DNSNames)
	}
	var ips []string
	for _, ip := range c.IPAddresses {
		ips = append(ips, ip.String())
	}
	if !slices.Equal(ips, []string{"192.168.1.20", "127.0.0.1"}) {
		t.Errorf("IPs = %v", ips)
	}
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key %T, want ECDSA P-256", c.PublicKey)
	}
	if d := c.NotAfter.Sub(now); d < Validity-2*time.Hour || d > Validity+time.Hour {
		t.Errorf("validity %v", d)
	}
	for _, h := range []string{"bosun-host", "localhost", "192.168.1.20"} {
		if err := c.VerifyHostname(h); err != nil {
			t.Errorf("VerifyHostname(%s): %v", h, err)
		}
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("load pair: %v", err)
	}
	// No silent overwrite.
	if _, _, err := GenerateSelfSigned(dir, sans, now); err == nil {
		t.Error("overwrote existing cert")
	}
}

func TestHostSANs(t *testing.T) {
	s, err := HostSANs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.DNSNames, "localhost") {
		t.Errorf("DNSNames %v lacks localhost", s.DNSNames)
	}
	if h, _ := os.Hostname(); h != "" && !slices.Contains(s.DNSNames, h) {
		t.Errorf("DNSNames %v lacks hostname %q", s.DNSNames, h)
	}
	for _, ip := range s.IPs {
		if ip.To4() == nil && !ip.IsLoopback() {
			t.Errorf("non-loopback IPv6 %v included", ip)
		}
	}
}

func TestFingerprintParse(t *testing.T) {
	fp := Fingerprint([]byte("hello"))
	if len(fp) != 95 || strings.ToUpper(fp) != fp {
		t.Fatalf("Fingerprint = %q", fp)
	}
	for _, in := range []string{fp, strings.ToLower(fp), strings.ReplaceAll(fp, ":", ""), "sha256:" + fp, "SHA256:" + strings.ToLower(strings.ReplaceAll(fp, ":", ""))} {
		got, err := NormalizeFingerprint(in)
		if err != nil || got != fp {
			t.Errorf("Normalize(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abcd", strings.Repeat("zz", 32), fp + ":00"} {
		if _, err := ParseFingerprint(bad); err == nil {
			t.Errorf("ParseFingerprint(%q) accepted", bad)
		}
	}
}

// TestPinnedRoundTrip serves HTTPS with a generated cert and checks that a
// pinned client connects, a wrong pin fails, and an unpinned client fails.
func TestPinnedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := GenerateSelfSigned(dir, SANs{DNSNames: []string{"localhost"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()

	leaf, _ := LoadCertificate(certPath)
	fp, _ := ParseFingerprint(Fingerprint(leaf.Raw))
	get := func(cfg *tls.Config) error {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if string(b) != "ok" {
			t.Errorf("body %q", b)
		}
		return nil
	}
	if err := get(PinnedConfig(fp)); err != nil {
		t.Errorf("pinned: %v", err)
	}
	wrong := slices.Clone(fp)
	wrong[0] ^= 0xff
	if err := get(PinnedConfig(wrong)); err == nil || !errors.Is(err, ErrFingerprintMismatch) && !strings.Contains(err.Error(), "pinned fingerprint") {
		t.Errorf("wrong pin: %v", err)
	}
	if err := get(&tls.Config{}); err == nil {
		t.Error("unpinned client accepted self-signed cert")
	}
	ca, err := CAConfig(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := get(ca); err != nil {
		t.Errorf("ca-file: %v", err)
	}
}
