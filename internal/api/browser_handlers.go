package api

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// BrowserStatus describes a user's browser session (GET /v1/browser).
type BrowserStatus struct {
	// Enabled is always true when the endpoint answers (the integration is
	// configured).
	Enabled bool `json:"enabled"`
	// Running: the user's Chromium is up.
	Running bool `json:"running"`
	// Active: a browser tool ran recently (the web UI polls the screenshot
	// while this is true).
	Active bool      `json:"active"`
	URL    string    `json:"url,omitempty"`
	Title  string    `json:"title,omitempty"`
	Steps  int       `json:"steps"`
	Budget int       `json:"step_budget"`
	LastAt time.Time `json:"last_step_at,omitzero"`
	// HasScreenshot: GET /v1/browser/screenshot has an image.
	HasScreenshot bool `json:"has_screenshot"`
}

// ErrNoScreenshot is returned by Browser.Screenshot when there is nothing
// to show.
var ErrNoScreenshot = errors.New("no browser screenshot")

// Browser is the browser integration's per-user control surface (the
// daemon adapts *browser.Integration to it).
type Browser interface {
	Status(ctx context.Context, userID int64) BrowserStatus
	// Screenshot returns the latest view of the user's page as PNG, or
	// ErrNoScreenshot.
	Screenshot(ctx context.Context, userID int64) ([]byte, error)
	// Stop kills the user's Chromium (ending any task).
	Stop(ctx context.Context, userID int64) error
}

func (s *Server) handleBrowserStatus(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	st := s.browser.Status(r.Context(), u.ID)
	st.Enabled = true
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleBrowserScreenshot(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	b, err := s.browser.Screenshot(r.Context(), u.ID)
	if errors.Is(err, ErrNoScreenshot) {
		writeError(w, http.StatusNotFound, "not_found", "no browser screenshot yet")
		return
	}
	if err != nil {
		s.internalError(w, r, "browser screenshot", err)
		return
	}
	writeImage(w, "image/png", b)
}

func (s *Server) handleBrowserStop(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	if err := s.browser.Stop(r.Context(), u.ID); err != nil {
		s.internalError(w, r, "browser stop", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
