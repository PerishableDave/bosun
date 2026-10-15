package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/policy"
	"github.com/perishabledave/bosun/internal/store"
)

// ConversationStore is the subset of *store.Store the conversation endpoints
// need.
type ConversationStore interface {
	CreateConversation(ctx context.Context, userID int64, title string, routineID int64) (*store.Conversation, error)
	GetConversation(ctx context.Context, userID int64, id string) (*store.Conversation, error)
	ListConversations(ctx context.Context, userID int64, limit int) ([]*store.Conversation, error)
	ListMessages(ctx context.Context, conversationID string) ([]*store.Message, error)
}

// Runner runs one agent turn and publishes its events (ending with exactly
// one done or error event for the conversation) on the bus.
type Runner interface {
	Run(ctx context.Context, userID int64, conversationID, userText string) error
}

// Approvals is the subset of *approval.StoreManager the API needs.
type Approvals interface {
	ListPending(ctx context.Context, userID int64) ([]*approval.Approval, error)
	Resolve(ctx context.Context, id string, userID int64, d approval.Decision) error
	Get(ctx context.Context, id string, userID int64) (*approval.Approval, error)
}

// ApprovalsAlways is optionally implemented by Approvals (as
// *approval.StoreManager does) to support "approve and always allow on
// <scope>".
type ApprovalsAlways interface {
	ResolveAlways(ctx context.Context, id string, userID int64) error
}

// ApprovalPreviews is optionally implemented by Approvals (as
// *approval.StoreManager does) to serve preview images.
type ApprovalPreviews interface {
	PreviewFile(ctx context.Context, id string, userID int64) (path, mime string, err error)
}

// ScopedPolicyRules is optionally implemented by PolicyRules (as
// *policy.StoreEngine does) to manage site-scoped rules (?scope=).
type ScopedPolicyRules interface {
	SetScopedRule(ctx context.Context, userID int64, c integration.Capability, scope string, d policy.Decision) (policy.Rule, error)
	DeleteScopedRule(ctx context.Context, userID int64, c integration.Capability, scope string) error
}

// PolicyRules is the subset of *policy.StoreEngine the API needs.
type PolicyRules interface {
	policy.Engine
	ListRules(ctx context.Context, userID int64) ([]policy.Rule, error)
	SetRule(ctx context.Context, userID int64, c integration.Capability, d policy.Decision) (policy.Rule, error)
	DeleteRule(ctx context.Context, userID int64, c integration.Capability) error
}

func notImplemented(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "not available on this server")
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// --- conversations ---

// MaxTitleLen bounds conversation titles (in bytes; longer titles are cut).
const MaxTitleLen = 200

// CreateConversationRequest is the body of POST /v1/conversations.
type CreateConversationRequest struct {
	Title string `json:"title,omitempty"`
}

// ConversationResponse describes a conversation.
type ConversationResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConversationListResponse is the body of GET /v1/conversations.
type ConversationListResponse struct {
	Conversations []ConversationResponse `json:"conversations"`
}

// MessageResponse is one stored message. Content is the array of neutral
// content blocks (text, tool_use, tool_result) as stored.
type MessageResponse struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CreatedAt time.Time       `json:"created_at"`
}

// ConversationDetailResponse is the body of GET /v1/conversations/{id}.
type ConversationDetailResponse struct {
	ConversationResponse
	Messages []MessageResponse `json:"messages"`
}

// PostMessageRequest is the body of POST /v1/conversations/{id}/messages.
type PostMessageRequest struct {
	Content string `json:"content"`
}

func toConversation(c *store.Conversation) ConversationResponse {
	return ConversationResponse{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var req CreateConversationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	title := strings.TrimSpace(req.Title)
	if len(title) > MaxTitleLen {
		title = strings.ToValidUTF8(title[:MaxTitleLen], "")
	}
	c, err := s.convs.CreateConversation(r.Context(), u.ID, title, 0)
	if err != nil {
		s.internalError(w, r, "create conversation", err)
		return
	}
	writeJSON(w, http.StatusCreated, toConversation(c))
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	cs, err := s.convs.ListConversations(r.Context(), u.ID, 0)
	if err != nil {
		s.internalError(w, r, "list conversations", err)
		return
	}
	out := ConversationListResponse{Conversations: make([]ConversationResponse, 0, len(cs))}
	for _, c := range cs {
		out.Conversations = append(out.Conversations, toConversation(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// ownConversation loads the caller's conversation or writes 404/500.
func (s *Server) ownConversation(w http.ResponseWriter, r *http.Request, userID int64) (*store.Conversation, bool) {
	c, err := s.convs.GetConversation(r.Context(), userID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "conversation not found")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, "get conversation", err)
		return nil, false
	}
	return c, true
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	c, ok := s.ownConversation(w, r, u.ID)
	if !ok {
		return
	}
	ms, err := s.convs.ListMessages(r.Context(), c.ID)
	if err != nil {
		s.internalError(w, r, "list messages", err)
		return
	}
	out := ConversationDetailResponse{ConversationResponse: toConversation(c), Messages: make([]MessageResponse, 0, len(ms))}
	for _, m := range ms {
		out.Messages = append(out.Messages, MessageResponse{Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) startTurn(convID string) bool {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	if s.turns[convID] {
		return false
	}
	s.turns[convID] = true
	return true
}

func (s *Server) endTurn(convID string) {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	delete(s.turns, convID)
}

// turnBuffer is the bus buffer for one message stream: large enough that a
// long streamed reply is not dropped while the client reads slowly.
const turnBuffer = 4096

// handlePostMessage runs one agent turn and streams that conversation's
// events as SSE until the turn's done or error event. The turn runs in a
// goroutine under the request context (a client disconnect cancels it); the
// handler always waits for it to finish before returning.
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var req PostMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content is required")
		return
	}
	if s.convs == nil || s.agent == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	c, ok := s.ownConversation(w, r, u.ID)
	if !ok {
		return
	}
	if !s.startTurn(c.ID) {
		writeError(w, http.StatusConflict, "conflict", "a turn is already running in this conversation")
		return
	}
	defer s.endTurn(c.ID)

	// Subscribe before starting the turn so no event can be missed.
	sub := s.bus.Subscribe(u.ID, turnBuffer)
	defer sub.Close()
	sse, err := events.NewSSEWriter(w)
	if err != nil {
		s.log.ErrorContext(r.Context(), "messages: sse", "err", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- s.agent.Run(ctx, u.ID, c.ID, req.Content) }()

	terminal, writable := false, true
	send := func(ev events.Event) {
		if terminal || !writable || events.ConversationOf(ev) != c.ID {
			return
		}
		if err := sse.Send(ev); err != nil {
			s.log.DebugContext(r.Context(), "messages: send", "err", err)
			writable = false
			cancel() // the client is gone: cancel the turn
			return
		}
		terminal = ev.Type == events.TypeDone || ev.Type == events.TypeError
	}
	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	for {
		select {
		case ev := <-sub.C:
			send(ev)
		case <-tick.C:
			if writable && !terminal {
				if err := sse.Comment("ping"); err != nil {
					writable = false
					cancel()
				}
			}
		case runErr := <-runDone:
			// Everything the turn published is already buffered; flush it.
			for drained := false; !drained; {
				select {
				case ev := <-sub.C:
					send(ev)
				default:
					drained = true
				}
			}
			if !terminal {
				// The terminal event was dropped (or never published).
				if runErr != nil {
					send(events.Event{Type: events.TypeError, Data: events.ErrorData{Message: "turn failed", ConversationID: c.ID}})
				} else {
					send(events.Event{Type: events.TypeDone, Data: events.DoneData{ConversationID: c.ID}})
				}
			}
			if d := sub.Dropped(); d > 0 {
				s.log.WarnContext(r.Context(), "messages: events dropped for slow client", "conversation_id", c.ID, "dropped", d)
			}
			return
		}
	}
}

// --- approvals ---

// ResolveApprovalRequest is the body of POST /v1/approvals/{id}.
type ResolveApprovalRequest struct {
	Decision string `json:"decision"` // "approve" | "deny"
	// Always, with decision "approve" on a scoped approval, also creates
	// the caller's "allow" rule for the approval's capability on its scope
	// ("approve and always allow on <scope>").
	Always bool `json:"always,omitempty"`
}

// ApprovalListResponse is the body of GET /v1/approvals.
type ApprovalListResponse struct {
	Approvals []*approval.Approval `json:"approvals"`
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	as, err := s.approvals.ListPending(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, "list approvals", err)
		return
	}
	if as == nil {
		as = []*approval.Approval{}
	}
	writeJSON(w, http.StatusOK, ApprovalListResponse{Approvals: as})
}

func (s *Server) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	var req ResolveApprovalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	d := approval.Decision(req.Decision)
	if !d.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", `decision must be "approve" or "deny"`)
		return
	}
	if s.approvals == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	id := r.PathValue("id")
	var err error
	if req.Always {
		al, ok := s.approvals.(ApprovalsAlways)
		if d != approval.Approve || !ok {
			writeError(w, http.StatusBadRequest, "bad_request", `"always" requires decision "approve" on a server that supports scoped rules`)
			return
		}
		err = al.ResolveAlways(r.Context(), id, u.ID)
	} else {
		err = s.approvals.Resolve(r.Context(), id, u.ID, d)
	}
	switch {
	case err == nil:
	case errors.Is(err, approval.ErrNoScope):
		writeError(w, http.StatusBadRequest, "bad_request", "this approval has no scope; approve it without \"always\"")
		return
	case errors.Is(err, approval.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "approval belongs to another user")
		return
	case errors.Is(err, approval.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "approval not found")
		return
	case errors.Is(err, approval.ErrAlreadyResolved):
		writeError(w, http.StatusConflict, "conflict", "approval already resolved")
		return
	case errors.Is(err, approval.ErrExpired):
		writeError(w, http.StatusConflict, "conflict", "approval expired")
		return
	default:
		s.internalError(w, r, "resolve approval", err)
		return
	}
	a, err := s.approvals.Get(r.Context(), id, u.ID)
	if err != nil {
		s.internalError(w, r, "get approval", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleApprovalPreview serves GET /v1/approvals/{id}/preview: the preview
// image of one of the caller's pending approvals.
func (s *Server) handleApprovalPreview(w http.ResponseWriter, r *http.Request) {
	pv, ok := s.approvals.(ApprovalPreviews)
	if s.approvals == nil || !ok {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	path, mime, err := pv.PreviewFile(r.Context(), r.PathValue("id"), u.ID)
	switch {
	case err == nil:
	case errors.Is(err, approval.ErrNotFound), errors.Is(err, approval.ErrForbidden), errors.Is(err, approval.ErrNoPreview):
		writeError(w, http.StatusNotFound, "not_found", "no preview for that approval")
		return
	default:
		s.internalError(w, r, "approval preview", err)
		return
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) { // resolved in the meantime
		writeError(w, http.StatusNotFound, "not_found", "no preview for that approval")
		return
	}
	if err != nil {
		s.internalError(w, r, "read approval preview", err)
		return
	}
	writeImage(w, mime, b)
}

// writeImage sends an image the client must not cache or sniff.
func writeImage(w http.ResponseWriter, mime string, b []byte) {
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// --- policy ---

// PolicyDefaultsResponse is the per-risk default decisions.
type PolicyDefaultsResponse struct {
	Read        string `json:"read"`
	Write       string `json:"write"`
	Destructive string `json:"destructive"`
}

// PolicyRuleResponse is one explicit rule.
type PolicyRuleResponse struct {
	Capability string `json:"capability"`
	// Scope is the site the rule is limited to; omitted for the
	// capability-wide rule.
	Scope     string    `json:"scope,omitempty"`
	Decision  string    `json:"decision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PolicyResponse is the body of GET /v1/policy.
type PolicyResponse struct {
	Defaults PolicyDefaultsResponse `json:"defaults"`
	Rules    []PolicyRuleResponse   `json:"rules"`
	// IntegrationDefaults maps capabilities whose integration declares
	// its own default decision (used instead of the risk default when
	// there is no rule) to that decision, e.g. {"browser.read": "ask"}.
	IntegrationDefaults map[string]string `json:"integration_defaults,omitempty"`
}

// SetPolicyRuleRequest is the body of PUT /v1/policy/{capability}.
type SetPolicyRuleRequest struct {
	Decision string `json:"decision"` // "allow" | "ask" | "deny"
}

func toRule(r policy.Rule) PolicyRuleResponse {
	return PolicyRuleResponse{Capability: string(r.Capability), Scope: r.Scope, Decision: string(r.Decision), UpdatedAt: r.UpdatedAt}
}

func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	if s.policy == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	rules, err := s.policy.ListRules(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, "list policy rules", err)
		return
	}
	d := s.defaults
	out := PolicyResponse{
		Defaults: PolicyDefaultsResponse{
			Read:        string(d.ForRisk(integration.RiskRead)),
			Write:       string(d.ForRisk(integration.RiskWrite)),
			Destructive: string(d.ForRisk(integration.RiskDestructive)),
		},
		Rules: make([]PolicyRuleResponse, 0, len(rules)),
	}
	for _, rule := range rules {
		out.Rules = append(out.Rules, toRule(rule))
	}
	if s.tools != nil {
		for _, name := range s.tools.Names() {
			if i, ok := s.tools.Integration(name); ok {
				for c, d := range i.Manifest().DefaultDecisions {
					if out.IntegrationDefaults == nil {
						out.IntegrationDefaults = map[string]string{}
					}
					out.IntegrationDefaults[string(c)] = d
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// knownCapability reports whether some registered tool uses c. With no tool
// registry configured every syntactically valid capability is accepted.
func (s *Server) knownCapability(c integration.Capability) bool {
	if s.tools == nil {
		return true
	}
	for _, name := range s.tools.Names() {
		if i, ok := s.tools.Integration(name); ok && i.Manifest().HasCapability(c) {
			return true
		}
	}
	return false
}

func (s *Server) handleSetPolicyRule(w http.ResponseWriter, r *http.Request) {
	var req SetPolicyRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.policy == nil {
		notImplemented(w)
		return
	}
	c := integration.Capability(r.PathValue("capability"))
	d := policy.Decision(req.Decision)
	if !d.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", `decision must be "allow", "ask", or "deny"`)
		return
	}
	if !policy.ValidCapability(c) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid capability name")
		return
	}
	if !s.knownCapability(c) {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown capability (see GET /v1/tools)")
		return
	}
	scope, ok := s.scopeParam(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	var rule policy.Rule
	var err error
	if scope != "" {
		rule, err = s.policy.(ScopedPolicyRules).SetScopedRule(r.Context(), u.ID, c, scope, d)
	} else {
		rule, err = s.policy.SetRule(r.Context(), u.ID, c, d)
	}
	if err != nil {
		s.internalError(w, r, "set policy rule", err)
		return
	}
	s.recordConfigScoped(r.Context(), u.ID, c, scope, string(d), "policy rule set")
	writeJSON(w, http.StatusOK, toRule(rule))
}

// scopeParam returns the normalized ?scope= of a policy request ("" when
// absent). It answers 400 for a value that is not a site and 501 when the
// engine has no scoped rules.
func (s *Server) scopeParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.URL.Query().Get("scope")
	if raw == "" {
		return "", true
	}
	scope := integration.NormalizeScope(raw)
	if scope == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "scope must be a site, e.g. opentable.com")
		return "", false
	}
	if _, ok := s.policy.(ScopedPolicyRules); !ok {
		notImplemented(w)
		return "", false
	}
	return scope, true
}

func (s *Server) handleDeletePolicyRule(w http.ResponseWriter, r *http.Request) {
	if s.policy == nil {
		notImplemented(w)
		return
	}
	c := integration.Capability(r.PathValue("capability"))
	scope, ok := s.scopeParam(w, r)
	if !ok {
		return
	}
	u, _ := UserFromContext(r.Context())
	var err error
	if scope != "" {
		err = s.policy.(ScopedPolicyRules).DeleteScopedRule(r.Context(), u.ID, c, scope)
	} else {
		err = s.policy.DeleteRule(r.Context(), u.ID, c)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no rule for that capability")
		return
	}
	if err != nil {
		s.internalError(w, r, "delete policy rule", err)
		return
	}
	note := "policy rule deleted (risk default applies)"
	if scope != "" {
		note = "scoped policy rule deleted"
	}
	s.recordConfigScoped(r.Context(), u.ID, c, scope, "", note)
	w.WriteHeader(http.StatusNoContent)
}

// recordConfigScoped is recordConfig noting the scope, if any.
func (s *Server) recordConfigScoped(ctx context.Context, userID int64, c integration.Capability, scope, decision, note string) {
	if scope != "" {
		note += " (scope " + scope + ")"
	}
	s.recordConfig(ctx, userID, c, decision, note)
}

func (s *Server) recordConfig(ctx context.Context, userID int64, c integration.Capability, decision, note string) {
	if s.audit == nil {
		return
	}
	e := audit.Entry{UserID: userID, Kind: audit.KindConfig, Capability: c, Decision: decision, Notes: []string{note}}
	if err := s.audit.Record(context.WithoutCancel(ctx), e); err != nil {
		s.log.ErrorContext(ctx, "audit policy change", "err", err)
	}
}

// --- tools ---

// ToolResponse describes one registered tool.
type ToolResponse struct {
	Name        string `json:"name"`
	Integration string `json:"integration"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
	Risk        string `json:"risk"`
	// Decision is what policy would decide for the caller right now
	// ("allow" | "ask" | "deny").
	Decision string `json:"decision,omitempty"`
}

// ToolListResponse is the body of GET /v1/tools.
type ToolListResponse struct {
	Tools []ToolResponse `json:"tools"`
}

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	if s.tools == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	ts := s.tools.Tools()
	out := ToolListResponse{Tools: make([]ToolResponse, 0, len(ts))}
	for _, t := range ts {
		_, integ, _ := s.tools.Tool(t.Name)
		tr := ToolResponse{Name: t.Name, Integration: integ, Description: t.Description,
			Capability: string(t.Capability), Risk: string(t.Risk)}
		if s.policy != nil {
			tr.Decision = string(s.policy.Decide(r.Context(), u.ID, t.Capability, t.Risk, nil))
		}
		out.Tools = append(out.Tools, tr)
	}
	writeJSON(w, http.StatusOK, out)
}
