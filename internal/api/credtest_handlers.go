package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// TestCredentialRequest is the body of POST /v1/integrations/{name}/test:
// the credential to try, in the same shape as PUT /v1/credentials. Nothing
// is stored.
type TestCredentialRequest struct {
	Kind   string            `json:"kind"`
	Secret string            `json:"secret"`
	Fields map[string]string `json:"fields,omitempty"`
}

// TestCredentialResponse reports the outcome. A failed test is still HTTP
// 200 with ok=false and a human-readable error.
type TestCredentialResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Credential test limits.
const (
	// CredentialTestsPerMinute bounds tests per user, so the endpoint
	// cannot be used to guess passwords against a mail server.
	CredentialTestsPerMinute = 5
	credentialTestTimeout    = 60 * time.Second
)

// rateLimiter allows n events per window per key (sliding window).
type rateLimiter struct {
	n      int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	seen map[int64][]time.Time
}

func newRateLimiter(n int, window time.Duration) *rateLimiter {
	return &rateLimiter{n: n, window: window, now: time.Now, seen: map[int64][]time.Time{}}
}

// allow records an event for key and reports whether it is within the
// limit; when it is not, it returns how long until the next is allowed.
func (l *rateLimiter) allow(key int64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.n {
		l.seen[key] = keep
		return false, l.window - now.Sub(keep[0])
	}
	l.seen[key] = append(keep, now)
	for k, ts := range l.seen { // forget idle users
		if k != key && (len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= l.window) {
			delete(l.seen, k)
		}
	}
	return true, 0
}

// findInstance returns the initialised integration name.
func (s *Server) findInstance(name string) (integration.Integration, bool) {
	if s.integrations == nil {
		return nil, false
	}
	for _, in := range s.integrations() {
		if in.Manifest().Name == name {
			return in, true
		}
	}
	return nil, false
}

// handleTestCredential tries a credential against the integration's backend
// (e.g. an IMAP login) without storing it. The secret is never logged or
// audited, and is scrubbed from the error text returned.
func (s *Server) handleTestCredential(w http.ResponseWriter, r *http.Request) {
	var req TestCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.integrations == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	name := r.PathValue("name")
	in, ok := s.findInstance(name)
	if !credNameRe.MatchString(name) || !ok {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown integration %q (not enabled; see GET /v1/integrations)", name))
		return
	}
	tester, ok := in.(integration.CredentialTester)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("integration %s cannot test credentials", name))
		return
	}
	m := in.Manifest()
	switch {
	case !credKindRe.MatchString(req.Kind):
		writeError(w, http.StatusBadRequest, "bad_request", `kind is required (e.g. "password")`)
		return
	case !KindAllowed(m.CredentialKinds, req.Kind):
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("integration %s does not use credential kind %q (declared: %s)", name, req.Kind, strings.Join(m.CredentialKinds, ", ")))
		return
	case req.Secret == "":
		writeError(w, http.StatusBadRequest, "bad_request", "secret is required")
		return
	case len(req.Secret) > MaxCredentialSecret:
		writeError(w, http.StatusBadRequest, "bad_request", "secret is too long")
		return
	case len(req.Fields) > MaxCredentialFields:
		writeError(w, http.StatusBadRequest, "bad_request", "too many fields")
		return
	}
	for k, v := range req.Fields {
		if !credFieldRe.MatchString(k) || len(v) > maxFieldValue {
			writeError(w, http.StatusBadRequest, "bad_request", "field names must match ^[a-z][a-z0-9_]*$ and values be at most 8 KiB")
			return
		}
	}
	if ok, wait := s.credTests.allow(u.ID); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			fmt.Sprintf("too many credential tests; at most %d per minute, try again in %ds", CredentialTestsPerMinute, int(wait.Seconds())+1))
		return
	}
	ctx, cancel := context.WithTimeout(integration.WithUserID(r.Context(), u.ID), credentialTestTimeout)
	defer cancel()
	cred := integration.Credential{Kind: req.Kind, Secret: req.Secret, Fields: req.Fields}
	err := tester.TestCredential(ctx, cred)
	resp := TestCredentialResponse{OK: err == nil}
	if err != nil {
		msg := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			msg = fmt.Sprintf("the test timed out after %s: %s", credentialTestTimeout, msg)
		}
		resp.Error = strings.ReplaceAll(msg, req.Secret, "[redacted]")
	}
	// Only the outcome is logged: never the secret, the fields' values or
	// the server's reply.
	s.log.InfoContext(r.Context(), "credential test", "user", u.ID, "integration", name, "kind", req.Kind, "ok", resp.OK)
	writeJSON(w, http.StatusOK, resp)
}
