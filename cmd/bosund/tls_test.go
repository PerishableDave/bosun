package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
	"github.com/perishabledave/bosun/internal/tlsutil"
	"github.com/perishabledave/bosun/internal/vault"
)

// Tests never touch the real OS keychain.
func init() { detectKeychain = func() vault.Keychain { return nil } }

// TestServeHTTPSPinned starts bosund with TLS on a generated self-signed
// cert, checks the fingerprint is logged, and talks to it with a pinned
// client.
func TestServeHTTPSPinned(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	certPath, keyPath, err := tlsutil.GenerateSelfSigned(filepath.Join(dataDir, "tls"),
		tlsutil.SANs{DNSNames: []string{"localhost"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q, tls: { enabled: true, cert_file: %q, key_file: %q } }
llm: { default: fake/m, providers: { fake: {} } }
memory: { enabled: false }
`, dataDir, certPath, keyPath))
	if err != nil {
		t.Fatal(err)
	}
	reg := llm.NewRegistry()
	if err := reg.Register(fake.New("fake")); err != nil {
		t.Fatal(err)
	}
	var logBuf syncBuffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := newDaemon(ctx, cfg, log, deps{LLM: reg, Argon2: cheapArgon()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, ln) }()

	leaf, _ := tlsutil.LoadCertificate(certPath)
	fpText := tlsutil.Fingerprint(leaf.Raw)
	fp, _ := tlsutil.ParseFingerprint(fpText)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsutil.PinnedConfig(fp)}}
	url := "https://" + ln.Addr().String() + "/v1/health"
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = client.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("pinned GET: %v", err)
	}
	var h struct{ Status string }
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if h.Status != "ok" {
		t.Errorf("health = %+v", h)
	}
	// Plain HTTP and unpinned HTTPS fail.
	if _, err := (&http.Client{Timeout: 2 * time.Second}).Get(url); err == nil {
		t.Error("unpinned client accepted the self-signed cert")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, fpText) {
		t.Errorf("fingerprint not logged:\n%s", logs)
	}
	if !strings.Contains(logs, "vault master key ready") || !strings.Contains(logs, "master.key") {
		t.Errorf("key source not logged:\n%s", logs)
	}
	if _, err := os.Stat(filepath.Join(dataDir, vault.MasterKeyFile)); err != nil {
		t.Errorf("file key source not used: %v", err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("serve: %v", err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ io.Writer = (*syncBuffer)(nil)
