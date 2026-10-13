package api

import (
	"net/http"
	"sort"

	"github.com/dperedo/bosun/internal/integration"
)

// IntegrationResponse describes one initialised integration.
type IntegrationResponse struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Scope   string `json:"scope"`
	// Status is "ok" | "degraded" | "down" (integration.Status*);
	// integrations that do not report health are "ok".
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	// ToolCount is the number of tools the integration currently offers.
	ToolCount int `json:"tool_count"`
	// Details is integration-specific (for "mcp": one entry per configured
	// server). Only admins receive it, since it can include commands, URLs
	// and error text.
	Details any `json:"details,omitempty"`
}

// IntegrationListResponse is the body of GET /v1/integrations.
type IntegrationListResponse struct {
	Integrations []IntegrationResponse `json:"integrations"`
}

func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	if s.integrations == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	list := s.integrations()
	out := IntegrationListResponse{Integrations: make([]IntegrationResponse, 0, len(list))}
	for _, in := range list {
		m := in.Manifest()
		ir := IntegrationResponse{Name: m.Name, Version: m.Version, Scope: string(m.Scope),
			Status: integration.StatusOK, ToolCount: len(in.Tools())}
		if sr, ok := in.(integration.StatusReporter); ok {
			st := sr.Status()
			ir.Status, ir.Message = st.State, st.Message
			if u.IsAdmin() {
				ir.Details = st.Details
			}
		}
		out.Integrations = append(out.Integrations, ir)
	}
	sort.Slice(out.Integrations, func(i, j int) bool { return out.Integrations[i].Name < out.Integrations[j].Name })
	writeJSON(w, http.StatusOK, out)
}
