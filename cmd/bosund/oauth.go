package main

import (
	"sort"
	"strings"

	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/oauth"
	"github.com/dperedo/bosun/internal/vault"
)

// setupOAuth builds the account-linking manager from the oauth: section and
// registers it as the vault's resolver for "oauth2:" credential kinds, so any
// integration asking its CredentialSource for "oauth2:google" gets the calling
// user's linked account. It must run before integrations are initialised.
// When providers are configured, the tool-less "oauth" pseudo-integration is
// listed so an admin can store client secrets with `bosun credentials set`.
func (d *daemon) setupOAuth(vlt *vault.Vault, auditLog audit.Logger) (*oauth.Manager, error) {
	clients, err := oauth.ClientsFromConfig(d.cfg.OAuth, vlt)
	if err != nil {
		return nil, err
	}
	m, err := oauth.New(oauth.Options{Vault: vlt, Clients: clients, Audit: auditLog, Log: d.log})
	if err != nil {
		return nil, err
	}
	m.Register(vlt)
	if len(clients) > 0 {
		d.integrations = append(d.integrations, m.Integration())
		names := make([]string, 0, len(clients))
		for n := range clients {
			names = append(names, n)
		}
		sort.Strings(names)
		d.log.Info("oauth providers configured", "providers", strings.Join(names, ","))
	}
	return m, nil
}
