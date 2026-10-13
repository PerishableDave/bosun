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
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
)

// Authenticator is the subset of *auth.Service the API needs.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*auth.User, error)
	LoginWithLabel(ctx context.Context, username, password, label string) (*auth.Session, error)
	Logout(ctx context.Context, token string) error
}

// Options configure a Server.
type Options struct {
	Auth    Authenticator
	Bus     *events.Bus
	Logger  *slog.Logger
	Version string
	// Heartbeat is the SSE keep-alive interval (default 15s).
	Heartbeat time.Duration
}

// Server holds API dependencies.
type Server struct {
	auth      Authenticator
	bus       *events.Bus
	log       *slog.Logger
	version   string
	heartbeat time.Duration
}

// New returns a Server. Auth and Bus are required.
func New(o Options) (*Server, error) {
	if o.Auth == nil || o.Bus == nil {
		return nil, errors.New("api: Auth and Bus are required")
	}
	s := &Server{auth: o.Auth, bus: o.Bus, log: o.Logger, version: o.Version, heartbeat: o.Heartbeat}
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

	// Contract stubs (501) implemented by the agent/approval work.
	mux.Handle("POST /v1/conversations", authed(s.handleCreateConversation))
	mux.Handle("POST /v1/conversations/{id}/messages", authed(s.handlePostMessage))
	mux.Handle("GET /v1/approvals", authed(s.handleListApprovals))
	mux.Handle("POST /v1/approvals/{id}", authed(s.handleResolveApproval))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
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
