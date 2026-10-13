// Package events is a small in-process, per-user event bus plus helpers for
// writing Server-Sent Events. Any package may publish to a user by ID; the
// API's GET /v1/events streams that user's events to each connected client.
package events

import (
	"encoding/json"
	"sync"
)

// Event types sent over SSE. See docs/API.md.
const (
	TypeText             = "text"
	TypeToolCall         = "tool_call"
	TypeToolResult       = "tool_result"
	TypeApprovalRequired = "approval_required"
	TypeApprovalResolved = "approval_resolved"
	TypeDone             = "done"
	TypeError            = "error"
)

// Event is one SSE event: Type becomes the SSE "event:" field and Data is
// JSON-encoded into the "data:" field.
type Event struct {
	Type string
	Data any
}

// Payloads for each event type.
//
// Every payload carries the conversation it belongs to (omitted when there is
// none), so a stream for one turn can filter the user's bus down to that
// conversation; see ConversationOf.
type (
	// TextData is a streamed chunk of assistant text.
	TextData struct {
		Delta          string `json:"delta"`
		ConversationID string `json:"conversation_id,omitempty"`
	}
	// ToolCallData announces a tool call the model made.
	ToolCallData struct {
		ID             string          `json:"id"`
		Name           string          `json:"name"`
		Args           json.RawMessage `json:"args"`
		ConversationID string          `json:"conversation_id,omitempty"`
	}
	// ToolResultData summarises a tool call's outcome.
	ToolResultData struct {
		ID             string `json:"id"`
		Summary        string `json:"summary"`
		ConversationID string `json:"conversation_id,omitempty"`
	}
	// ApprovalRequiredData asks the user to approve or deny a tool call.
	ApprovalRequiredData struct {
		ApprovalID     string          `json:"approval_id"`
		Tool           string          `json:"tool"`
		Args           json.RawMessage `json:"args"`
		Risk           string          `json:"risk"`
		ConversationID string          `json:"conversation_id,omitempty"`
	}
	// ApprovalResolvedData reports an approval's outcome to all clients.
	ApprovalResolvedData struct {
		ApprovalID     string `json:"approval_id"`
		Decision       string `json:"decision"` // "approve" | "deny" | "expired" | "canceled"
		ConversationID string `json:"conversation_id,omitempty"`
	}
	// DoneData ends a message stream.
	DoneData struct {
		ConversationID string `json:"conversation_id,omitempty"`
	}
	// ErrorData reports a failure.
	ErrorData struct {
		Message        string `json:"message"`
		ConversationID string `json:"conversation_id,omitempty"`
	}
)

// ConversationOf returns the conversation ID carried by ev's payload, or ""
// if it has none (or the payload is not one of this package's types).
func ConversationOf(ev Event) string {
	switch d := ev.Data.(type) {
	case TextData:
		return d.ConversationID
	case ToolCallData:
		return d.ConversationID
	case ToolResultData:
		return d.ConversationID
	case ApprovalRequiredData:
		return d.ConversationID
	case ApprovalResolvedData:
		return d.ConversationID
	case DoneData:
		return d.ConversationID
	case ErrorData:
		return d.ConversationID
	}
	return ""
}

// Publisher is the interface other packages depend on to notify a user's
// connected clients.
type Publisher interface {
	Publish(userID int64, ev Event)
}

// Bus fans events out to each user's subscribers. Publishing never blocks: if
// a subscriber's buffer is full the event is dropped for that subscriber
// (clients recover state via the REST endpoints, e.g. GET /v1/approvals).
type Bus struct {
	mu   sync.RWMutex
	subs map[int64]map[*Subscription]struct{}
}

// NewBus returns an empty Bus.
func NewBus() *Bus { return &Bus{subs: map[int64]map[*Subscription]struct{}{}} }

// Subscription receives one user's events until closed.
type Subscription struct {
	C       <-chan Event
	c       chan Event
	bus     *Bus
	userID  int64
	once    sync.Once
	mu      sync.Mutex
	dropped int
}

// DefaultBuffer is the per-subscriber buffer size used when Subscribe is
// given buf <= 0.
const DefaultBuffer = 64

// Subscribe registers a subscriber for userID. Call Close when done.
func (b *Bus) Subscribe(userID int64, buf int) *Subscription {
	if buf <= 0 {
		buf = DefaultBuffer
	}
	c := make(chan Event, buf)
	s := &Subscription{C: c, c: c, bus: b, userID: userID}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[userID] == nil {
		b.subs[userID] = map[*Subscription]struct{}{}
	}
	b.subs[userID][s] = struct{}{}
	return s
}

// Close unsubscribes and closes C. It is idempotent.
func (s *Subscription) Close() {
	s.once.Do(func() {
		b := s.bus
		b.mu.Lock()
		delete(b.subs[s.userID], s)
		if len(b.subs[s.userID]) == 0 {
			delete(b.subs, s.userID)
		}
		close(s.c)
		b.mu.Unlock()
	})
}

// Dropped returns how many events were dropped because the buffer was full.
func (s *Subscription) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Publish delivers ev to every subscriber of userID without blocking.
func (b *Bus) Publish(userID int64, ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs[userID] {
		select {
		case s.c <- ev:
		default:
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
		}
	}
}

// Subscribers returns the number of active subscribers for userID.
func (b *Bus) Subscribers(userID int64) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[userID])
}

var _ Publisher = (*Bus)(nil)
