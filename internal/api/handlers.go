package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
)

// HealthResponse is the body of GET /v1/health.
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok", Version: s.version})
}

// LoginRequest is the body of POST /v1/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Label    string `json:"label,omitempty"`
}

// LoginResponse is the body of a successful POST /v1/login.
type LoginResponse struct {
	Token     string     `json:"token"`
	ExpiresAt time.Time  `json:"expires_at"`
	User      *auth.User `json:"user"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "username and password are required")
		return
	}
	if len(req.Label) > 100 {
		req.Label = req.Label[:100]
	}
	sess, err := s.auth.LoginWithLabel(r.Context(), req.Username, req.Password, req.Label)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "login", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, LoginResponse{Token: sess.Token, ExpiresAt: sess.ExpiresAt, User: sess.User})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), bearerToken(r)); err != nil {
		s.log.ErrorContext(r.Context(), "logout", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	writeJSON(w, http.StatusOK, u)
}

// handleEvents streams the user's events (approval_required,
// approval_resolved, ...) as SSE until the client disconnects.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	sub := s.bus.Subscribe(u.ID, 0)
	defer sub.Close()
	sse, err := events.NewSSEWriter(w)
	if err != nil {
		s.log.ErrorContext(r.Context(), "events: sse", "err", err)
		return
	}
	if err := sse.Comment("connected"); err != nil {
		return
	}
	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if err := sse.Comment("ping"); err != nil {
				return
			}
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			if err := sse.Send(ev); err != nil {
				s.log.DebugContext(r.Context(), "events: send", "err", err)
				return
			}
		}
	}
}
