package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/llm"
)

// fakeVault is an in-memory llm/<provider> credential store.
type fakeVault struct {
	mu    sync.Mutex
	keys  map[string]string
	calls int
	err   error
}

func (f *fakeVault) lookup(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.keys[p], f.err
}

func (f *fakeVault) set(p, k string) {
	f.mu.Lock()
	f.keys[p] = k
	f.mu.Unlock()
}

func TestKeysResolutionOrder(t *testing.T) {
	t.Setenv("TEST_LLM_KEY", "from-env")
	cfg := config.Default()
	cfg.LLM.Providers = map[string]config.ProviderConfig{"anthropic": {APIKeyEnv: "TEST_LLM_KEY"}, "ollama": {}}
	fv := &fakeVault{keys: map[string]string{}}
	k := NewKeys(cfg, fv.lookup)
	now := time.Unix(1000, 0)
	k.now = func() time.Time { return now }
	ctx := context.Background()

	if key, src, err := k.Resolve(ctx, "anthropic"); err != nil || key != "from-env" || src != SourceEnv {
		t.Fatalf("env: %q %q %v", key, src, err)
	}
	if key, src, _ := k.Resolve(ctx, "ollama"); key != "" || src != SourceNone {
		t.Fatalf("none: %q %q", key, src)
	}
	// A vault key wins over the env var, without a restart (misses are not cached).
	fv.set("anthropic", "from-vault")
	if key, src, _ := k.Resolve(ctx, "anthropic"); key != "from-vault" || src != SourceVault {
		t.Fatalf("vault: %q %q", key, src)
	}
	// Hits are cached for KeyTTL.
	calls := fv.calls
	fv.set("anthropic", "rotated")
	if key, _, _ := k.Resolve(ctx, "anthropic"); key != "from-vault" || fv.calls != calls {
		t.Fatalf("cached: %q calls %d->%d", key, calls, fv.calls)
	}
	now = now.Add(KeyTTL + time.Second)
	if key, _, _ := k.Resolve(ctx, "anthropic"); key != "rotated" {
		t.Fatalf("after ttl: %q", key)
	}
	// Vault errors are reported, not silently replaced by the env var.
	now = now.Add(KeyTTL + time.Second)
	fv.err = errors.New("boom")
	if _, _, err := k.Resolve(ctx, "anthropic"); err == nil {
		t.Fatal("want vault error")
	}
}

func TestProviderUsesVaultKeyPerRequest(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("x-api-key"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.LLM.Providers = map[string]config.ProviderConfig{"anthropic": {BaseURL: srv.URL}}
	fv := &fakeVault{keys: map[string]string{}}
	keys := NewKeys(cfg, fv.lookup)
	reg, err := FromConfig(cfg, WithKeys(keys))
	if err != nil {
		t.Fatal(err)
	}
	p, model, err := reg.Resolve("anthropic/claude-test")
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{Model: model, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}}}}}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	fv.set("anthropic", "sk-vault")
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "" || seen[1] != "sk-vault" {
		t.Fatalf("keys sent = %q", seen)
	}
}

func TestLLMIntegration(t *testing.T) {
	t.Setenv("TEST_OAI_KEY", "x")
	cfg := config.Default()
	cfg.LLM.Providers = map[string]config.ProviderConfig{
		"anthropic": {APIKeyEnv: "UNSET_TEST_VAR_FOR_BOSUN"}, "ollama": {}, "openai": {APIKeyEnv: "TEST_OAI_KEY"},
	}
	fv := &fakeVault{keys: map[string]string{}}
	in := NewKeys(cfg, fv.lookup).Integration()
	m := in.Manifest()
	if m.Name != "llm" || m.Scope != integration.ScopeShared || strings.Join(m.CredentialKinds, ",") != "anthropic,ollama,openai" {
		t.Fatalf("manifest = %+v", m)
	}
	st := in.(integration.StatusReporter).Status()
	if st.State != integration.StatusDegraded || !strings.Contains(st.Message, "anthropic: no key") ||
		!strings.Contains(st.Message, "openai: $TEST_OAI_KEY") || !strings.Contains(st.Message, "ollama: no key needed") {
		t.Fatalf("status = %+v", st)
	}
	fv.set("anthropic", "k")
	if st := in.(integration.StatusReporter).Status(); st.State != integration.StatusOK || !strings.Contains(st.Message, "anthropic: vault") {
		t.Fatalf("status = %+v", st)
	}
}
