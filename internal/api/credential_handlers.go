package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/vault"
)

// Credentials is the subset of *vault.Vault the credential endpoints need.
// A nil userID addresses the shared (household-wide) credential.
type Credentials interface {
	Put(ctx context.Context, userID *int64, integrationName, kind string, cred integration.Credential) error
	Delete(ctx context.Context, userID *int64, integrationName, kind string) error
	List(ctx context.Context, userID *int64, integrationName string) ([]vault.Info, error)
}

var _ Credentials = (*vault.Vault)(nil)

// Credential scopes in the API.
const (
	CredentialScopeUser   = "user"
	CredentialScopeShared = "shared"
)

// Credential input limits.
const (
	MaxCredentialSecret = 64 << 10
	MaxCredentialFields = 32
	maxFieldValue       = 8 << 10
)

// CredentialResponse describes a stored credential. It never includes the
// secret or the fields' values.
type CredentialResponse struct {
	Integration string `json:"integration"`
	Kind        string `json:"kind"`
	// Scope is "user" (the caller's own) or "shared" (household-wide).
	Scope     string     `json:"scope"`
	Expiry    *time.Time `json:"expiry,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// CredentialListResponse is the body of GET /v1/credentials.
type CredentialListResponse struct {
	Credentials []CredentialResponse `json:"credentials"`
}

// PutCredentialRequest is the body of PUT /v1/credentials/{integration}/{kind}.
type PutCredentialRequest struct {
	Secret string            `json:"secret"`
	Fields map[string]string `json:"fields,omitempty"`
	Expiry *time.Time        `json:"expiry,omitempty"`
}

var (
	credKindRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}(:[a-z0-9][a-z0-9_.-]{0,63})?$`)
	credNameRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	credFieldRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// KindAllowed reports whether kind is one of declared, or "<d>:<suffix>"
// for a declared kind d that has no suffix of its own (so an integration
// declaring "bearer" accepts "bearer:github").
func KindAllowed(declared []string, kind string) bool {
	for _, d := range declared {
		if kind == d {
			return true
		}
		if !strings.Contains(d, ":") && strings.HasPrefix(kind, d+":") && len(kind) > len(d)+1 {
			return true
		}
	}
	return false
}

func toCredential(in vault.Info) CredentialResponse {
	c := CredentialResponse{Integration: in.Integration, Kind: in.Kind, Scope: CredentialScopeUser,
		CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt}
	if in.UserID == nil {
		c.Scope = CredentialScopeShared
	}
	if !in.Expiry.IsZero() {
		e := in.Expiry
		c.Expiry = &e
	}
	return c
}

// findIntegration returns the manifest of the initialised integration name.
func (s *Server) findIntegration(name string) (integration.Manifest, bool) {
	if s.integrations == nil {
		return integration.Manifest{}, false
	}
	for _, in := range s.integrations() {
		if m := in.Manifest(); m.Name == name {
			return m, true
		}
	}
	return integration.Manifest{}, false
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	filter := r.URL.Query().Get("integration")
	mine, err := s.creds.List(r.Context(), &u.ID, filter)
	if err != nil {
		s.internalError(w, r, "list credentials", err)
		return
	}
	// Shared metadata (not secrets) is visible to every user, so members
	// can see whether a household integration is configured.
	shared, err := s.creds.List(r.Context(), nil, filter)
	if err != nil {
		s.internalError(w, r, "list shared credentials", err)
		return
	}
	out := CredentialListResponse{Credentials: make([]CredentialResponse, 0, len(mine)+len(shared))}
	for _, in := range append(mine, shared...) {
		out.Credentials = append(out.Credentials, toCredential(in))
	}
	sort.SliceStable(out.Credentials, func(i, j int) bool {
		a, b := out.Credentials[i], out.Credentials[j]
		if a.Integration != b.Integration {
			return a.Integration < b.Integration
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Scope > b.Scope // "user" before "shared"
	})
	writeJSON(w, http.StatusOK, out)
}

// credentialTarget resolves {integration}/{kind} and ?scope= to the vault
// owner (nil = shared), enforcing the scope rules. write is true for PUT,
// which also requires a registered integration that declares the kind. On
// failure it writes the response and returns ok=false.
func (s *Server) credentialTarget(w http.ResponseWriter, r *http.Request, u *auth.User, write bool) (name, kind, scope string, owner *int64, ok bool) {
	name, kind = r.PathValue("integration"), r.PathValue("kind")
	if !credNameRe.MatchString(name) {
		writeError(w, http.StatusNotFound, "not_found", "unknown integration")
		return
	}
	if !credKindRe.MatchString(kind) {
		writeError(w, http.StatusBadRequest, "bad_request", `invalid credential kind (want e.g. "apikey" or "bearer:github")`)
		return
	}
	q := r.URL.Query().Get("scope")
	if q != "" && q != CredentialScopeUser && q != CredentialScopeShared {
		writeError(w, http.StatusBadRequest, "bad_request", `scope must be "user" or "shared"`)
		return
	}
	m, found := s.findIntegration(name)
	if write {
		if !found {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown integration %q (see GET /v1/integrations)", name))
			return
		}
		if !KindAllowed(m.CredentialKinds, kind) {
			declared := "none"
			if len(m.CredentialKinds) > 0 {
				declared = strings.Join(m.CredentialKinds, ", ")
			}
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("integration %s does not use credential kind %q (declared: %s)", name, kind, declared))
			return
		}
	}
	scope = q
	if found {
		switch {
		case m.Scope == integration.ScopeShared && q == CredentialScopeUser:
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("integration %s is shared: it only uses the household-wide credential (scope=shared, admin only)", name))
			return
		case m.Scope == integration.ScopePerUser && q == CredentialScopeShared:
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("integration %s is per-user: it never reads shared credentials", name))
			return
		case m.Scope == integration.ScopeShared:
			scope = CredentialScopeShared
		}
	}
	if scope == "" {
		scope = CredentialScopeUser
	}
	if scope == CredentialScopeShared {
		if !u.IsAdmin() {
			msg := "only an admin can manage shared credentials"
			if found && m.Scope == integration.ScopeShared {
				msg = fmt.Sprintf("integration %s is shared; only an admin can manage its credentials", name)
			}
			writeError(w, http.StatusForbidden, "forbidden", msg)
			return
		}
		return name, kind, scope, nil, true
	}
	id := u.ID
	return name, kind, scope, &id, true
}

func (s *Server) handlePutCredential(w http.ResponseWriter, r *http.Request) {
	var req PutCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.creds == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	name, kind, scope, owner, ok := s.credentialTarget(w, r, u, true)
	if !ok {
		return
	}
	switch {
	case req.Secret == "":
		writeError(w, http.StatusBadRequest, "bad_request", "secret is required")
		return
	case len(req.Secret) > MaxCredentialSecret:
		writeError(w, http.StatusBadRequest, "bad_request", "secret is too long")
		return
	case len(req.Fields) > MaxCredentialFields:
		writeError(w, http.StatusBadRequest, "bad_request", "too many fields")
		return
	case req.Expiry != nil && !req.Expiry.After(time.Now()):
		writeError(w, http.StatusBadRequest, "bad_request", "expiry is in the past")
		return
	}
	fieldNames := make([]string, 0, len(req.Fields))
	for k, v := range req.Fields {
		if !credFieldRe.MatchString(k) || len(v) > maxFieldValue {
			writeError(w, http.StatusBadRequest, "bad_request", "field names must match ^[a-z][a-z0-9_]*$ and values be at most 8 KiB")
			return
		}
		fieldNames = append(fieldNames, k)
	}
	sort.Strings(fieldNames)
	cred := integration.Credential{Kind: kind, Secret: req.Secret, Fields: req.Fields}
	if req.Expiry != nil {
		cred.Expiry = req.Expiry.UTC()
	}
	if err := s.creds.Put(r.Context(), owner, name, kind, cred); err != nil {
		s.internalError(w, r, "put credential", err)
		return
	}
	s.recordCredential(r.Context(), u.ID, name, kind, scope, "credential set", fieldNames)
	infos, err := s.creds.List(r.Context(), owner, name)
	if err != nil {
		s.internalError(w, r, "list credentials", err)
		return
	}
	for _, in := range infos {
		if in.Kind == kind {
			writeJSON(w, http.StatusOK, toCredential(in))
			return
		}
	}
	s.internalError(w, r, "put credential", errors.New("stored credential not found"))
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	// Deleting does not require the integration to still be enabled, so
	// stale credentials can be cleaned up.
	name, kind, scope, owner, ok := s.credentialTarget(w, r, u, false)
	if !ok {
		return
	}
	err := s.creds.Delete(r.Context(), owner, name, kind)
	if errors.Is(err, vault.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such credential")
		return
	}
	if err != nil {
		s.internalError(w, r, "delete credential", err)
		return
	}
	s.recordCredential(r.Context(), u.ID, name, kind, scope, "credential deleted", nil)
	w.WriteHeader(http.StatusNoContent)
}

// recordCredential audits a credential change. The secret and field values
// are never recorded; only field names are.
func (s *Server) recordCredential(ctx context.Context, userID int64, name, kind, scope, note string, fields []string) {
	if s.audit == nil {
		return
	}
	detail, _ := json.Marshal(struct {
		Integration string   `json:"integration"`
		Kind        string   `json:"kind"`
		Scope       string   `json:"scope"`
		Fields      []string `json:"fields,omitempty"`
	}{name, kind, scope, fields})
	e := audit.Entry{UserID: userID, Kind: audit.KindConfig, Integration: name, Notes: []string{note}, Detail: detail}
	if err := s.audit.Record(context.WithoutCancel(ctx), e); err != nil {
		s.log.ErrorContext(ctx, "audit credential change", "err", err)
	}
}
