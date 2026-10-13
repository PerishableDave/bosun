package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/vault"
)

// Vault names and timings.
const (
	// IntegrationName is the pseudo-integration the tokens are stored under:
	// vault row (user, "oauth", "oauth2:<provider>").
	IntegrationName = "oauth"
	// KindPrefix is the credential-kind prefix the vault routes to Manager.
	KindPrefix = "oauth2:"
	// ClientSecretKindPrefix names the shared vault credential holding a
	// provider's client secret: ("oauth", "client_secret:<provider>").
	ClientSecretKindPrefix = "client_secret:"
	// CallbackPath is where the provider redirects the browser (served by
	// bosund outside /v1, without auth).
	CallbackPath = "/oauth/callback"
	// StateTTL bounds how long an authorization may take.
	StateTTL = 10 * time.Minute
	// RefreshSkew: an access token this close to expiry is refreshed
	// before it is handed out.
	RefreshSkew = 2 * time.Minute
	// maxPendingPerUser caps outstanding authorizations per user.
	maxPendingPerUser = 10
	// stateRetention keeps finished states around for status polling.
	stateRetention = 5 * time.Minute
)

// Kind returns the credential kind for provider, e.g. "oauth2:google".
func Kind(provider string) string { return KindPrefix + provider }

// Credential field names. Integrations see FieldEmail, FieldScopes and
// FieldProvider; FieldRefreshToken is stored but never handed out.
const (
	FieldEmail        = "email"
	FieldScopes       = "scopes"
	FieldProvider     = "provider"
	FieldRefreshToken = "refresh_token"
	FieldTokenType    = "token_type"
)

// Errors. API handlers map them to status codes.
var (
	ErrUnknownProvider = errors.New("oauth: unknown provider")
	ErrNotConfigured   = errors.New("oauth: provider not configured")
	ErrInvalid         = errors.New("oauth: invalid request")
	ErrBadState        = errors.New("oauth: invalid, expired or already used state")
	ErrNotLinked       = errors.New("oauth: no linked account")
	ErrTooMany         = errors.New("oauth: too many pending authorizations")
)

// Vault is the subset of *vault.Vault the manager needs.
type Vault interface {
	Put(ctx context.Context, userID *int64, integrationName, kind string, cred integration.Credential) error
	Get(ctx context.Context, userID *int64, integrationName, kind string) (integration.Credential, error)
	Delete(ctx context.Context, userID *int64, integrationName, kind string) error
	List(ctx context.Context, userID *int64, integrationName string) ([]vault.Info, error)
}

var _ Vault = (*vault.Vault)(nil)

// Client is a configured OAuth client at a provider.
type Client struct {
	Provider Provider
	ClientID string
	// Secret returns the client secret; "" means a public client (PKCE
	// only). Nil is the same as returning "".
	Secret func(ctx context.Context) (string, error)
	// SecretRequired makes Start fail early when Secret yields "" (Google
	// clients, including "Desktop app" ones, are confidential).
	SecretRequired bool
}

// Options configure a Manager.
type Options struct {
	Vault   Vault             // required
	Clients map[string]Client // configured providers, by name
	// HTTPClient talks to the providers (default: 30s timeout).
	HTTPClient *http.Client
	Audit      audit.Logger // optional; link/unlink are recorded as kind config
	Log        *slog.Logger
	Now        func() time.Time
}

// Manager runs authorization flows and serves tokens. It is safe for
// concurrent use.
type Manager struct {
	v       Vault
	clients map[string]Client
	hc      *http.Client
	audit   audit.Logger
	log     *slog.Logger
	now     func() time.Time

	mu     sync.Mutex
	states map[string]*pending

	flMu    sync.Mutex
	flights map[string]*flight
}

// New returns a Manager.
func New(o Options) (*Manager, error) {
	if o.Vault == nil {
		return nil, errors.New("oauth: Vault is required")
	}
	m := &Manager{v: o.Vault, clients: map[string]Client{}, hc: o.HTTPClient, audit: o.Audit, log: o.Log, now: o.Now,
		states: map[string]*pending{}, flights: map[string]*flight{}}
	for name, c := range o.Clients {
		if c.Provider.Name == "" {
			c.Provider.Name = name
		}
		if c.ClientID == "" || c.Provider.AuthURL == "" || c.Provider.TokenURL == "" {
			return nil, fmt.Errorf("oauth: provider %s: client id, auth and token URLs are required", name)
		}
		m.clients[name] = c
	}
	if m.hc == nil {
		m.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m, nil
}

// SecretLookup is the vault method ClientsFromConfig reads shared client
// secrets with.
type SecretLookup interface {
	Get(ctx context.Context, userID *int64, integrationName, kind string) (integration.Credential, error)
}

// ClientsFromConfig builds the clients configured under `oauth:` in
// bosun.yaml. A provider name that is not built in must set auth_url,
// token_url and userinfo_url. The client secret is read on each use from the
// environment variable client_secret_env, else from the shared vault
// credential ("oauth", "client_secret:<provider>"), else none.
func ClientsFromConfig(cfg map[string]config.OAuthProviderConfig, secrets SecretLookup) (map[string]Client, error) {
	out := map[string]Client{}
	for name, pc := range cfg {
		p, builtin := Builtin(name)
		if !builtin {
			if pc.AuthURL == "" || pc.TokenURL == "" || pc.UserinfoURL == "" {
				return nil, fmt.Errorf("oauth.%s: not a built-in provider (%s); set auth_url, token_url and userinfo_url",
					name, strings.Join(BuiltinNames(), ", "))
			}
			p = Provider{Name: name, BaseScopes: []string{"openid", "email"}, Bundles: map[string][]string{}}
		}
		for dst, src := range map[*string]string{&p.AuthURL: pc.AuthURL, &p.TokenURL: pc.TokenURL,
			&p.UserinfoURL: pc.UserinfoURL, &p.RevokeURL: pc.RevokeURL} {
			if src != "" {
				*dst = src
			}
		}
		env, kind := pc.ClientSecretEnv, ClientSecretKindPrefix+name
		out[name] = Client{
			Provider: p, ClientID: pc.ClientID, SecretRequired: name == "google",
			Secret: func(ctx context.Context) (string, error) {
				if env != "" {
					if s := os.Getenv(env); s != "" {
						return s, nil
					}
				}
				if secrets == nil {
					return "", nil
				}
				c, err := secrets.Get(ctx, nil, IntegrationName, kind)
				if errors.Is(err, vault.ErrNotFound) {
					return "", nil
				}
				if err != nil {
					return "", err
				}
				return c.Secret, nil
			},
		}
	}
	return out, nil
}

// Register makes m the vault's resolver for every "oauth2:" kind, so any
// integration's CredentialSource yields the calling user's linked account.
func (m *Manager) Register(v *vault.Vault) { v.RegisterResolver(KindPrefix, m) }

// ProviderInfo describes a provider for listings.
type ProviderInfo struct {
	Name           string              `json:"name"`
	Configured     bool                `json:"configured"`
	Bundles        map[string][]string `json:"bundles"`
	DefaultBundles []string            `json:"default_bundles"`
}

// Providers lists the built-in and configured providers.
func (m *Manager) Providers() []ProviderInfo {
	seen := map[string]bool{}
	var out []ProviderInfo
	for name, c := range m.clients {
		seen[name] = true
		out = append(out, ProviderInfo{Name: name, Configured: true, Bundles: c.Provider.Bundles, DefaultBundles: c.Provider.DefaultBundles})
	}
	for _, name := range BuiltinNames() {
		if !seen[name] {
			p, _ := Builtin(name)
			out = append(out, ProviderInfo{Name: name, Bundles: p.Bundles, DefaultBundles: p.DefaultBundles})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) client(provider string) (Client, error) {
	if c, ok := m.clients[provider]; ok {
		return c, nil
	}
	if _, ok := Builtin(provider); ok {
		return Client{}, fmt.Errorf("%w: add oauth.%s.client_id to bosun.yaml (see README, \"Connect your Google account\")", ErrNotConfigured, provider)
	}
	return Client{}, fmt.Errorf("%w %q", ErrUnknownProvider, provider)
}

func (c Client) secret(ctx context.Context) (string, error) {
	if c.Secret == nil {
		return "", nil
	}
	s, err := c.Secret(ctx)
	if err != nil {
		return "", fmt.Errorf("oauth: %s client secret: %w", c.Provider.Name, err)
	}
	return s, nil
}

// --- authorization flow ---

type pending struct {
	state       string
	provider    string
	userID      int64
	verifier    string
	scopes      []string
	redirectURL string
	expires     time.Time

	used     bool // the callback consumed it (single use)
	finished time.Time
	account  *Account
	errMsg   string
}

// StartResult is returned by Start.
type StartResult struct {
	Provider    string    `json:"provider"`
	State       string    `json:"state"`
	AuthURL     string    `json:"auth_url"`
	RedirectURL string    `json:"redirect_url"`
	Scopes      []string  `json:"scopes"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge is the S256 code challenge for verifier (RFC 7636).
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Start begins linking userID's account at provider. bundles are scope bundle
// names (empty: the provider's defaults). redirectURL is the daemon's
// callback URL as the browser will reach it (…/oauth/callback); it must be
// registered with the provider.
func (m *Manager) Start(ctx context.Context, userID int64, provider string, bundles []string, redirectURL string) (*StartResult, error) {
	c, err := m.client(provider)
	if err != nil {
		return nil, err
	}
	if userID <= 0 {
		return nil, fmt.Errorf("%w: no user", ErrInvalid)
	}
	u, err := url.Parse(redirectURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: bad redirect URL", ErrInvalid)
	}
	scopes, err := c.Provider.ExpandScopes(bundles)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if c.SecretRequired {
		s, err := c.secret(ctx)
		if err != nil {
			return nil, err
		}
		if s == "" {
			return nil, fmt.Errorf("%w: %s needs a client secret: set the environment variable named by oauth.%s.client_secret_env "+
				"for bosund, or store it with `bosun credentials set oauth %s%s` (admin)", ErrNotConfigured, provider, provider,
				ClientSecretKindPrefix, provider)
		}
	}
	state, err := randomToken()
	if err != nil {
		return nil, err
	}
	verifier, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := m.now()
	rec := &pending{state: state, provider: provider, userID: userID, verifier: verifier, scopes: scopes,
		redirectURL: redirectURL, expires: now.Add(StateTTL)}

	m.mu.Lock()
	m.purgeLocked(now)
	n := 0
	for _, p := range m.states {
		if p.userID == userID && !p.used {
			n++
		}
	}
	if n >= maxPendingPerUser {
		m.mu.Unlock()
		return nil, ErrTooMany
	}
	m.states[state] = rec
	m.mu.Unlock()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.ClientID},
		"redirect_uri":          {redirectURL},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	for k, v := range c.Provider.AuthParams {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(c.Provider.AuthURL, "?") {
		sep = "&"
	}
	return &StartResult{Provider: provider, State: state, AuthURL: c.Provider.AuthURL + sep + q.Encode(),
		RedirectURL: redirectURL, Scopes: scopes, ExpiresAt: rec.expires}, nil
}

// purgeLocked drops states that expired (unfinished) or finished long ago.
func (m *Manager) purgeLocked(now time.Time) {
	for k, p := range m.states {
		if now.After(p.expires.Add(stateRetention)) || (!p.finished.IsZero() && now.After(p.finished.Add(stateRetention))) {
			delete(m.states, k)
		}
	}
}

// Flow statuses reported by Status.
const (
	StatusPending  = "pending"
	StatusComplete = "complete"
	StatusError    = "error"
	StatusExpired  = "expired"
)

// FlowStatus is the result of Status.
type FlowStatus struct {
	Status  string   `json:"status"`
	Account *Account `json:"account,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Status reports the progress of userID's authorization identified by state.
// Another user's state is reported as ErrBadState.
func (m *Manager) Status(userID int64, provider, state string) (FlowStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.purgeLocked(now)
	p, ok := m.states[state]
	if !ok || p.userID != userID || p.provider != provider {
		return FlowStatus{}, ErrBadState
	}
	switch {
	case p.account != nil:
		a := *p.account
		return FlowStatus{Status: StatusComplete, Account: &a}, nil
	case p.errMsg != "":
		return FlowStatus{Status: StatusError, Error: p.errMsg}, nil
	case !p.used && now.After(p.expires):
		return FlowStatus{Status: StatusExpired, Error: "the authorization was not completed within 10 minutes"}, nil
	}
	return FlowStatus{Status: StatusPending}, nil
}

// CallbackResult describes a completed callback, for the browser page.
type CallbackResult struct {
	UserID   int64
	Provider string
	Account  Account
}

// Callback completes an authorization: q is the callback query (code, state,
// or error). The state is consumed on first use whatever the outcome, so a
// replayed callback fails with ErrBadState. On success the tokens are stored
// in the vault for the user who started the flow.
func (m *Manager) Callback(ctx context.Context, q url.Values) (*CallbackResult, error) {
	state := q.Get("state")
	m.mu.Lock()
	p, ok := m.states[state]
	if !ok || p.used || m.now().After(p.expires) {
		m.mu.Unlock()
		return nil, ErrBadState
	}
	p.used = true
	m.mu.Unlock()

	acct, err := m.complete(ctx, p, q)
	m.mu.Lock()
	p.finished = m.now()
	if err != nil {
		p.errMsg = err.Error()
	} else {
		p.account = acct
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &CallbackResult{UserID: p.userID, Provider: p.provider, Account: *acct}, nil
}

func (m *Manager) complete(ctx context.Context, p *pending, q url.Values) (*Account, error) {
	if e := q.Get("error"); e != "" {
		msg := "authorization was not granted (" + clip(e, 64) + ")"
		if d := q.Get("error_description"); d != "" {
			msg += ": " + clip(d, 200)
		}
		return nil, errors.New(msg)
	}
	code := q.Get("code")
	if code == "" {
		return nil, fmt.Errorf("%w: callback without code", ErrInvalid)
	}
	c, err := m.client(p.provider)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.redirectURL},
		"code_verifier": {p.verifier},
	}
	tr, err := m.tokenRequest(ctx, c, form)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	email, err := m.accountEmail(ctx, c, tr)
	if err != nil {
		return nil, err
	}
	now := m.now()
	scopes := tr.Scope
	if scopes == "" {
		scopes = strings.Join(p.scopes, " ")
	}
	cred := integration.Credential{
		Kind:   Kind(p.provider),
		Secret: tr.AccessToken,
		Fields: map[string]string{
			FieldEmail: email, FieldScopes: scopes, FieldProvider: p.provider,
			FieldRefreshToken: tr.RefreshToken, FieldTokenType: tr.TokenType,
		},
		Expiry: tr.expiry(now),
	}
	uid := p.userID
	if cred.Fields[FieldRefreshToken] == "" {
		// Keep a previous refresh token for the same account (a provider
		// may only issue one on first consent).
		if old, err := m.v.Get(ctx, &uid, IntegrationName, Kind(p.provider)); err == nil && old.Fields[FieldEmail] == email {
			cred.Fields[FieldRefreshToken] = old.Fields[FieldRefreshToken]
		}
	}
	if err := m.v.Put(ctx, &uid, IntegrationName, Kind(p.provider), cred); err != nil {
		return nil, fmt.Errorf("store tokens: %w", err)
	}
	m.record(ctx, uid, "oauth account linked", p.provider, email, scopes)
	acct := Account{Provider: p.provider, Email: email, Scopes: strings.Fields(scopes), Expiry: cred.Expiry,
		LinkedAt: now, UpdatedAt: now}
	if infos, err := m.v.List(ctx, &uid, IntegrationName); err == nil {
		for _, in := range infos {
			if in.Kind == Kind(p.provider) {
				acct.LinkedAt, acct.UpdatedAt = in.CreatedAt, in.UpdatedAt
			}
		}
	}
	return &acct, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// --- token endpoint ---

type tokenResponse struct {
	AccessToken  string      `json:"access_token"`
	TokenType    string      `json:"token_type"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    json.Number `json:"expires_in"`
	Scope        string      `json:"scope"`
	IDToken      string      `json:"id_token"`
}

func (t *tokenResponse) expiry(now time.Time) time.Time {
	secs, err := strconv.ParseInt(t.ExpiresIn.String(), 10, 64)
	if err != nil || secs <= 0 {
		secs = 3600 // conservative default when the provider omits it
	}
	return now.Add(time.Duration(secs) * time.Second).UTC()
}

// TokenError is an error response from a token endpoint (RFC 6749 5.2).
type TokenError struct {
	Status      int
	Code        string // e.g. "invalid_grant"
	Description string
}

func (e *TokenError) Error() string {
	s := fmt.Sprintf("token endpoint: HTTP %d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// tokenRequest posts form (plus client credentials) to the token endpoint.
// Token values are never included in errors.
func (m *Manager) tokenRequest(ctx context.Context, c Client, form url.Values) (*tokenResponse, error) {
	form.Set("client_id", c.ClientID)
	secret, err := c.secret(ctx)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		form.Set("client_secret", secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Provider.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("token endpoint: read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		return nil, &TokenError{Status: resp.StatusCode, Code: clip(e.Error, 64), Description: clip(e.Description, 200)}
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("token endpoint: malformed response")
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint: response has no access_token")
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return nil, fmt.Errorf("token endpoint: unsupported token_type %q", clip(tr.TokenType, 32))
	}
	return &tr, nil
}

// accountEmail asks the userinfo endpoint for the account's email, falling
// back to the id_token's claims (received directly from the token endpoint
// over TLS, so its signature need not be checked; OIDC Core 3.1.3.7).
func (m *Manager) accountEmail(ctx context.Context, c Client, tr *tokenResponse) (string, error) {
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		UPN               string `json:"upn"`
	}
	pick := func() string {
		for _, s := range []string{claims.Email, claims.PreferredUsername, claims.UPN} {
			if s != "" {
				return clip(s, 320)
			}
		}
		return ""
	}
	var uerr error
	if c.Provider.UserinfoURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Provider.UserinfoURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+tr.AccessToken)
		req.Header.Set("Accept", "application/json")
		resp, err := m.hc.Do(req)
		if err != nil {
			uerr = fmt.Errorf("userinfo: %w", err)
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				uerr = fmt.Errorf("userinfo: HTTP %d", resp.StatusCode)
			} else if err := json.Unmarshal(body, &claims); err != nil {
				uerr = fmt.Errorf("userinfo: malformed response")
			} else if e := pick(); e != "" {
				return e, nil
			}
		}
	}
	if parts := strings.Split(tr.IDToken, "."); len(parts) == 3 {
		if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil && json.Unmarshal(b, &claims) == nil {
			if e := pick(); e != "" {
				return e, nil
			}
		}
	}
	if uerr != nil {
		return "", uerr
	}
	return "", errors.New("the provider did not report the account's email (was the email scope granted?)")
}

// --- tokens for integrations ---

// Account describes a linked account. It never carries tokens.
type Account struct {
	Provider string   `json:"provider"`
	Email    string   `json:"email"`
	Scopes   []string `json:"scopes"`
	// Expiry is the current access token's expiry (it is refreshed on use).
	Expiry    time.Time `json:"expiry"`
	LinkedAt  time.Time `json:"linked_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func reconnect(provider string) string {
	return fmt.Sprintf("run `bosun connect %s` again", provider)
}

// Get returns userID's credential for provider with a valid access token,
// refreshing (and persisting) it first when it expires within RefreshSkew.
// Concurrent refreshes for the same user and provider are collapsed into
// one. This is what every integration receives for kind "oauth2:<provider>":
//
//	integration.Credential{
//		Kind:   "oauth2:google",
//		Secret: "<access token>",
//		Fields: map[string]string{"email": "<account email>", "scopes": "<space-separated granted scopes>", "provider": "google"},
//		Expiry: <access token expiry>,
//	}
//
// The refresh token is never handed out. Errors wrap
// integration.ErrNoCredential when nothing is linked or the grant was
// revoked (the message tells the user to run `bosun connect <provider>`).
func (m *Manager) Get(ctx context.Context, userID int64, provider string) (integration.Credential, error) {
	if userID <= 0 {
		return integration.Credential{}, fmt.Errorf("oauth: no calling user: %w", integration.ErrNoCredential)
	}
	cred, err := m.load(ctx, userID, provider)
	if err != nil {
		return integration.Credential{}, err
	}
	if m.needsRefresh(cred) {
		if cred, err = m.refresh(ctx, userID, provider); err != nil {
			return integration.Credential{}, err
		}
	}
	return public(provider, cred), nil
}

// Resolve implements vault.KindResolver for "oauth2:<provider>".
func (m *Manager) Resolve(ctx context.Context, owner *int64, kind string) (integration.Credential, error) {
	provider := strings.TrimPrefix(kind, KindPrefix)
	if owner == nil {
		return integration.Credential{}, fmt.Errorf("%s: linked accounts are per user and not available to shared integrations: %w",
			kind, integration.ErrNoCredential)
	}
	return m.Get(ctx, *owner, provider)
}

func public(provider string, c integration.Credential) integration.Credential {
	return integration.Credential{
		Kind:   Kind(provider),
		Secret: c.Secret,
		Fields: map[string]string{FieldEmail: c.Fields[FieldEmail], FieldScopes: c.Fields[FieldScopes], FieldProvider: provider},
		Expiry: c.Expiry,
	}
}

func (m *Manager) load(ctx context.Context, userID int64, provider string) (integration.Credential, error) {
	c, err := m.v.Get(ctx, &userID, IntegrationName, Kind(provider))
	if errors.Is(err, vault.ErrNotFound) {
		return c, fmt.Errorf("no %s account is linked; run `bosun connect %s`: %w", provider, provider, integration.ErrNoCredential)
	}
	return c, err
}

func (m *Manager) needsRefresh(c integration.Credential) bool {
	return c.Expiry.IsZero() || !m.now().Add(RefreshSkew).Before(c.Expiry)
}

type flight struct {
	done chan struct{}
	cred integration.Credential
	err  error
}

// refresh refreshes once per user+provider at a time; other callers wait for
// that result (or their own ctx).
func (m *Manager) refresh(ctx context.Context, userID int64, provider string) (integration.Credential, error) {
	key := strconv.FormatInt(userID, 10) + "/" + provider
	m.flMu.Lock()
	f, ok := m.flights[key]
	if !ok {
		f = &flight{done: make(chan struct{})}
		m.flights[key] = f
		go func() {
			// Detached from the first caller so its cancellation does not
			// fail everyone else waiting on the same refresh.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			f.cred, f.err = m.doRefresh(rctx, userID, provider)
			m.flMu.Lock()
			delete(m.flights, key)
			m.flMu.Unlock()
			close(f.done)
		}()
	}
	m.flMu.Unlock()
	select {
	case <-f.done:
		return f.cred, f.err
	case <-ctx.Done():
		return integration.Credential{}, ctx.Err()
	}
}

func (m *Manager) doRefresh(ctx context.Context, userID int64, provider string) (integration.Credential, error) {
	cred, err := m.load(ctx, userID, provider) // another flight may have refreshed it
	if err != nil || !m.needsRefresh(cred) {
		return cred, err
	}
	rt := cred.Fields[FieldRefreshToken]
	if rt == "" {
		return integration.Credential{}, fmt.Errorf("the %s access token expired and there is no refresh token; %s: %w",
			provider, reconnect(provider), integration.ErrNoCredential)
	}
	c, err := m.client(provider)
	if err != nil {
		return integration.Credential{}, err
	}
	tr, err := m.tokenRequest(ctx, c, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}})
	if err != nil {
		var te *TokenError
		if errors.As(err, &te) && (te.Code == "invalid_grant" || te.Code == "unauthorized_client" || te.Status == http.StatusUnauthorized) {
			m.log.WarnContext(ctx, "oauth refresh token rejected", "user_id", userID, "provider", provider, "error", te.Code)
			return integration.Credential{}, fmt.Errorf("%s access for %s was revoked or has expired; %s: %w",
				provider, cred.Fields[FieldEmail], reconnect(provider), integration.ErrNoCredential)
		}
		return integration.Credential{}, fmt.Errorf("oauth: refresh %s token: %w", provider, err)
	}
	fields := map[string]string{}
	for k, v := range cred.Fields {
		fields[k] = v
	}
	if tr.RefreshToken != "" { // rotated (Microsoft rotates on every refresh)
		fields[FieldRefreshToken] = tr.RefreshToken
	}
	if tr.Scope != "" {
		fields[FieldScopes] = tr.Scope
	}
	if tr.TokenType != "" {
		fields[FieldTokenType] = tr.TokenType
	}
	next := integration.Credential{Kind: Kind(provider), Secret: tr.AccessToken, Fields: fields, Expiry: tr.expiry(m.now())}
	if err := m.v.Put(ctx, &userID, IntegrationName, Kind(provider), next); err != nil {
		return integration.Credential{}, fmt.Errorf("oauth: persist refreshed %s token: %w", provider, err)
	}
	m.log.DebugContext(ctx, "oauth token refreshed", "user_id", userID, "provider", provider)
	return next, nil
}

// --- listing and unlinking ---

// Accounts lists userID's linked accounts.
func (m *Manager) Accounts(ctx context.Context, userID int64) ([]Account, error) {
	infos, err := m.v.List(ctx, &userID, IntegrationName)
	if err != nil {
		return nil, err
	}
	out := []Account{}
	for _, in := range infos {
		if !strings.HasPrefix(in.Kind, KindPrefix) {
			continue
		}
		provider := strings.TrimPrefix(in.Kind, KindPrefix)
		c, err := m.v.Get(ctx, &userID, IntegrationName, in.Kind)
		if err != nil {
			return nil, err
		}
		out = append(out, Account{Provider: provider, Email: c.Fields[FieldEmail], Scopes: strings.Fields(c.Fields[FieldScopes]),
			Expiry: c.Expiry, LinkedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt})
	}
	return out, nil
}

// Unlink revokes userID's grant at provider (best effort, if the provider
// has a revocation endpoint) and deletes the stored tokens. Returns
// ErrNotLinked if there is nothing to unlink.
func (m *Manager) Unlink(ctx context.Context, userID int64, provider string) (*Account, error) {
	kind := Kind(provider)
	c, err := m.v.Get(ctx, &userID, IntegrationName, kind)
	if errors.Is(err, vault.ErrNotFound) {
		return nil, ErrNotLinked
	}
	if err != nil {
		return nil, err
	}
	if cl, ok := m.clients[provider]; ok && cl.Provider.RevokeURL != "" {
		tok := c.Fields[FieldRefreshToken]
		if tok == "" {
			tok = c.Secret
		}
		if err := m.revoke(ctx, cl, tok); err != nil {
			m.log.WarnContext(ctx, "oauth revoke failed; deleting stored tokens anyway", "provider", provider, "err", err)
		}
	}
	if err := m.v.Delete(ctx, &userID, IntegrationName, kind); err != nil && !errors.Is(err, vault.ErrNotFound) {
		return nil, err
	}
	m.record(ctx, userID, "oauth account unlinked", provider, c.Fields[FieldEmail], c.Fields[FieldScopes])
	return &Account{Provider: provider, Email: c.Fields[FieldEmail], Scopes: strings.Fields(c.Fields[FieldScopes]), Expiry: c.Expiry}, nil
}

func (m *Manager) revoke(ctx context.Context, c Client, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	form := url.Values{"token": {token}, "client_id": {c.ClientID}}
	if s, err := c.secret(ctx); err == nil && s != "" {
		form.Set("client_secret", s)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Provider.RevokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("revoke: HTTP %d", resp.StatusCode)
	}
	return nil
}

// record audits a link or unlink (kind config). Tokens are never recorded.
func (m *Manager) record(ctx context.Context, userID int64, note, provider, email, scopes string) {
	if m.audit == nil {
		return
	}
	detail, _ := json.Marshal(struct {
		Provider string   `json:"provider"`
		Email    string   `json:"email"`
		Scopes   []string `json:"scopes,omitempty"`
	}{provider, email, strings.Fields(scopes)})
	e := audit.Entry{UserID: userID, Kind: audit.KindConfig, Integration: IntegrationName, Notes: []string{note}, Detail: detail}
	if err := m.audit.Record(context.WithoutCancel(ctx), e); err != nil {
		m.log.ErrorContext(ctx, "audit oauth change", "err", err)
	}
}
