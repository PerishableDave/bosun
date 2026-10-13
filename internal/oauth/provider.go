// Package oauth links users' accounts at OAuth2 providers (Google,
// Microsoft) to Bosun and hands integrations fresh access tokens.
//
// Flow: `bosun connect google` calls POST /v1/oauth/google/start; the daemon
// (Manager.Start) creates a single-use state with a PKCE verifier and returns
// the provider's authorization URL. The browser is redirected back to the
// daemon's GET /oauth/callback, where Manager.Callback exchanges the code
// (with the verifier), learns the account email from the userinfo endpoint
// and stores the tokens in the vault as the calling user's credential
// ("oauth", "oauth2:<provider>").
//
// Integrations never see this package: Manager registers itself with the
// vault as the KindResolver for "oauth2:", so any integration whose
// CredentialSource is asked for "oauth2:google" receives the calling user's
// Google access token, refreshed transparently when it is about to expire.
// See Credential contract on Manager.Get.
//
// Only the standard library is used.
package oauth

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Provider describes an OAuth2 authorization server.
type Provider struct {
	Name        string
	AuthURL     string
	TokenURL    string
	UserinfoURL string // OIDC userinfo; the account email is read from it
	RevokeURL   string // RFC 7009 revocation; empty if the provider has none
	// BaseScopes are always requested (identity and offline access).
	BaseScopes []string
	// Bundles map a short name integrations ask for ("email", "calendar")
	// to the provider scopes it needs.
	Bundles map[string][]string
	// DefaultBundles are used when the user names none.
	DefaultBundles []string
	// AuthParams are extra authorization request parameters (e.g. Google's
	// access_type=offline so a refresh token is issued).
	AuthParams map[string]string
}

// Google scope bundles. "email" includes gmail.modify because archiving and
// labelling (removing INBOX, adding labels) need it; readonly+send alone
// cannot change a message. Gmail over IMAP/SMTP (XOAUTH2) accepts only the
// full https://mail.google.com/ scope, which is the separate "imap" bundle;
// likewise Google's CalDAV endpoint wants the full calendar scope ("caldav").
func googleProvider() Provider {
	const g = "https://www.googleapis.com/auth/"
	return Provider{
		Name:        "google",
		AuthURL:     "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:    "https://oauth2.googleapis.com/token",
		UserinfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		RevokeURL:   "https://oauth2.googleapis.com/revoke",
		BaseScopes:  []string{"openid", "email"},
		Bundles: map[string][]string{
			"email":    {g + "gmail.readonly", g + "gmail.send", g + "gmail.modify"},
			"calendar": {g + "calendar.events", g + "calendar.readonly"},
			"tasks":    {g + "tasks"},
			"drive":    {g + "drive.readonly"},
			"imap":     {"https://mail.google.com/"},
			"caldav":   {g + "calendar"},
		},
		DefaultBundles: []string{"email", "calendar"},
		// offline: issue a refresh token; consent: re-issue it on every
		// connect (Google only sends one on first consent otherwise);
		// include_granted_scopes: incremental authorization.
		AuthParams: map[string]string{"access_type": "offline", "prompt": "consent", "include_granted_scopes": "true"},
	}
}

// Microsoft identity platform v2, "common" tenant (work, school and
// personal accounts), as a public client: PKCE, no secret. Microsoft has no
// RFC 7009 token revocation endpoint, so unlinking only deletes the tokens.
func microsoftProvider() Provider {
	const gr = "https://graph.microsoft.com/"
	return Provider{
		Name:        "microsoft",
		AuthURL:     "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		TokenURL:    "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		UserinfoURL: "https://graph.microsoft.com/oidc/userinfo",
		BaseScopes:  []string{"openid", "email", "profile", "offline_access"},
		Bundles: map[string][]string{
			"email":    {gr + "Mail.ReadWrite", gr + "Mail.Send"},
			"calendar": {gr + "Calendars.ReadWrite"},
			"tasks":    {gr + "Tasks.ReadWrite"},
			"drive":    {gr + "Files.Read"},
		},
		DefaultBundles: []string{"email", "calendar"},
		AuthParams:     map[string]string{"prompt": "select_account"},
	}
}

// Builtin returns the built-in provider named name.
func Builtin(name string) (Provider, bool) {
	switch name {
	case "google":
		return googleProvider(), true
	case "microsoft":
		return microsoftProvider(), true
	}
	return Provider{}, false
}

// BuiltinNames lists the built-in providers.
func BuiltinNames() []string { return []string{"google", "microsoft"} }

// BundleNames returns p's bundle names, sorted.
func (p Provider) BundleNames() []string {
	out := make([]string, 0, len(p.Bundles))
	for k := range p.Bundles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ExpandScopes turns bundle names into the deduplicated scope list to
// request: BaseScopes, then each bundle's scopes in order. An empty list
// selects DefaultBundles. An entry starting with "https://" is taken as a
// literal provider scope. Unknown bundle names are an error.
func (p Provider) ExpandScopes(bundles []string) ([]string, error) {
	if len(bundles) == 0 {
		bundles = p.DefaultBundles
	}
	out := slices.Clone(p.BaseScopes)
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, b := range bundles {
		b = strings.TrimSpace(b)
		switch {
		case b == "":
			continue
		case strings.HasPrefix(b, "https://"):
			add(b)
		default:
			scopes, ok := p.Bundles[b]
			if !ok {
				return nil, fmt.Errorf("unknown %s scope bundle %q (available: %s)", p.Name, b, strings.Join(p.BundleNames(), ", "))
			}
			for _, s := range scopes {
				add(s)
			}
		}
	}
	return out, nil
}
