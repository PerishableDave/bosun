package api

import (
	"net/http"

	"github.com/perishabledave/bosun/internal/doctor"
)

// DoctorResponse is the body of GET /v1/doctor.
type DoctorResponse struct {
	// Version is the daemon's version.
	Version string `json:"version"`
	// Status is the worst check status: "ok", "warn", or "fail".
	Status doctor.Status `json:"status"`
	// Checks are the daemon-side checks, in a stable order.
	Checks []doctor.Check `json:"checks"`
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if u, _ := UserFromContext(r.Context()); !u.IsAdmin() {
		writeError(w, http.StatusForbidden, "forbidden", "admin only")
		return
	}
	if s.doctor == nil {
		notImplemented(w)
		return
	}
	checks := s.doctor(r.Context())
	if checks == nil {
		checks = []doctor.Check{}
	}
	writeJSON(w, http.StatusOK, DoctorResponse{Version: s.version, Status: doctor.Worst(checks), Checks: checks})
}

// ProxyIgnored reports how many requests carried X-Forwarded-* headers that
// were not believed, and the last such peer (for the doctor's proxy check).
func (s *Server) ProxyIgnored() (int64, string) {
	if s.proxyStats == nil {
		return 0, ""
	}
	return s.proxyStats.Ignored()
}
