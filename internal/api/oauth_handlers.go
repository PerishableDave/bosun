package api

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"regexp"

	"github.com/perishabledave/bosun/internal/oauth"
)

// OAuth is the subset of *oauth.Manager behind /v1/oauth and /oauth/callback.
type OAuth interface {
	Start(ctx context.Context, userID int64, provider string, bundles []string, redirectURL string) (*oauth.StartResult, error)
	Status(userID int64, provider, state string) (oauth.FlowStatus, error)
	Callback(ctx context.Context, q url.Values) (*oauth.CallbackResult, error)
	Accounts(ctx context.Context, userID int64) ([]oauth.Account, error)
	Unlink(ctx context.Context, userID int64, provider string) (*oauth.Account, error)
	Providers() []oauth.ProviderInfo
}

var _ OAuth = (*oauth.Manager)(nil)

// OAuthStartRequest is the body of POST /v1/oauth/{provider}/start.
type OAuthStartRequest struct {
	// Scopes are scope bundle names ("email", "calendar", ...); empty
	// selects the provider's defaults.
	Scopes []string `json:"scopes,omitempty"`
}

// OAuthListResponse is the body of GET /v1/oauth.
type OAuthListResponse struct {
	Accounts  []oauth.Account      `json:"accounts"`
	Providers []oauth.ProviderInfo `json:"providers"`
}

var oauthProviderRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func (s *Server) oauthProvider(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.oauth == nil {
		notImplemented(w)
		return "", false
	}
	p := r.PathValue("provider")
	if !oauthProviderRe.MatchString(p) {
		writeError(w, http.StatusNotFound, "not_found", "unknown provider")
		return "", false
	}
	return p, true
}

func (s *Server) oauthError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, oauth.ErrUnknownProvider), errors.Is(err, oauth.ErrNotLinked):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, oauth.ErrNotConfigured), errors.Is(err, oauth.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, oauth.ErrBadState):
		writeError(w, http.StatusNotFound, "not_found", "unknown or expired authorization state")
	case errors.Is(err, oauth.ErrTooMany):
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	default:
		s.internalError(w, r, op, err)
	}
}

// callbackURL is the daemon's /oauth/callback as the client reached it. The
// browser runs where the CLI runs, so a loopback daemon yields
// http://127.0.0.1:<port>/oauth/callback, a LAN daemon
// https://<host>:<port>/oauth/callback, and a daemon behind a trusted proxy
// (daemon.behind_proxy) the proxy's public scheme and host.
func callbackURL(r *http.Request) string {
	c, ok := ClientFromContext(r.Context())
	if !ok {
		c, _ = resolveClient(r, nil)
	}
	return (&url.URL{Scheme: c.Scheme, Host: c.Host, Path: oauth.CallbackPath}).String()
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var req OAuthStartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, ok := s.oauthProvider(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	res, err := s.oauth.Start(r.Context(), u.ID, p, req.Scopes, callbackURL(r))
	if err != nil {
		s.oauthError(w, r, "oauth start", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauthProvider(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	st, err := s.oauth.Status(u.ID, p, r.URL.Query().Get("state"))
	if err != nil {
		s.oauthError(w, r, "oauth status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleListOAuth(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	accts, err := s.oauth.Accounts(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, "list oauth accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, OAuthListResponse{Accounts: accts, Providers: s.oauth.Providers()})
}

func (s *Server) handleDeleteOAuth(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauthProvider(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	if _, err := s.oauth.Unlink(r.Context(), u.ID, p); err != nil {
		s.oauthError(w, r, "oauth unlink", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var callbackPage = template.Must(template.New("cb").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bosun</title>
<style>body{font:16px system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#222;background:#fff}
@media (prefers-color-scheme:dark){body{color:#ddd;background:#111}}</style></head>
<body>{{if .OK}}<h1>Account connected</h1><p>Bosun can now use your {{.Provider}} account{{if .Email}} <b>{{.Email}}</b>{{end}}.</p>
{{else}}<h1>Connection failed</h1><p>{{.Message}}</p><p>Run <code>bosun connect</code> again to retry.</p>{{end}}
<p>You can close this tab.</p></body></html>
`))

// handleOAuthCallback is the provider's redirect target. It takes no bearer
// token: the single-use state identifies the user who started the flow.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		notImplemented(w)
		return
	}
	data := struct {
		OK                       bool
		Provider, Email, Message string
	}{}
	status := http.StatusOK
	res, err := s.oauth.Callback(r.Context(), r.URL.Query())
	switch {
	case err == nil:
		data.OK, data.Provider, data.Email = true, res.Provider, res.Account.Email
	case errors.Is(err, oauth.ErrBadState):
		status, data.Message = http.StatusBadRequest, "This sign-in link is invalid, expired or was already used."
	default:
		s.log.WarnContext(r.Context(), "oauth callback failed", "err", err)
		status, data.Message = http.StatusBadGateway, err.Error()
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_ = callbackPage.Execute(w, data)
}
