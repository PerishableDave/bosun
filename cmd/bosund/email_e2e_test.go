package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/hooks"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/integrations/email"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
)

// evStub records the emitter and user lister the daemon wires.
type evStub struct {
	mu   sync.Mutex
	emit func(context.Context, int64, string, json.RawMessage) error
	list func(context.Context) ([]int64, error)
}

func (s *evStub) Manifest() integration.Manifest {
	return integration.Manifest{Name: "evstub", Version: "test", Scope: integration.ScopePerUser,
		Capabilities: []integration.Capability{"evstub.x"}, CredentialKinds: []string{"apikey"}}
}
func (s *evStub) Init(context.Context, integration.Config, integration.CredentialSource) error {
	return nil
}
func (s *evStub) Close() error { return nil }
func (s *evStub) Tools() []integration.Tool {
	return []integration.Tool{{Name: "evstub.x", Capability: "evstub.x", Risk: integration.RiskRead,
		Handler: func(context.Context, json.RawMessage) (integration.Result, error) { return integration.Result{}, nil }}}
}
func (s *evStub) SetEmitter(f func(context.Context, int64, string, json.RawMessage) error) {
	s.mu.Lock()
	s.emit = f
	s.mu.Unlock()
}
func (s *evStub) SetUserLister(f func(context.Context) ([]int64, error)) {
	s.mu.Lock()
	s.list = f
	s.mu.Unlock()
}

// TestEndToEndEmailWiring checks the email integration is a built-in with
// the expected risks, and that integrations implementing EventEmitter /
// UserLister are wired to the hooks and the vault.
func TestEndToEndEmailWiring(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm:
  default: fake/test-model
  providers: { fake: {} }
memory: { enabled: false }
integrations:
  email: { enabled: true, poll_interval: "off" }
`, filepath.Join(dir, "data")))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, llm: fake.New("fake")}
	reg := llm.NewRegistry()
	if err := reg.Register(h.llm); err != nil {
		t.Fatal(err)
	}
	stub := &evStub{}
	ctx, cancel := context.WithCancel(context.Background())
	h.d, err = newDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		deps{LLM: reg, Argon2: cheapArgon(), Integrations: []integration.Integration{stub}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if h.alice, err = h.d.auth.CreateUser(ctx, "alice", "alice-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if h.bob, err = h.d.auth.CreateUser(ctx, "bob", "bob-password", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.base = "http://" + ln.Addr().String()
	served := make(chan error, 1)
	go func() { served <- h.d.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		h.d.Close()
	})

	// The email tools are registered, email.send as destructive.
	var found bool
	for _, in := range h.d.integrations {
		if e, ok := in.(*email.Integration); ok {
			found = true
			for _, tl := range e.Tools() {
				if tl.Name == "email.send" && tl.Risk != integration.RiskDestructive {
					t.Errorf("email.send risk = %s", tl.Risk)
				}
			}
		}
	}
	if !found {
		t.Fatal("email integration not enabled")
	}

	stub.mu.Lock()
	emit, list := stub.emit, stub.list
	stub.mu.Unlock()
	if emit == nil || list == nil {
		t.Fatal("stub not wired")
	}

	// The lister reports exactly the users holding a credential.
	if ids, err := list(ctx); err != nil || len(ids) != 0 {
		t.Errorf("users before any credential = %v, %v", ids, err)
	}
	alice := h.login("alice", "alice-password")
	if c := h.do("PUT", "/v1/credentials/evstub/apikey?scope=user", alice, api.PutCredentialRequest{Secret: "k"}, nil); c != 200 {
		t.Fatalf("put credential: %d", c)
	}
	if c := h.do("PUT", "/v1/credentials/email/password?scope=user", alice, api.PutCredentialRequest{Secret: "pw",
		Fields: map[string]string{"host": "imap.example.test", "user": "alice@example.test"}}, nil); c != 200 {
		t.Fatalf("put email credential: %d", c)
	}
	if ids, err := list(ctx); err != nil || len(ids) != 1 || ids[0] != h.alice.ID {
		t.Errorf("users = %v, %v; want [alice]", ids, err)
	}

	// The emitter reaches on_event hooks, only in the integration's own
	// namespace and only for a single real user.
	got := make(chan hooks.Event, 4)
	if err := h.d.hooks.OnEvent("test-capture", func(_ context.Context, ev hooks.Event) { got <- ev }); err != nil {
		t.Fatal(err)
	}
	if err := emit(ctx, h.alice.ID, "email.received", json.RawMessage(`{}`)); err == nil {
		t.Error("evstub emitted into the email namespace")
	}
	if err := emit(ctx, 0, "evstub.ping", nil); err == nil {
		t.Error("event for user 0 (everyone) accepted")
	}
	if err := emit(ctx, h.alice.ID, "evstub.ping", json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-got:
		if ev.Name != "evstub.ping" || ev.UserID != h.alice.ID || string(ev.Payload) != `{"n":1}` {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered")
	}
}
