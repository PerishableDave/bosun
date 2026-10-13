// Package api serves Bosun's HTTP API (JSON + Server-Sent Events) and
// authenticates requests. See docs/API.md for the contract.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/policy"
)

// Authenticator is the subset of *auth.Service the API needs.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*auth.User, error)
	LoginWithLabel(ctx context.Context, username, password, label string) (*auth.Session, error)
	Logout(ctx context.Context, token string) error
}

// Options configure a Server. Auth and Bus are required; the remaining
// dependencies are optional, and the endpoints that need a missing one answer
// 501 not_implemented.
type Options struct {
	Auth    Authenticator
	Bus     *events.Bus
	Logger  *slog.Logger
	Version string
	// Heartbeat is the SSE keep-alive interval (default 15s).
	Heartbeat time.Duration

	// Conversations persists conversations (usually *store.Store).
	Conversations ConversationStore
	// Agent runs a turn (usually *agent.Agent).
	Agent Runner
	// Approvals answers and lists approvals (usually *approval.StoreManager).
	Approvals Approvals
	// Policy manages per-user rules (usually *policy.StoreEngine).
	Policy PolicyRules
	// PolicyDefaults are the per-risk defaults shown by GET /v1/policy.
	PolicyDefaults policy.Defaults
	// Tools is the registry of tools the agent can call.
	Tools *integration.Registry
	// Audit receives config entries for policy changes. Optional.
	Audit audit.Logger
	// Memories stores per-user memory (usually *memory.Store).
	Memories Memories
	// WebUI, if set, serves every path outside /v1/ (the browser UI).
	WebUI http.Handler

	// Integrations lists every initialised integration, including ones that
	// registered no tools, for GET /v1/integrations.
	Integrations func() []integration.Integration

	// Routines manages per-user routines (usually routines.Service).
	Routines Routines

	// Hooks is the hook registry listed by GET /v1/hooks (admin only).
	Hooks *hooks.Registry

	// Credentials is the credential vault (usually *vault.Vault) behind
	// /v1/credentials.
	Credentials Credentials

	// Channels manages chat-app links (usually *channels.Manager) behind
	// /v1/channels. Nil means no channel is enabled.
	Channels Channels
}

// Server holds API dependencies.
type Server struct {
	auth      Authenticator
	bus       *events.Bus
	log       *slog.Logger
	version   string
	heartbeat time.Duration

	convs     ConversationStore
	agent     Runner
	approvals Approvals
	policy    PolicyRules
	defaults  policy.Defaults
	tools     *integration.Registry
	audit     audit.Logger
	memories  Memories
	webUI     http.Handler

	integrations func() []integration.Integration
	hooks        *hooks.Registry

	routines Routines

	creds Credentials

	channels Channels

	turnsMu sync.Mutex
	turns   map[string]bool // conversations with a turn in progress
}

// New returns a Server. Auth and Bus are required.
func New(o Options) (*Server, error) {
	if o.Auth == nil || o.Bus == nil {
		return nil, errors.New("api: Auth and Bus are required")
	}
	s := &Server{auth: o.Auth, bus: o.Bus, log: o.Logger, version: o.Version, heartbeat: o.Heartbeat,
		convs: o.Conversations, agent: o.Agent, approvals: o.Approvals, policy: o.Policy, defaults: o.PolicyDefaults,
		tools: o.Tools, audit: o.Audit, memories: o.Memories, webUI: o.WebUI, integrations: o.Integrations, hooks: o.Hooks, turns: map[string]bool{}}
	s.routines = o.Routines
	s.creds = o.Credentials
	s.channels = o.Channels
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.heartbeat <= 0 {
		s.heartbeat = 15 * time.Second
	}
	if s.version == "" {
		s.version = "dev"
	}
	return s, nil
}

// MaxBodyBytes bounds JSON request bodies.
const MaxBodyBytes = 1 << 20

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.Handler { return s.RequireAuth(h) }

	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/login", s.handleLogin)
	mux.Handle("POST /v1/logout", authed(s.handleLogout))
	mux.Handle("GET /v1/me", authed(s.handleMe))
	mux.Handle("GET /v1/events", authed(s.handleEvents))

	mux.Handle("POST /v1/conversations", authed(s.handleCreateConversation))
	mux.Handle("GET /v1/conversations", authed(s.handleListConversations))
	mux.Handle("GET /v1/conversations/{id}", authed(s.handleGetConversation))
	mux.Handle("POST /v1/conversations/{id}/messages", authed(s.handlePostMessage))
	mux.Handle("GET /v1/approvals", authed(s.handleListApprovals))
	mux.Handle("POST /v1/approvals/{id}", authed(s.handleResolveApproval))
	mux.Handle("GET /v1/policy", authed(s.handleGetPolicy))
	mux.Handle("PUT /v1/policy/{capability}", authed(s.handleSetPolicyRule))
	mux.Handle("DELETE /v1/policy/{capability}", authed(s.handleDeletePolicyRule))
	mux.Handle("GET /v1/tools", authed(s.handleListTools))
	mux.Handle("GET /v1/integrations", authed(s.handleListIntegrations))
	mux.Handle("GET /v1/hooks", authed(s.handleListHooks))
	mux.Handle("GET /v1/memory", authed(s.handleListMemories))
	mux.Handle("POST /v1/memory", authed(s.handleCreateMemory))
	mux.Handle("PUT /v1/memory/{id}", authed(s.handleUpdateMemory))
	mux.Handle("DELETE /v1/memory/{id}", authed(s.handleDeleteMemory))
	mux.Handle("GET /v1/routines", authed(s.handleListRoutines))
	mux.Handle("POST /v1/routines", authed(s.handleCreateRoutine))
	mux.Handle("GET /v1/routines/{id}", authed(s.handleGetRoutine))
	mux.Handle("PUT /v1/routines/{id}", authed(s.handleUpdateRoutine))
	mux.Handle("DELETE /v1/routines/{id}", authed(s.handleDeleteRoutine))
	mux.Handle("POST /v1/routines/{id}/run", authed(s.handleRunRoutine))
	mux.Handle("GET /v1/routines/{id}/runs", authed(s.handleListRoutineRuns))
	mux.Handle("GET /v1/credentials", authed(s.handleListCredentials))
	mux.Handle("PUT /v1/credentials/{integration}/{kind}", authed(s.handlePutCredential))
	mux.Handle("DELETE /v1/credentials/{integration}/{kind}", authed(s.handleDeleteCredential))
	mux.Handle("GET /v1/channels", authed(s.handleListChannels))
	mux.Handle("POST /v1/channels/{channel}/link", authed(s.handleChannelLinkCode))
	mux.Handle("DELETE /v1/channels/{channel}/{external_id}", authed(s.handleDeleteChannelLink))

	if s.webUI != nil {
		mux.Handle("/", s.webUI)
		mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		})
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		})
	}
	return s.recoverer(s.securityHeaders(mux))
}

// --- context ---

type userKey struct{}

// WithUser returns ctx carrying the authenticated user.
func WithUser(ctx context.Context, u *auth.User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFromContext returns the authenticated user set by RequireAuth.
func UserFromContext(ctx context.Context) (*auth.User, bool) {
	u, ok := ctx.Value(userKey{}).(*auth.User)
	return u, ok && u != nil
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// RequireAuth authenticates the bearer token and puts the *auth.User in the
// request context, or responds 401.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bosun"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		u, err := s.auth.Authenticate(r.Context(), tok)
		if errors.Is(err, auth.ErrUnauthenticated) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bosun", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		}
		if err != nil {
			s.log.ErrorContext(r.Context(), "authenticate", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.ErrorContext(r.Context(), "panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(v))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// --- JSON helpers ---

// ErrorBody is the JSON error envelope: {"error":{"code":..,"message":..}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes an error.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: msg}})
}

// decodeJSON decodes a single JSON object from the body, rejecting unknown
// fields and oversized bodies. On failure it writes a 400 and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "body must contain a single JSON object")
		return false
	}
	return true
}
