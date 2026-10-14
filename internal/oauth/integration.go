package oauth

import (
	"context"
	"sort"
	"strings"

	"github.com/perishabledave/bosun/internal/integration"
)

// SecretsIntegration is a tool-less pseudo-integration named "oauth" that the
// daemon lists so an admin can store a provider's client secret in the vault
// with `bosun credentials set oauth client_secret:<provider>` (shared scope)
// instead of an environment variable. It never registers tools.
type SecretsIntegration struct{ m *Manager }

// Integration returns the "oauth" pseudo-integration for m.
func (m *Manager) Integration() *SecretsIntegration { return &SecretsIntegration{m: m} }

// Manifest implements integration.Integration.
func (s *SecretsIntegration) Manifest() integration.Manifest {
	return integration.Manifest{Name: IntegrationName, Version: "1", Scope: integration.ScopeShared,
		CredentialKinds: []string{strings.TrimSuffix(ClientSecretKindPrefix, ":")}}
}

// Init implements integration.Integration.
func (s *SecretsIntegration) Init(context.Context, integration.Config, integration.CredentialSource) error {
	return nil
}

// Tools implements integration.Integration (none).
func (s *SecretsIntegration) Tools() []integration.Tool { return nil }

// Close implements integration.Integration.
func (s *SecretsIntegration) Close() error { return nil }

// Status implements integration.StatusReporter.
func (s *SecretsIntegration) Status() integration.Status {
	var names []string
	for n := range s.m.clients {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return integration.Status{State: integration.StatusOK, Message: "no OAuth providers configured (see oauth: in bosun.yaml)"}
	}
	return integration.Status{State: integration.StatusOK, Message: "account linking via `bosun connect`: " + strings.Join(names, ", ")}
}
