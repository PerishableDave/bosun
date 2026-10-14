// Package oauthtest is a fake OAuth2/OIDC identity provider for tests: an
// authorization endpoint that immediately redirects with a code, a token
// endpoint that enforces PKCE (S256) and supports refresh (with optional
// rotation and revocation), a userinfo endpoint and an RFC 7009 revocation
// endpoint.
package oauthtest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/oauth"
)

// IdP is a fake identity provider. Exported fields may be changed between
// requests; use the methods for anything shared with handlers.
type IdP struct {
	Server       *httptest.Server
	ClientID     string
	ClientSecret string // "" accepts requests without a secret (public client)

	mu sync.Mutex
	// Email is the account the next authorization signs in as.
	email string
	// CodeExpiresIn / RefreshExpiresIn are the expires_in of tokens from
	// the code exchange and from refreshes.
	codeExpiresIn, refreshExpiresIn int
	rotate                          bool
	refreshDelay                    time.Duration

	codes    map[string]codeRec
	access   map[string]string // access token -> email
	refresh  map[string]string // refresh token -> email
	revoked  map[string]bool   // refresh tokens rejected with invalid_grant
	n        int
	verifier []string
	revokes  []string
	refreshN int
}

type codeRec struct {
	challenge, redirect, email, scope string
}

// New starts a fake IdP; Close it when done.
func New() *IdP {
	i := &IdP{ClientID: "client-123", ClientSecret: "secret-456", email: "alice@example.com",
		codeExpiresIn: 3600, refreshExpiresIn: 3600,
		codes: map[string]codeRec{}, access: map[string]string{}, refresh: map[string]string{}, revoked: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", i.authorize)
	mux.HandleFunc("POST /token", i.token)
	mux.HandleFunc("GET /userinfo", i.userinfo)
	mux.HandleFunc("POST /revoke", i.revoke)
	i.Server = httptest.NewServer(mux)
	return i
}

// Close stops the server.
func (i *IdP) Close() { i.Server.Close() }

// Provider returns the built-in provider name with its endpoints pointed at
// the fake.
func (i *IdP) Provider(name string) oauth.Provider {
	p, ok := oauth.Builtin(name)
	if !ok {
		p = oauth.Provider{Name: name, BaseScopes: []string{"openid", "email"}}
	}
	p.AuthURL = i.Server.URL + "/authorize"
	p.TokenURL = i.Server.URL + "/token"
	p.UserinfoURL = i.Server.URL + "/userinfo"
	p.RevokeURL = i.Server.URL + "/revoke"
	return p
}

// Client returns an oauth.Client for the fake (confidential).
func (i *IdP) Client(name string) oauth.Client {
	secret := i.ClientSecret
	return oauth.Client{Provider: i.Provider(name), ClientID: i.ClientID,
		Secret: func(context.Context) (string, error) { return secret, nil }, SecretRequired: secret != ""}
}

// SetEmail sets the account the next authorization signs in as.
func (i *IdP) SetEmail(e string) { i.mu.Lock(); i.email = e; i.mu.Unlock() }

// SetExpiry sets expires_in (seconds) for code-exchange and refresh tokens.
func (i *IdP) SetExpiry(code, refresh int) {
	i.mu.Lock()
	i.codeExpiresIn, i.refreshExpiresIn = code, refresh
	i.mu.Unlock()
}

// SetRotate makes refreshes issue a new refresh token and revoke the old.
func (i *IdP) SetRotate(on bool) { i.mu.Lock(); i.rotate = on; i.mu.Unlock() }

// SetRefreshDelay slows refresh responses (to test single-flight).
func (i *IdP) SetRefreshDelay(d time.Duration) { i.mu.Lock(); i.refreshDelay = d; i.mu.Unlock() }

// RevokeAll rejects every refresh token issued so far (invalid_grant).
func (i *IdP) RevokeAll() {
	i.mu.Lock()
	for rt := range i.refresh {
		i.revoked[rt] = true
	}
	i.mu.Unlock()
}

// Verifiers returns the PKCE verifiers received (each one matched its
// challenge, or the exchange was refused).
func (i *IdP) Verifiers() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.verifier...)
}

// Revokes returns the tokens posted to the revocation endpoint.
func (i *IdP) Revokes() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.revokes...)
}

// RefreshCount is how many successful refresh_token grants were served.
func (i *IdP) RefreshCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.refreshN
}

func (i *IdP) next(prefix string) string {
	i.n++
	return fmt.Sprintf("%s-%d", prefix, i.n)
}

func tokenErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": code + " (fake idp)"})
}

func (i *IdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != i.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Host == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	i.mu.Lock()
	code := i.next("code")
	i.codes[code] = codeRec{challenge: q.Get("code_challenge"), redirect: redirect.String(), email: i.email, scope: q.Get("scope")}
	i.mu.Unlock()
	rq := redirect.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (i *IdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	f := r.PostForm
	if f.Get("client_id") != i.ClientID || (i.ClientSecret != "" && f.Get("client_secret") != i.ClientSecret) {
		tokenErr(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	i.mu.Lock()
	var resp map[string]any
	switch f.Get("grant_type") {
	case "authorization_code":
		c, ok := i.codes[f.Get("code")]
		delete(i.codes, f.Get("code"))
		v := f.Get("code_verifier")
		sum := sha256.Sum256([]byte(v))
		if !ok || c.redirect != f.Get("redirect_uri") || v == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			i.mu.Unlock()
			tokenErr(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		i.verifier = append(i.verifier, v)
		at, rt := i.next("at"), i.next("rt")
		i.access[at], i.refresh[rt] = c.email, c.email
		resp = map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer",
			"expires_in": i.codeExpiresIn, "scope": c.scope}
	case "refresh_token":
		rt := f.Get("refresh_token")
		email, ok := i.refresh[rt]
		if !ok || i.revoked[rt] {
			i.mu.Unlock()
			tokenErr(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		delay := i.refreshDelay
		i.mu.Unlock()
		time.Sleep(delay)
		i.mu.Lock()
		i.refreshN++
		at := i.next("at")
		i.access[at] = email
		resp = map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": i.refreshExpiresIn}
		if i.rotate {
			nrt := i.next("rt")
			i.refresh[nrt] = email
			i.revoked[rt] = true
			resp["refresh_token"] = nrt
		}
	default:
		i.mu.Unlock()
		tokenErr(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (i *IdP) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	i.mu.Lock()
	email, ok := i.access[tok]
	i.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"sub": "sub-" + email, "email": email, "email_verified": true})
}

func (i *IdP) revoke(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	i.mu.Lock()
	tok := r.PostForm.Get("token")
	i.revokes = append(i.revokes, tok)
	i.revoked[tok] = true
	i.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}
