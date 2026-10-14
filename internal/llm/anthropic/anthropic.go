// Package anthropic implements llm.Provider against the Anthropic Messages
// API using only net/http.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/perishabledave/bosun/internal/llm"
)

// Defaults.
const (
	DefaultBaseURL   = "https://api.anthropic.com"
	DefaultMaxTokens = 8192
	apiVersion       = "2023-06-01"
	maxErrBody       = 512
)

// Config configures a Provider.
type Config struct {
	APIKey     string       // sent as x-api-key; never logged
	BaseURL    string       // default https://api.anthropic.com
	HTTPClient *http.Client // optional; default has no overall timeout (ctx governs)
}

// Provider is the Anthropic llm.Provider.
type Provider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New returns a Provider.
func New(cfg Config) *Provider {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{}
	}
	return &Provider{apiKey: cfg.APIKey, baseURL: base, client: c}
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "anthropic" }

// APIError is returned for non-2xx responses.
type APIError struct {
	Status int
	Body   string // truncated
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic: API error: HTTP %d: %s", e.Status, e.Body)
}

// ---- wire types ----

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	Stream    bool          `json:"stream,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *string         `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type wireResponse struct {
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      wireUsage   `json:"usage"`
}

func buildRequest(req llm.Request, stream bool) ([]byte, error) {
	if req.Model == "" {
		return nil, errors.New("anthropic: request model is empty")
	}
	max := req.MaxTokens
	if max <= 0 {
		max = DefaultMaxTokens
	}
	w := wireRequest{Model: req.Model, MaxTokens: max, System: req.System, Stream: stream}
	for _, m := range req.Messages {
		role := "user"
		if m.Role == llm.RoleAssistant {
			role = "assistant"
		}
		wm := wireMessage{Role: role}
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				if b.Text == "" {
					continue // API rejects empty text blocks
				}
				t := b.Text
				wm.Content = append(wm.Content, wireBlock{Type: "text", Text: &t})
			case llm.BlockToolUse:
				in := b.Input
				if len(bytes.TrimSpace(in)) == 0 {
					in = json.RawMessage("{}")
				}
				wm.Content = append(wm.Content, wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: in})
			case llm.BlockToolResult:
				c := b.Content
				wm.Content = append(wm.Content, wireBlock{Type: "tool_result", ToolUseID: b.ToolUseID, Content: &c, IsError: b.IsError})
			default:
				return nil, fmt.Errorf("anthropic: unsupported content block type %q", b.Type)
			}
		}
		if len(wm.Content) == 0 {
			continue
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(bytes.TrimSpace(schema)) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		w.Tools = append(w.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	b, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode request: %w", err)
	}
	return b, nil
}

func mapStop(s string) llm.StopReason {
	switch s {
	case "end_turn", "stop_sequence":
		return llm.StopEndTurn
	case "tool_use":
		return llm.StopToolUse
	case "max_tokens":
		return llm.StopMaxTokens
	default:
		return llm.StopOther
	}
}

func (p *Provider) do(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("anthropic-version", apiVersion)
	if p.apiKey != "" {
		hreq.Header.Set("x-api-key", p.apiKey)
	}
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody+1))
		s := strings.TrimSpace(string(b))
		if len(s) > maxErrBody {
			s = s[:maxErrBody] + "...(truncated)"
		}
		return nil, &APIError{Status: resp.StatusCode, Body: s}
	}
	return resp, nil
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := buildRequest(req, false)
	if err != nil {
		return llm.Response{}, err
	}
	resp, err := p.do(ctx, body, false)
	if err != nil {
		return llm.Response{}, err
	}
	defer resp.Body.Close()
	var wr wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return llm.Response{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	out := llm.Response{
		Model:      wr.Model,
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: mapStop(wr.StopReason),
		Usage:      llm.Usage{InputTokens: wr.Usage.InputTokens, OutputTokens: wr.Usage.OutputTokens},
	}
	for _, b := range wr.Content {
		switch b.Type {
		case "text":
			t := ""
			if b.Text != nil {
				t = *b.Text
			}
			out.Message.Content = append(out.Message.Content, llm.TextBlock(t))
		case "tool_use":
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			out.Message.Content = append(out.Message.Content, llm.ToolUseBlock(b.ID, b.Name, in))
		}
	}
	return out, nil
}

// ---- streaming ----

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string    `json:"model"`
		Usage wireUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type asmBlock struct {
	kind  string // "text" or "tool_use"
	text  strings.Builder
	id    string
	name  string
	input strings.Builder
}

// Stream implements llm.Provider. HTTP-level failures (connection errors,
// non-2xx) are returned directly; failures after the stream starts are
// delivered as one EventError.
func (p *Provider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	body, err := buildRequest(req, true)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, body, true)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.Event, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		send := func(e llm.Event) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		final, err := p.consume(ctx, resp.Body, req.Model, send)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			send(llm.Event{Type: llm.EventError, Err: err})
			return
		}
		send(llm.Event{Type: llm.EventDone, Response: final})
	}()
	return ch, nil
}

func (p *Provider) consume(ctx context.Context, r io.Reader, model string, send func(llm.Event) bool) (*llm.Response, error) {
	var (
		blocks   []*asmBlock
		localIdx = map[int]int{} // upstream index -> local index
		usage    llm.Usage
		stop     string
		gotStop  bool
		respModl = model
	)
	err := readSSE(r, func(data string) (bool, error) {
		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return false, fmt.Errorf("anthropic: decode stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				if ev.Message.Model != "" {
					respModl = ev.Message.Model
				}
				usage.InputTokens = ev.Message.Usage.InputTokens
				usage.OutputTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				return false, nil
			}
			switch ev.ContentBlock.Type {
			case "text":
				li := len(blocks)
				localIdx[ev.Index] = li
				blocks = append(blocks, &asmBlock{kind: "text"})
				if t := ev.ContentBlock.Text; t != "" {
					blocks[li].text.WriteString(t)
					if !send(llm.Event{Type: llm.EventTextDelta, Index: li, Text: t}) {
						return true, ctx.Err()
					}
				}
			case "tool_use":
				li := len(blocks)
				localIdx[ev.Index] = li
				blocks = append(blocks, &asmBlock{kind: "tool_use", id: ev.ContentBlock.ID, name: ev.ContentBlock.Name})
				if !send(llm.Event{Type: llm.EventToolUseStart, Index: li, ToolUseID: ev.ContentBlock.ID, ToolName: ev.ContentBlock.Name}) {
					return true, ctx.Err()
				}
			}
			// thinking and other block types are not surfaced.
		case "content_block_delta":
			li, ok := localIdx[ev.Index]
			if !ok || ev.Delta == nil {
				return false, nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				blocks[li].text.WriteString(ev.Delta.Text)
				if !send(llm.Event{Type: llm.EventTextDelta, Index: li, Text: ev.Delta.Text}) {
					return true, ctx.Err()
				}
			case "input_json_delta":
				blocks[li].input.WriteString(ev.Delta.PartialJSON)
				if !send(llm.Event{Type: llm.EventToolUseDelta, Index: li, InputDelta: ev.Delta.PartialJSON}) {
					return true, ctx.Err()
				}
			}
		case "content_block_stop":
			li, ok := localIdx[ev.Index]
			if ok && blocks[li].kind == "tool_use" {
				if !send(llm.Event{Type: llm.EventToolUseStop, Index: li}) {
					return true, ctx.Err()
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				if ev.Usage.OutputTokens > 0 {
					usage.OutputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.InputTokens > 0 {
					usage.InputTokens = ev.Usage.InputTokens
				}
			}
		case "message_stop":
			gotStop = true
			return true, nil
		case "error":
			msg := "unknown error"
			typ := ""
			if ev.Error != nil {
				msg, typ = ev.Error.Message, ev.Error.Type
			}
			return true, fmt.Errorf("anthropic: stream error (%s): %s", typ, msg)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if !gotStop {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("anthropic: stream ended before message_stop")
	}
	out := &llm.Response{
		Model:      respModl,
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: mapStop(stop),
		Usage:      usage,
	}
	for _, b := range blocks {
		if b.kind == "text" {
			out.Message.Content = append(out.Message.Content, llm.TextBlock(b.text.String()))
			continue
		}
		in := strings.TrimSpace(b.input.String())
		if in == "" {
			in = "{}"
		}
		if !json.Valid([]byte(in)) {
			return nil, fmt.Errorf("anthropic: tool %q produced invalid JSON input", b.name)
		}
		out.Message.Content = append(out.Message.Content, llm.ToolUseBlock(b.id, b.name, json.RawMessage(in)))
	}
	return out, nil
}

// readSSE reads server-sent events from r and calls fn with each event's data
// payload. fn returns stop=true to end reading.
func readSSE(r io.Reader, fn func(data string) (stop bool, err error)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data []string
	flush := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		d := strings.Join(data, "\n")
		data = data[:0]
		return fn(d)
	}
	for {
		line, err := br.ReadString('\n')
		if line != "" || err == nil {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if stop, ferr := flush(); ferr != nil || stop {
					return ferr
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if err != nil {
			if err == io.EOF {
				_, ferr := flush()
				return ferr
			}
			return fmt.Errorf("anthropic: read stream: %w", err)
		}
	}
}
