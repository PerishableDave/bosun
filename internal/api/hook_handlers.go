package api

import (
	"net/http"

	"github.com/perishabledave/bosun/internal/hooks"
)

// HookResponse describes one registered hook.
type HookResponse struct {
	Name string `json:"name"`
	Type string `json:"type"` // "go" | "script"
}

// HookListResponse is the body of GET /v1/hooks: registered hooks keyed by
// point, in run order. Every runnable point is present (possibly empty).
type HookListResponse struct {
	Hooks map[hooks.Point][]HookResponse `json:"hooks"`
}

// HookPoints are the points listed by GET /v1/hooks, in lifecycle order.
var HookPoints = []hooks.Point{hooks.PointPreModelCall, hooks.PointPreToolCall, hooks.PointPostToolCall, hooks.PointOnApproval}

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	if u, _ := UserFromContext(r.Context()); !u.IsAdmin() {
		writeError(w, http.StatusForbidden, "forbidden", "admin only")
		return
	}
	if s.hooks == nil {
		notImplemented(w)
		return
	}
	out := HookListResponse{Hooks: map[hooks.Point][]HookResponse{}}
	for _, p := range HookPoints {
		out.Hooks[p] = []HookResponse{}
	}
	for _, h := range s.hooks.List() {
		out.Hooks[h.Point] = append(out.Hooks[h.Point], HookResponse{Name: h.Name, Type: h.Type})
	}
	writeJSON(w, http.StatusOK, out)
}
