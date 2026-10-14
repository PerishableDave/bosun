package providers

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/config"
)

// CredentialIntegration is the tool-less pseudo-integration LLM API keys are
// stored under in the vault, as shared credentials whose kind is the
// provider name: `bosun credentials set llm anthropic --shared`.
const CredentialIntegration = "llm"

// KeyTTL is how long a key found in the vault is cached. A key set or
// rotated with `bosun credentials set llm <provider>` takes effect within
// this long, without a restart. Misses are not cached.
const KeyTTL = 30 * time.Second

// VaultLookup returns the shared vault credential llm/<provider>'s secret,
// or "" (and no error) when there is none.
type VaultLookup func(ctx context.Context, provider string) (string, error)

// Key sources reported by Keys.Resolve.
const (
	SourceVault = "vault"
	SourceEnv   = "env"
	SourceNone  = ""
)

// Keys resolves provider API keys: the vault's shared llm/<provider>
// credential first, then the provider's api_key_env variable, else none.
// Vault hits are cached for KeyTTL.
type Keys struct {
	cfg   map[string]config.ProviderConfig
	vault VaultLookup
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedKey
}

type cachedKey struct {
	key     string
	expires time.Time
}

// NewKeys returns a resolver for cfg's providers. vault may be nil (env
// only).
func NewKeys(cfg *config.Config, vault VaultLookup) *Keys {
	return &Keys{cfg: cfg.LLM.Providers, vault: vault, ttl: KeyTTL, now: time.Now, cache: map[string]cachedKey{}}
}

// Resolve returns provider's key and where it came from (SourceVault,
// SourceEnv, or SourceNone with key ""). A vault error is returned rather
// than silently falling back to the environment.
func (k *Keys) Resolve(ctx context.Context, provider string) (key, source string, err error) {
	if k.vault != nil {
		k.mu.Lock()
		c, ok := k.cache[provider]
		k.mu.Unlock()
		if ok && k.now().Before(c.expires) {
			return c.key, SourceVault, nil
		}
		v, err := k.vault(ctx, provider)
		if err != nil {
			return "", SourceNone, err
		}
		if v != "" {
			k.mu.Lock()
			k.cache[provider] = cachedKey{key: v, expires: k.now().Add(k.ttl)}
			k.mu.Unlock()
			return v, SourceVault, nil
		}
		k.mu.Lock()
		delete(k.cache, provider)
		k.mu.Unlock()
	}
	if env := k.cfg[provider].APIKeyEnv; env != "" {
		if v := os.Getenv(env); v != "" {
			return v, SourceEnv, nil
		}
	}
	return "", SourceNone, nil
}

// Forget drops provider's cached key (e.g. after it was deleted).
func (k *Keys) Forget(provider string) {
	k.mu.Lock()
	delete(k.cache, provider)
	k.mu.Unlock()
}

// KeyFunc returns the per-request key function for provider.
func (k *Keys) KeyFunc(provider string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		key, _, err := k.Resolve(ctx, provider)
		return key, err
	}
}
