package integration

import (
	"context"
	"encoding/json"
)

// EmitFunc delivers an event (e.g. "email.received") on behalf of userID to
// the daemon's on_event hooks, which is how routines with
// `trigger: {event: ...}` fire. The payload is treated as untrusted data by
// every consumer.
type EmitFunc func(ctx context.Context, userID int64, name string, payload json.RawMessage) error

// EventEmitter is optionally implemented by integrations that produce
// events outside tool calls (e.g. a mail poller). The daemon calls
// SetEmitter once, after Init and after routines are subscribed.
type EventEmitter interface {
	SetEmitter(emit func(ctx context.Context, userID int64, name string, payload json.RawMessage) error)
}

// UserLister is optionally implemented by per-user integrations that do
// background work for each linked user. The daemon supplies a function
// returning the IDs of (enabled) users that may have credentials for the
// integration; the integration then acts for each with WithUserID, so its
// CredentialSource still yields only that user's credential.
type UserLister interface {
	SetUserLister(list func(ctx context.Context) ([]int64, error))
}
