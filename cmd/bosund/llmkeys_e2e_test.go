package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/doctor"
)

// fakeAnthropic answers /v1/messages and /v1/models, accepting only key
// "sk-vault", and records the keys it saw.
type fakeAnthropic struct {
	mu   sync.Mutex
	keys []string
}

func (f *fakeAnthropic) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		f.mu.Lock()
		f.keys = append(f.keys, key)
		f.mu.Unlock()
		if key != "sk-vault" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"claude-test"}]}`))
			return
		}
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from the vault key"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
				`{"type":"message_stop"}`,
			} {
				var typ struct {
					Type string `json:"type"`
				}
				json.Unmarshal([]byte(ev), &typ)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, ev)
			}
			return
		}
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hello from the vault key"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
}

// TestLLMKeyFromVault: with no key in the environment the Anthropic
// provider fails; after an admin stores llm/anthropic through the
// credentials API (what `bosun credentials set llm anthropic` does), the
// next turn uses it without a restart, and GET /v1/doctor reports it.
func TestLLMKeyFromVault(t *testing.T) {
	fa := &fakeAnthropic{}
	llmSrv := httptest.NewServer(fa.handler())
	defer llmSrv.Close()

	dir := t.TempDir()
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q, master_key: file }
llm:
  default: anthropic/claude-test
  providers:
    anthropic: { api_key_env: BOSUN_TEST_UNSET_ANTHROPIC_KEY, base_url: %q }
memory: { enabled: false }
`, filepath.Join(dir, "data"), llmSrv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := newDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), deps{Argon2: cheapArgon()})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		d.Close()
	}()
	h := &harness{t: t, d: d, base: "http://" + ln.Addr().String()}
	if _, err := d.auth.CreateUser(ctx, "alice", "alice-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	alice := h.login("alice", "alice-password")

	var il api.IntegrationListResponse
	if c := h.do("GET", "/v1/integrations", alice, nil, &il); c != 200 || len(il.Integrations) != 1 ||
		il.Integrations[0].Name != "llm" || il.Integrations[0].Status != "degraded" || !strings.Contains(il.Integrations[0].Message, "anthropic: no key") {
		t.Fatalf("integrations before: %d %+v", c, il)
	}
	var dr api.DoctorResponse
	if c := h.do("GET", "/v1/doctor", alice, nil, &dr); c != 200 || dr.Status != doctor.Fail {
		t.Fatalf("doctor before: %d %+v", c, dr)
	}

	conv := h.newConversation(alice)
	evs := h.turn(alice, conv, "hi", nil)
	if last := evs[len(evs)-1]; last.Type != "error" {
		t.Fatalf("turn without a key should fail, got %+v", last)
	}

	var cr api.CredentialResponse
	if c := h.do("PUT", "/v1/credentials/llm/anthropic", alice, api.PutCredentialRequest{Secret: "sk-vault"}, &cr); c != 200 || cr.Scope != "shared" {
		t.Fatalf("PUT = %d %+v", c, cr)
	}
	evs = h.turn(alice, conv, "hi again", nil)
	text := ""
	for _, ev := range evs {
		var data struct {
			Delta string `json:"delta"`
		}
		json.Unmarshal(ev.Data, &data)
		text += data.Delta
	}
	if !strings.Contains(text, "hello from the vault key") {
		t.Fatalf("turn with the vault key: %+v", evs)
	}

	if c := h.do("GET", "/v1/doctor", alice, nil, &dr); c != 200 {
		t.Fatalf("doctor = %d", c)
	}
	byName := map[string]doctor.Check{}
	for _, c := range dr.Checks {
		byName[c.Name] = c
	}
	if c := byName["llm anthropic"]; c.Status != doctor.OK || !strings.Contains(c.Detail, "vault") {
		t.Errorf("llm check = %+v", c)
	}
	if c := byName["routines"]; c.Status != doctor.OK {
		t.Errorf("routines check = %+v", c)
	}
	for _, name := range []string{"data dir", "master key", "tls", "disk space", "reverse proxy", "channels"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("doctor lacks %q: %+v", name, dr.Checks)
		}
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	for _, k := range fa.keys {
		if k != "" && k != "sk-vault" {
			t.Errorf("unexpected key sent: %q", k)
		}
	}
}
