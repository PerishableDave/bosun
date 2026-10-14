package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/integration"
)

// Integration returns the tool-less "llm" pseudo-integration. The daemon
// lists it so an admin can store provider API keys in the vault with
// `bosun credentials set llm <provider> --shared` (its credential kinds are
// the configured provider names), and GET /v1/integrations shows where each
// key comes from. It never registers tools.
func (k *Keys) Integration() integration.Integration { return keysIntegration{k} }

type keysIntegration struct{ k *Keys }

func (i keysIntegration) names() []string {
	names := make([]string, 0, len(i.k.cfg))
	for n := range i.k.cfg {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (i keysIntegration) Manifest() integration.Manifest {
	return integration.Manifest{Name: CredentialIntegration, Version: "1", Scope: integration.ScopeShared, CredentialKinds: i.names()}
}

func (keysIntegration) Init(context.Context, integration.Config, integration.CredentialSource) error {
	return nil
}
func (keysIntegration) Tools() []integration.Tool { return nil }
func (keysIntegration) Close() error              { return nil }

// NeedsKey reports whether provider normally needs an API key: the hosted
// APIs (anthropic, openai), or any provider configured with api_key_env.
func (k *Keys) NeedsKey(provider string) bool {
	return provider == "anthropic" || provider == "openai" || k.cfg[provider].APIKeyEnv != ""
}

// AnyNeedsKey reports whether some configured provider needs an API key
// (the daemon lists the "llm" integration only then).
func (k *Keys) AnyNeedsKey() bool {
	for n := range k.cfg {
		if k.NeedsKey(n) {
			return true
		}
	}
	return false
}

// Status reports each provider's key source (never the key). A provider
// that needs a key and has none makes the integration degraded.
func (i keysIntegration) Status() integration.Status {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := integration.Status{State: integration.StatusOK}
	var parts []string
	for _, n := range i.names() {
		_, src, err := i.k.Resolve(ctx, n)
		switch {
		case err != nil:
			parts = append(parts, fmt.Sprintf("%s: vault error", n))
			st.State = integration.StatusDegraded
		case src == SourceVault:
			parts = append(parts, n+": vault")
		case src == SourceEnv:
			parts = append(parts, n+": $"+i.k.cfg[n].APIKeyEnv)
		case !i.k.NeedsKey(n):
			parts = append(parts, n+": no key needed")
		default:
			parts = append(parts, n+": no key")
			st.State = integration.StatusDegraded
		}
	}
	st.Message = strings.Join(parts, ", ")
	return st
}
