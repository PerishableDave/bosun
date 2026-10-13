// Package integration defines the contract every integration (first-party or
// MCP-bridged) implements, and a Registry that enforces the capability rules
// at registration time.
//
// Integrations act only through Tools. Each Tool declares the Capability it
// exercises and its Risk; the dispatcher (not the integration) consults policy
// before invoking a Tool's Handler.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Capability names a permission a user can grant, e.g. "email.send". By
// convention it is "<integration>.<action>" (for MCP: "mcp.<server>.<tool>").
type Capability string

// Risk classifies how dangerous a tool call is. Policy defaults are keyed by
// risk when no explicit rule matches.
type Risk string

// Risk levels.
const (
	RiskRead        Risk = "read"
	RiskWrite       Risk = "write"
	RiskDestructive Risk = "destructive"
)

// Valid reports whether r is a known risk level.
func (r Risk) Valid() bool { return r == RiskRead || r == RiskWrite || r == RiskDestructive }

// Scope declares whose credentials an integration uses. Every integration
// must declare one.
type Scope string

// Scopes.
const (
	// ScopePerUser integrations act on each user's own account (e.g. email,
	// calendar) and receive that user's credentials from CredentialSource.
	ScopePerUser Scope = "per_user"
	// ScopeShared integrations talk to a single backend shared by the whole
	// household (e.g. a self-hosted SearXNG). Their CredentialSource returns
	// admin-configured credentials regardless of the calling user.
	ScopeShared Scope = "shared"
)

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return s == ScopePerUser || s == ScopeShared }

// Integration is a pluggable bundle of tools.
//
// Lifecycle: the daemon calls Init once at startup with the admin-level
// Config and a CredentialSource, then registers the integration (Registry
// validates Manifest and Tools, so tools discovered at runtime, e.g. by the
// MCP bridge, must be available from Tools by then), and calls Close at
// shutdown.
type Integration interface {
	Manifest() Manifest
	Init(ctx context.Context, cfg Config, creds CredentialSource) error
	Tools() []Tool
	Close() error
}

// Manifest describes an integration up front.
type Manifest struct {
	Name    string // "email"; must match ^[a-z][a-z0-9_-]*$
	Version string
	// Scope declares whether the integration takes per-user credentials or
	// is a shared backend. Required.
	Scope Scope
	// Capabilities lists every capability any tool may use. Each must be
	// prefixed "<Name>." so an integration cannot claim another's namespace.
	Capabilities    []Capability
	ConfigSchema    json.RawMessage // JSON Schema for Config.Settings; optional
	CredentialKinds []string        // "oauth2:google", "apikey", ...
}

// HasCapability reports whether c is declared in the manifest.
func (m Manifest) HasCapability(c Capability) bool {
	for _, x := range m.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// Handler executes a tool call. in is the model-supplied arguments, already
// passed through policy and pre_tool_call hooks. The calling user is
// available via UserIDFromContext.
type Handler func(ctx context.Context, in json.RawMessage) (Result, error)

// Tool is one model-callable action.
type Tool struct {
	Name        string          // "email.send"; unique across all integrations
	Description string          // shown to the model
	Capability  Capability      // "email.send"; must be in Manifest.Capabilities
	Risk        Risk            // Read | Write | Destructive
	InputSchema json.RawMessage // JSON Schema object for the arguments
	Handler     Handler
}

// Result is a tool's output.
type Result struct {
	Content string // text handed to the model
	// Untrusted is true for anything originating outside Bosun (emails, web
	// pages, documents, all MCP results). The agent wraps untrusted content
	// in a delimited data block and the model is told it is never
	// instructions.
	Untrusted bool
	Data      any // structured result for the UI / hooks
}

// Config is the admin-level configuration handed to Init: the integration's
// section of bosun.yaml (minus "enabled"), as JSON.
type Config struct {
	Name     string
	Settings json.RawMessage
	// DataDir is a private directory the integration may use for caches.
	DataDir string
}

// Decode unmarshals Settings into v.
func (c Config) Decode(v any) error {
	if len(c.Settings) == 0 {
		return nil
	}
	if err := json.Unmarshal(c.Settings, v); err != nil {
		return fmt.Errorf("integration %s: decode config: %w", c.Name, err)
	}
	return nil
}

// Credential is a secret supplied to an integration.
type Credential struct {
	Kind   string            // one of Manifest.CredentialKinds
	Secret string            // API key, access token, password, ...
	Fields map[string]string // extra values (refresh_token, username, server, ...)
	Expiry time.Time         // zero if it does not expire
}

// ErrNoCredential is returned by CredentialSource.Get when the user has not
// provided a credential of the requested kind.
var ErrNoCredential = errors.New("integration: no credential")

// CredentialSource yields credentials scoped to a single user. The user is
// taken from ctx (see WithUserID), which the dispatcher sets before invoking a
// Handler; integrations therefore call Get with the ctx they were given and
// never see or choose the user ID themselves. For ScopeShared integrations the
// source returns the shared credential regardless of user.
type CredentialSource interface {
	Get(ctx context.Context, kind string) (Credential, error)
}

type userIDKey struct{}

// WithUserID returns ctx carrying the ID of the user a tool call runs as.
// Only the dispatcher should call this.
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext returns the user a tool call runs as.
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(userIDKey{}).(int64)
	return id, ok
}
