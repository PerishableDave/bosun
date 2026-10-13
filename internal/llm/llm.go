// Package llm defines the provider-neutral model interface: messages, content
// blocks, tool specs, streaming events, and a registry of providers. Provider
// implementations live in subpackages (anthropic, openai, ollama) and
// translate to and from their own wire formats.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Provider is a language-model backend.
type Provider interface {
	// Name is the registry key, e.g. "anthropic". It is the part before "/"
	// in a model reference like "anthropic/claude-sonnet-5-5".
	Name() string
	// Complete performs a non-streaming request.
	Complete(ctx context.Context, req Request) (Response, error)
	// Stream performs a streaming request. The channel is closed after a
	// terminal EventDone or EventError, or when ctx is canceled. Exactly one
	// terminal event is sent unless ctx is canceled first.
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}

// Request is a provider-neutral completion request.
type Request struct {
	Model     string     // model name without provider prefix
	System    string     // system prompt
	Messages  []Message  // conversation, oldest first
	Tools     []ToolSpec // provider-neutral tool definitions
	MaxTokens int        // 0 means provider default
}

// Role is the author of a message.
type Role string

// Message roles. Tool results are carried in RoleTool messages; providers
// whose APIs put tool results in user messages translate accordingly.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// BlockType discriminates ContentBlock.
type BlockType string

// Content block types.
const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// ContentBlock is a tagged union; which fields are meaningful depends on Type.
//
//	text:        Text
//	tool_use:    ID, Name, Input   (assistant asks to call a tool)
//	tool_result: ToolUseID, Content, IsError   (result of a prior tool_use)
type ContentBlock struct {
	Type BlockType `json:"type"`

	Text string `json:"text,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// TextBlock returns a text content block.
func TextBlock(text string) ContentBlock { return ContentBlock{Type: BlockText, Text: text} }

// ToolUseBlock returns a tool_use content block.
func ToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

// ToolResultBlock returns a tool_result content block.
func ToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{Type: BlockToolResult, ToolUseID: toolUseID, Content: content, IsError: isError}
}

// NewTextMessage returns a single-text-block message.
func NewTextMessage(role Role, text string) Message {
	return Message{Role: role, Content: []ContentBlock{TextBlock(text)}}
}

// Text concatenates all text blocks in the message.
func (m Message) Text() string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == BlockText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// ToolUses returns the tool_use blocks in the message.
func (m Message) ToolUses() []ContentBlock {
	var out []ContentBlock
	for _, c := range m.Content {
		if c.Type == BlockToolUse {
			out = append(out, c)
		}
	}
	return out
}

// ToolSpec is a provider-neutral tool definition offered to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"` // JSON Schema object
}

// StopReason says why the model stopped.
type StopReason string

// Stop reasons.
const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopOther     StopReason = "other"
)

// Usage reports token counts for audit and budgeting.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is a complete model reply.
type Response struct {
	Model      string     `json:"model"`
	Message    Message    `json:"message"` // Role is always RoleAssistant
	StopReason StopReason `json:"stop_reason"`
	Usage      Usage      `json:"usage"`
}

// EventType discriminates streaming Events.
type EventType string

// Streaming event types.
const (
	EventTextDelta    EventType = "text_delta"     // Index, Text
	EventToolUseStart EventType = "tool_use_start" // Index, ToolUseID, ToolName
	EventToolUseDelta EventType = "tool_use_delta" // Index, InputDelta (partial JSON)
	EventToolUseStop  EventType = "tool_use_stop"  // Index
	EventDone         EventType = "done"           // Response (fully assembled)
	EventError        EventType = "error"          // Err
)

// Event is one streaming update. Index is the content block index within the
// assistant message being built.
type Event struct {
	Type       EventType
	Index      int
	Text       string
	ToolUseID  string
	ToolName   string
	InputDelta string
	// Response is set on EventDone and holds the fully assembled reply, so
	// consumers never need to reassemble deltas themselves.
	Response *Response
	Err      error
}

// ParseModelRef splits "provider/model" into its parts.
func ParseModelRef(ref string) (provider, model string, err error) {
	p, m, ok := strings.Cut(ref, "/")
	if !ok || p == "" || m == "" {
		return "", "", fmt.Errorf("llm: model reference %q must be <provider>/<model>", ref)
	}
	return p, m, nil
}

// ErrUnknownProvider is returned by Registry lookups for unregistered names.
var ErrUnknownProvider = errors.New("llm: unknown provider")

// Registry holds configured providers keyed by Name(). It is safe for
// concurrent use.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{providers: map[string]Provider{}} }

// Register adds p. It fails if p is nil, unnamed, or a duplicate.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return errors.New("llm: register nil provider")
	}
	name := p.Name()
	if name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("llm: invalid provider name %q", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[name]; dup {
		return fmt.Errorf("llm: provider %q already registered", name)
	}
	r.providers[name] = p
	return nil
}

// Get returns the provider registered under name.
func (r *Registry) Get(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	return p, nil
}

// Resolve parses "provider/model" and returns the provider and bare model name.
func (r *Registry) Resolve(ref string) (Provider, string, error) {
	pn, model, err := ParseModelRef(ref)
	if err != nil {
		return nil, "", err
	}
	p, err := r.Get(pn)
	if err != nil {
		return nil, "", err
	}
	return p, model, nil
}

// Names returns registered provider names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
