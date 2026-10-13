package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// Event is something that happened outside a conversation (for example an
// integration noticing new mail) that routines may react to. Payload is
// event-specific JSON and must be treated as untrusted data by consumers.
type Event struct {
	// Name is a dotted lower-case identifier such as "email.received".
	Name string
	// UserID is the user the event belongs to; 0 means every user.
	UserID int64
	// Payload is the event data (may be empty).
	Payload json.RawMessage
}

// EventHandler receives emitted events (the on_event hook point). Handlers
// are called synchronously, in registration order, by EmitEvent; they must
// not block for long (start goroutines for real work).
type EventHandler func(ctx context.Context, ev Event)

var eventNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+)*$`)

// MaxEventNameLen bounds event names.
const MaxEventNameLen = 100

// ValidEventName reports whether name is a well-formed event name: dotted
// segments of lower-case letters, digits, '_' and '-', starting with a
// letter, at most MaxEventNameLen bytes.
func ValidEventName(name string) bool {
	return len(name) <= MaxEventNameLen && eventNameRe.MatchString(name)
}

// OnEvent registers an on_event handler under a unique hook name.
func (r *Registry) OnEvent(name string, fn EventHandler) error {
	return r.add(PointOnEvent, name, fn == nil, nil, func() { r.events = append(r.events, Named[EventHandler]{name, fn}) })
}

// EventHandlers returns the on_event handlers in registration order.
func (r *Registry) EventHandlers() []Named[EventHandler] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Named[EventHandler](nil), r.events...)
}

// EmitEvent delivers ev to every on_event handler. It returns an error only
// for a malformed event (bad name, negative user ID, or non-JSON payload);
// handlers do not report errors back to the emitter.
func (r *Registry) EmitEvent(ctx context.Context, ev Event) error {
	if !ValidEventName(ev.Name) {
		return fmt.Errorf("hooks: invalid event name %q", ev.Name)
	}
	if ev.UserID < 0 {
		return fmt.Errorf("hooks: invalid event user id %d", ev.UserID)
	}
	if len(ev.Payload) > 0 && !json.Valid(ev.Payload) {
		return fmt.Errorf("hooks: event %s: payload is not valid JSON", ev.Name)
	}
	for _, h := range r.EventHandlers() {
		h.Fn(ctx, ev)
	}
	return nil
}
