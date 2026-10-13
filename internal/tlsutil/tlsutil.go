// Package tlsutil generates the self-signed certificate `bosun setup --lan`
// installs for bosund, computes certificate fingerprints, and builds client
// TLS configs that pin a fingerprint instead of skipping verification.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// File names inside the TLS directory ($data_dir/tls).
const (
	CertFileName = "cert.pem"
	KeyFileName  = "key.pem"
)

// Validity of generated certificates.
const Validity = 10 * 365 * 24 * time.Hour

// SANs are the names and addresses a certificate is valid for.
type SANs struct {
	DNSNames []string
	IPs      []net.IP
}

// HostSANs returns the SANs for this machine: its hostname, "localhost",
// every non-loopback IPv4 address of an up interface, and the loopback
// addresses 127.0.0.1 and ::1 (so the local CLI can verify the same cert).
func HostSANs() (SANs, error) {
	var s SANs
	if h, err := os.Hostname(); err == nil && h != "" {
		s.DNSNames = append(s.DNSNames, h)
		// macOS reports "name.local" or "name"; offer both forms for mDNS.
		if short, _, ok := strings.Cut(h, "."); ok && short != "" {
			s.DNSNames = append(s.DNSNames, short)
		} else if !strings.Contains(h, ".") {
			s.DNSNames = append(s.DNSNames, h+".local")
		}
	}
	s.DNSNames = append(s.DNSNames, "localhost")
	s.IPs = append(s.IPs, net.IPv4(127, 0, 0, 1).To4(), net.IPv6loopback)
	ifaces, err := net.Interfaces()
	if err != nil {
		return s, fmt.Errorf("list network interfaces: %w", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				s.IPs = append(s.IPs, ip4)
			}
		}
	}
	s.DNSNames = dedupe(s.DNSNames)
	return s, nil
}

func dedupe(in []string) []string {
	var out []string
	for _, v := range in {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// GenerateSelfSigned writes a self-signed ECDSA P-256 certificate valid for
// Validity and sans to dir/cert.pem, and its private key to dir/key.pem with
// mode 0600 (dir is created 0700). It refuses to overwrite existing files.
// It returns the two paths.
func GenerateSelfSigned(dir string, sans SANs, now time.Time) (certPath, keyPath string, err error) {
	if len(sans.DNSNames) == 0 && len(sans.IPs) == 0 {
		return "", "", errors.New("tls: at least one SAN is required")
	}
	certPath, keyPath = filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName)
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err == nil {
			return "", "", fmt.Errorf("tls: %s already exists", p)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("tls: create %s: %w", dir, err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("tls: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", fmt.Errorf("tls: serial: %w", err)
	}
	cn := "bosund"
	if len(sans.DNSNames) > 0 {
		cn = "bosund " + sans.DNSNames[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Bosun (self-signed)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// An end-entity cert (browsers reject CA certs used as server
		// certs). Clients trust it directly: as a --ca-file root, which
		// Go accepts for a self-signed leaf, or by fingerprint pin.
		IsCA:        false,
		DNSNames:    sans.DNSNames,
		IPAddresses: sans.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", "", fmt.Errorf("tls: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", fmt.Errorf("tls: encode key: %w", err)
	}
	// Key first, created exclusively with 0600, so it is never readable.
	if err := writeExclusive(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	if err := writeExclusive(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		os.Remove(keyPath)
		return "", "", err
	}
	return certPath, keyPath, nil
}

func writeExclusive(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(path)
		return fmt.Errorf("tls: write %s: %w", path, err)
	}
	return nil
}

// LoadCertificate reads the first certificate from a PEM file.
func LoadCertificate(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("tls: no certificate in %s", path)
		}
		if blk.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, fmt.Errorf("tls: parse %s: %w", path, err)
			}
			return c, nil
		}
	}
}

// Fingerprint returns the SHA-256 of a DER certificate as uppercase hex
// pairs separated by colons ("AB:CD:..."), the form browsers show.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// ParseFingerprint accepts a SHA-256 fingerprint as 64 hex digits, with or
// without ":" separators or a "sha256:" prefix, in any case.
func ParseFingerprint(s string) ([]byte, error) {
	t := strings.TrimSpace(s)
	if len(t) > 7 && strings.EqualFold(t[:7], "sha256:") {
		t = t[7:]
	}
	t = strings.NewReplacer(":", "", " ", "").Replace(t)
	b, err := hex.DecodeString(t)
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("invalid SHA-256 fingerprint %q (want 64 hex digits, colons optional)", s)
	}
	return b, nil
}

// NormalizeFingerprint returns s in Fingerprint's canonical form.
func NormalizeFingerprint(s string) (string, error) {
	b, err := ParseFingerprint(s)
	if err != nil {
		return "", err
	}
	h := strings.ToUpper(hex.EncodeToString(b))
	parts := make([]string, 0, len(b))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":"), nil
}

// ErrFingerprintMismatch means the server presented a certificate other than
// the pinned one.
var ErrFingerprintMismatch = errors.New("tls: server certificate does not match the pinned fingerprint")

// PinnedConfig returns a client TLS config that accepts exactly the server
// certificate whose SHA-256 is fp, and nothing else. Chain and hostname
// verification are replaced by the pin (a self-signed cert has no chain to
// verify, and the pin is stronger than a name check); it is never a blanket
// skip.
func PinnedConfig(fp []byte) *tls.Config {
	want := slices.Clone(fp)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Required to replace chain verification with the pin below; the
		// connection is still rejected unless VerifyConnection passes.
		InsecureSkipVerify: true, //nolint:gosec // see VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrFingerprintMismatch
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return fmt.Errorf("%w (got %s)", ErrFingerprintMismatch, Fingerprint(cs.PeerCertificates[0].Raw))
			}
			return nil
		},
	}
}

// CAConfig returns a client TLS config trusting the system roots plus the
// PEM certificates in caFile.
func CAConfig(caFile string) (*tls.Config, error) {
	b, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("tls: read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("tls: no PEM certificates in %s", caFile)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
}
