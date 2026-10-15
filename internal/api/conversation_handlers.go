package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/perishabledave/bosun/internal/agent"
	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/titles"
)

// ConversationStore is the subset of *store.Store the conversation endpoints
// need.
type ConversationStore interface {
	CreateConversationWith(ctx context.Context, userID int64, nc store.NewConversation) (*store.Conversation, error)
	GetConversation(ctx context.Context, userID int64, id string) (*store.Conversation, error)
	ListConversationPage(ctx context.Context, userID int64, f store.ConversationFilter) ([]*store.Conversation, string, error)
	UpdateConversation(ctx context.Context, userID int64, id string, p store.ConversationPatch) (*store.Conversation, error)
	DeleteConversation(ctx context.Context, userID int64, id string) error
	SetAutoTitle(ctx context.Context, userID int64, id, title string) (*store.Conversation, bool, error)
	SetConversationUnread(ctx context.Context, userID int64, id string, unread bool) (*store.Conversation, bool, error)
	ListMessages(ctx context.Context, conversationID string) ([]*store.Message, error)
}

var _ ConversationStore = (*store.Store)(nil)

// ApprovalCanceler is optionally implemented by Approvals (as
// *approval.StoreManager does): DELETE /v1/conversations/{id} cancels the
// conversation's pending approvals through it.
type ApprovalCanceler interface {
	CancelConversation(ctx context.Context, userID int64, conversationID string) (int, error)
}

// Titler names a conversation from its first exchange (usually
// *titles.Generator on memory.extract_model).
type Titler interface {
	Title(ctx context.Context, userID int64, userText, assistantText string) (string, error)
}

// MaxTitleLen bounds conversation titles (in bytes; longer titles are cut).
const MaxTitleLen = 200

// Conversation list paging.
const (
	DefaultConversationPage = 50
	MaxConversationPage     = 200
)

// CreateConversationRequest is the body of POST /v1/conversations.
type CreateConversationRequest struct {
	Title string `json:"title,omitempty"`
}

// ConversationResponse describes a conversation.
type ConversationResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// TitleSource is "auto" (Bosun may retitle) or "user".
	TitleSource string `json:"title_source"`
	// Source is "chat", "routine", or "telegram".
	Source string `json:"source"`
	// RoutineName is set on routine conversations.
	RoutineName  string `json:"routine_name,omitempty"`
	Pinned       bool   `json:"pinned"`
	Archived     bool   `json:"archived"`
	Unread       bool   `json:"unread"`
	MessageCount int    `json:"message_count"`
	// LastMessageAt is null while the conversation has no message.
	LastMessageAt      *time.Time `json:"last_message_at"`
	LastMessagePreview string     `json:"last_message_preview"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// ConversationListResponse is the body of GET /v1/conversations.
type ConversationListResponse struct {
	Conversations []ConversationResponse `json:"conversations"`
	// NextCursor is the ?before= of the next page; null on the last page.
	NextCursor *string `json:"next_cursor"`
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

// PatchConversationRequest is the body of PATCH /v1/conversations/{id}.
// Omitted fields are left alone; at least one is required.
type PatchConversationRequest struct {
	Title    *string `json:"title,omitempty"`
	Pinned   *bool   `json:"pinned,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
}

// StopResponse is the body of POST /v1/conversations/{id}/stop.
type StopResponse struct {
	// Stopped is true when a running turn was told to stop, false when no
	// turn was running.
	Stopped bool `json:"stopped"`
}

func toConversation(c *store.Conversation) ConversationResponse {
	out := ConversationResponse{ID: c.ID, Title: c.Title, TitleSource: c.TitleSource, Source: c.Source,
		RoutineName: c.RoutineName, Pinned: c.Pinned, Archived: c.Archived, Unread: c.Unread,
		MessageCount: c.MessageCount, LastMessagePreview: c.LastMessagePreview,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if out.Source == store.SourceRoutine && out.RoutineName == "" {
		out.RoutineName = strings.TrimPrefix(c.Title, "routine: ") // the routine was deleted
	}
	if !c.LastMessageAt.IsZero() {
		t := c.LastMessageAt
		out.LastMessageAt = &t
	}
	return out
}

func conversationUpdated(c *store.Conversation) events.Event {
	return events.Event{Type: events.TypeConversationUpdated, Data: events.ConversationUpdatedData{
		ID: c.ID, Title: c.Title, TitleSource: c.TitleSource, Pinned: c.Pinned, Archived: c.Archived,
		Unread: c.Unread, UpdatedAt: c.UpdatedAt, ConversationID: c.ID}}
}

// cleanTitle trims, drops control characters, and cuts to MaxTitleLen.
func cleanTitle(t string) string {
	t = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(t, ""))
	t = strings.TrimSpace(t)
	if len(t) > MaxTitleLen {
		t = strings.TrimSpace(strings.ToValidUTF8(t[:MaxTitleLen], ""))
	}
	return t
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
	nc := store.NewConversation{Title: cleanTitle(req.Title), Source: store.SourceChat}
	if titles.Placeholder(nc.Title) {
		nc.TitleSource = store.TitleAuto // "New chat" and the like get an automatic title
	}
	c, err := s.convs.CreateConversationWith(r.Context(), u.ID, nc)
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
	q := r.URL.Query()
	f := store.ConversationFilter{Limit: DefaultConversationPage, Before: q.Get("before"),
		Query: strings.TrimSpace(q.Get("q"))}
	switch q.Get("archived") {
	case "", "false":
		no := false
		f.Archived = &no
	case "true":
		yes := true
		f.Archived = &yes
	case "all":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", `archived must be "false", "true", or "all"`)
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxConversationPage {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("limit must be 1..%d", MaxConversationPage))
			return
		}
		f.Limit = n
	}
	if len(f.Query) > 200 {
		writeError(w, http.StatusBadRequest, "bad_request", "q is too long")
		return
	}
	u, _ := UserFromContext(r.Context())
	cs, next, err := s.convs.ListConversationPage(r.Context(), u.ID, f)
	if errors.Is(err, store.ErrBadCursor) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid before cursor")
		return
	}
	if err != nil {
		s.internalError(w, r, "list conversations", err)
		return
	}
	out := ConversationListResponse{Conversations: make([]ConversationResponse, 0, len(cs))}
	for _, c := range cs {
		out.Conversations = append(out.Conversations, toConversation(c))
	}
	if next != "" {
		out.NextCursor = &next
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

func (s *Server) conversationDetail(r *http.Request, c *store.Conversation) (ConversationDetailResponse, error) {
	ms, err := s.convs.ListMessages(r.Context(), c.ID)
	if err != nil {
		return ConversationDetailResponse{}, err
	}
	out := ConversationDetailResponse{ConversationResponse: toConversation(c), Messages: make([]MessageResponse, 0, len(ms))}
	for _, m := range ms {
		out.Messages = append(out.Messages, MessageResponse{Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt})
	}
	return out, nil
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
	out, err := s.conversationDetail(r, c)
	if err != nil {
		s.internalError(w, r, "list messages", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePatchConversation(w http.ResponseWriter, r *http.Request) {
	var req PatchConversationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == nil && req.Pinned == nil && req.Archived == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to change: set title, pinned, or archived")
		return
	}
	if req.Title != nil {
		t := cleanTitle(*req.Title)
		if t == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "title must not be blank")
			return
		}
		req.Title = &t
	}
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	c, err := s.convs.UpdateConversation(r.Context(), u.ID, r.PathValue("id"),
		store.ConversationPatch{Title: req.Title, Pinned: req.Pinned, Archived: req.Archived})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "update conversation", err)
		return
	}
	s.bus.Publish(u.ID, conversationUpdated(c))
	writeJSON(w, http.StatusOK, toConversation(c))
}

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	c, ok := s.ownConversation(w, r, u.ID)
	if !ok {
		return
	}
	// Hold the conversation's turn slot so no turn starts while it goes.
	if !s.startTurn(c.ID, func(error) {}) {
		writeError(w, http.StatusConflict, "conflict", "a turn is running in this conversation; stop it first")
		return
	}
	defer s.endTurn(c.ID)
	if ac, ok := s.approvals.(ApprovalCanceler); ok && s.approvals != nil {
		if _, err := ac.CancelConversation(r.Context(), u.ID, c.ID); err != nil {
			s.internalError(w, r, "cancel conversation approvals", err)
			return
		}
	}
	err := s.convs.DeleteConversation(r.Context(), u.ID, c.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "delete conversation", err)
		return
	}
	s.bus.Publish(u.ID, events.Event{Type: events.TypeConversationDeleted,
		Data: events.ConversationDeletedData{ID: c.ID, ConversationID: c.ID}})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStopConversation(w http.ResponseWriter, r *http.Request) {
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	c, ok := s.ownConversation(w, r, u.ID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, StopResponse{Stopped: s.stopTurn(c.ID)})
}

func (s *Server) handleReadConversation(w http.ResponseWriter, r *http.Request) {
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	c, changed, err := s.convs.SetConversationUnread(r.Context(), u.ID, r.PathValue("id"), false)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "mark conversation read", err)
		return
	}
	if changed {
		s.bus.Publish(u.ID, conversationUpdated(c))
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- turns ---

// startTurn claims convID's turn slot, registering cancel for stopTurn. It
// returns false if a turn (or a delete) holds it.
func (s *Server) startTurn(convID string, cancel context.CancelCauseFunc) bool {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	if _, busy := s.turns[convID]; busy {
		return false
	}
	s.turns[convID] = cancel
	return true
}

func (s *Server) endTurn(convID string) {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	delete(s.turns, convID)
}

// stopTurn cancels convID's running turn with agent.ErrStopped. It reports
// whether one was running.
func (s *Server) stopTurn(convID string) bool {
	s.turnsMu.Lock()
	cancel, ok := s.turns[convID]
	s.turnsMu.Unlock()
	if ok {
		cancel(agent.ErrStopped)
	}
	return ok
}

// turnBuffer is the bus buffer for one message stream: large enough that a
// long streamed reply is not dropped while the client reads slowly.
const turnBuffer = 4096

// autoTitled reports whether c's title is Bosun's to choose and has not been
// chosen yet: an API conversation with an automatic, placeholder title.
func autoTitled(c *store.Conversation) bool {
	return c.Source == store.SourceChat && c.TitleSource == store.TitleAuto && titles.Placeholder(c.Title)
}

// setAutoTitle stores an automatic title and announces it.
func (s *Server) setAutoTitle(ctx context.Context, userID int64, convID, title string) {
	if title == "" {
		return
	}
	c, changed, err := s.convs.SetAutoTitle(ctx, userID, convID, title)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) { // deleted meanwhile
			s.log.WarnContext(ctx, "set conversation title", "conversation_id", convID, "err", err)
		}
		return
	}
	if changed {
		s.bus.Publish(userID, conversationUpdated(c))
	}
}

// titleAsync asks the titler for a better title once the first exchange of
// convID is stored. It runs in the background under the server's context.
func (s *Server) titleAsync(userID int64, convID string) {
	if s.titler == nil {
		return
	}
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(s.bgCtx, titles.Timeout)
		defer cancel()
		ms, err := s.convs.ListMessages(ctx, convID)
		if err != nil {
			s.log.WarnContext(ctx, "title: load messages", "conversation_id", convID, "err", err)
			return
		}
		userText, reply := firstExchange(ms)
		if userText == "" || reply == "" {
			return
		}
		t, err := s.titler.Title(ctx, userID, userText, reply)
		if err != nil {
			s.log.DebugContext(ctx, "title: no generated title", "conversation_id", convID, "err", err)
			return
		}
		s.setAutoTitle(ctx, userID, convID, cleanTitle(t))
	}()
}

// firstExchange returns the text of the first user message and the
// assistant text that follows it, up to the next user message.
func firstExchange(ms []*store.Message) (userText, reply string) {
	var parts []string
	for _, m := range ms {
		t := messageText(m)
		switch m.Role {
		case string(llm.RoleUser):
			if userText != "" {
				return userText, strings.Join(parts, "\n\n")
			}
			userText = t
		case string(llm.RoleAssistant):
			if userText != "" && t != "" {
				parts = append(parts, t)
			}
		}
	}
	return userText, strings.Join(parts, "\n\n")
}

// messageText returns the text blocks of a stored message.
func messageText(m *store.Message) string {
	var blocks []llm.ContentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	return strings.TrimSpace((llm.Message{Role: llm.Role(m.Role), Content: blocks}).Text())
}

// Close stops background work (title generation) and waits for it.
func (s *Server) Close() {
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgMu.Unlock()
	s.bgCancel()
	s.bg.Wait()
}

// waitBackground waits for background work without stopping it (tests).
func (s *Server) waitBackground() { s.bg.Wait() }

// handlePostMessage runs one agent turn and streams that conversation's
// events as SSE until the turn's done or error event. The turn runs in a
// goroutine under the request context (a client disconnect cancels it, POST
// .../stop stops it); the handler always waits for it to finish before
// returning.
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
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	if !s.startTurn(c.ID, cancel) {
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

	// The first message of an untitled conversation names it right away;
	// the titler improves on that once the first reply is in.
	first := c.MessageCount == 0 && autoTitled(c)
	if first {
		s.setAutoTitle(ctx, u.ID, c.ID, cleanTitle(titles.Quick(req.Content)))
	}

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
			cancel(nil) // the client is gone: cancel the turn
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
					cancel(nil)
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
			stopped := errors.Is(context.Cause(ctx), agent.ErrStopped)
			if !terminal {
				// The terminal event was dropped (or never published).
				switch {
				case runErr == nil || stopped:
					send(events.Event{Type: events.TypeDone, Data: events.DoneData{ConversationID: c.ID, Stopped: stopped}})
				default:
					send(events.Event{Type: events.TypeError, Data: events.ErrorData{Message: "turn failed", ConversationID: c.ID}})
				}
			}
			if d := sub.Dropped(); d > 0 {
				s.log.WarnContext(r.Context(), "messages: events dropped for slow client", "conversation_id", c.ID, "dropped", d)
			}
			if first && runErr == nil {
				s.titleAsync(u.ID, c.ID)
			}
			return
		}
	}
}
