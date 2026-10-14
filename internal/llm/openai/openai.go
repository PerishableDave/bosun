// Package openai implements llm.Provider for any OpenAI-compatible chat
// completions endpoint (OpenAI, Ollama's /v1, vLLM, LM Studio, ...).
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/perishabledave/bosun/internal/llm"
)

const maxErrBody = 512

// Config configures a Provider.
type Config struct {
	Name    string // registry name; default "openai"
	BaseURL string // required, e.g. https://api.openai.com/v1
	APIKey  string // optional; sent as a Bearer token, never logged
	// KeyFunc, if set, is called for every request instead of using
	// APIKey (see anthropic.Config.KeyFunc).
	KeyFunc    func(ctx context.Context) (string, error)
	HTTPClient *http.Client // optional
}

// Provider is an OpenAI-compatible llm.Provider.
type Provider struct {
	name      string
	baseURL   string
	apiKey    string
	keyFunc   func(ctx context.Context) (string, error)
	client    *http.Client
	maxTokKey string
}

var _ llm.Provider = (*Provider)(nil)

// New returns a Provider. BaseURL is required.
func New(cfg Config) (*Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("openai: BaseURL is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("openai: invalid BaseURL %q", cfg.BaseURL)
	}
	name := cfg.Name
	if name == "" {
		name = "openai"
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{}
	}
	key := "max_tokens"
	if u.Hostname() == "api.openai.com" {
		key = "max_completion_tokens" // required by newer OpenAI models
	}
	return &Provider{name: name, baseURL: base, apiKey: cfg.APIKey, keyFunc: cfg.KeyFunc, client: c, maxTokKey: key}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.name }

// APIError is returned for non-2xx responses.
type APIError struct {
	Provider string
	Status   int
	Body     string // truncated
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: API error: HTTP %d: %s", e.Provider, e.Status, e.Body)
}

// ---- wire types ----

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type wireChoice struct {
	Message struct {
		Content   *string        `json:"content"`
		ToolCalls []wireToolCall `json:"tool_calls"`
	} `json:"message"`
	Delta struct {
		Content   *string        `json:"content"`
		ToolCalls []wireToolCall `json:"tool_calls"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type wireResponse struct {
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func strp(s string) *string { return &s }

func buildRequest(maxTokKey string, req llm.Request, stream bool) ([]byte, error) {
	if req.Model == "" {
		return nil, errors.New("openai: request model is empty")
	}
	var msgs []wireMessage
	if req.System != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: strp(req.System)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleAssistant:
			wm := wireMessage{Role: "assistant"}
			if t := m.Text(); t != "" {
				wm.Content = strp(t)
			}
			for _, b := range m.Content {
				if b.Type != llm.BlockToolUse {
					continue
				}
				tc := wireToolCall{ID: b.ID, Type: "function"}
				tc.Function.Name = b.Name
				args := string(bytes.TrimSpace(b.Input))
				if args == "" {
					args = "{}"
				}
				tc.Function.Arguments = args
				wm.ToolCalls = append(wm.ToolCalls, tc)
			}
			if wm.Content == nil && len(wm.ToolCalls) == 0 {
				continue
			}
			msgs = append(msgs, wm)
		default: // user or tool
			// Tool results first (they must follow the assistant tool_calls).
			for _, b := range m.Content {
				if b.Type == llm.BlockToolResult {
					c := b.Content
					if b.IsError {
						c = "Error: " + c
					}
					msgs = append(msgs, wireMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: strp(c)})
				}
			}
			if t := m.Text(); t != "" {
				msgs = append(msgs, wireMessage{Role: "user", Content: strp(t)})
			}
		}
	}
	body := map[string]any{"model": req.Model, "messages": msgs}
	if req.MaxTokens > 0 {
		body[maxTokKey] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		var tools []wireTool
		for _, t := range req.Tools {
			wt := wireTool{Type: "function"}
			wt.Function.Name = t.Name
			wt.Function.Description = t.Description
			params := t.InputSchema
			if len(bytes.TrimSpace(params)) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			wt.Function.Parameters = params
			tools = append(tools, wt)
		}
		body["tools"] = tools
	}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return b, nil
}

func (p *Provider) do(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", p.name, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	key := p.apiKey
	if p.keyFunc != nil {
		if key, err = p.keyFunc(ctx); err != nil {
			return nil, fmt.Errorf("%s: API key: %w", p.name, err)
		}
	}
	if key != "" {
		hreq.Header.Set("Authorization", "Bearer "+key)
	}
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", p.name, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody+1))
		s := strings.TrimSpace(string(b))
		if len(s) > maxErrBody {
			s = s[:maxErrBody] + "...(truncated)"
		}
		return nil, &APIError{Provider: p.name, Status: resp.StatusCode, Body: s}
	}
	return resp, nil
}

func mapStop(s string, hasTools bool) llm.StopReason {
	switch s {
	case "tool_calls", "function_call":
		return llm.StopToolUse
	case "length":
		return llm.StopMaxTokens
	case "stop":
		if hasTools { // some servers report "stop" with tool calls
			return llm.StopToolUse
		}
		return llm.StopEndTurn
	default:
		if hasTools {
			return llm.StopToolUse
		}
		return llm.StopOther
	}
}

// argsJSON returns a valid JSON object for tool arguments.
func argsJSON(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]string{"_raw": s})
	return json.RawMessage(b)
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := buildRequest(p.maxTokKey, req, false)
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
		return llm.Response{}, fmt.Errorf("%s: decode response: %w", p.name, err)
	}
	if len(wr.Choices) == 0 {
		return llm.Response{}, fmt.Errorf("%s: response has no choices", p.name)
	}
	ch := wr.Choices[0]
	out := llm.Response{Model: wr.Model, Message: llm.Message{Role: llm.RoleAssistant}}
	if ch.Message.Content != nil && *ch.Message.Content != "" {
		out.Message.Content = append(out.Message.Content, llm.TextBlock(*ch.Message.Content))
	}
	for i, tc := range ch.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		out.Message.Content = append(out.Message.Content, llm.ToolUseBlock(id, tc.Function.Name, argsJSON(tc.Function.Arguments)))
	}
	fr := ""
	if ch.FinishReason != nil {
		fr = *ch.FinishReason
	}
	out.StopReason = mapStop(fr, len(ch.Message.ToolCalls) > 0)
	if wr.Usage != nil {
		out.Usage = llm.Usage{InputTokens: wr.Usage.PromptTokens, OutputTokens: wr.Usage.CompletionTokens}
	}
	return out, nil
}

type asmTool struct {
	local int
	id    string
	name  string
	args  strings.Builder
}

// Stream implements llm.Provider. HTTP-level failures are returned directly;
// failures after the stream starts are delivered as one EventError.
func (p *Provider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	body, err := buildRequest(p.maxTokKey, req, true)
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
		order    []any // *strings.Builder for text, *asmTool for tools, in local index order
		text     *strings.Builder
		textIdx  int
		tools    = map[int]*asmTool{}
		usage    llm.Usage
		finish   string
		gotEnd   bool
		respModl = model
	)
	err := readSSE(p.name, r, func(data string) (bool, error) {
		if data == "[DONE]" {
			gotEnd = true
			return true, nil
		}
		var wr wireResponse
		if err := json.Unmarshal([]byte(data), &wr); err != nil {
			return false, fmt.Errorf("%s: decode stream chunk: %w", p.name, err)
		}
		if wr.Error != nil {
			return true, fmt.Errorf("%s: stream error: %s", p.name, wr.Error.Message)
		}
		if wr.Model != "" {
			respModl = wr.Model
		}
		if wr.Usage != nil {
			usage = llm.Usage{InputTokens: wr.Usage.PromptTokens, OutputTokens: wr.Usage.CompletionTokens}
		}
		if len(wr.Choices) == 0 {
			return false, nil
		}
		c := wr.Choices[0]
		if c.Delta.Content != nil && *c.Delta.Content != "" {
			if text == nil {
				text = &strings.Builder{}
				textIdx = len(order)
				order = append(order, text)
			}
			text.WriteString(*c.Delta.Content)
			if !send(llm.Event{Type: llm.EventTextDelta, Index: textIdx, Text: *c.Delta.Content}) {
				return true, ctx.Err()
			}
		}
		for _, tc := range c.Delta.ToolCalls {
			t, ok := tools[tc.Index]
			if !ok {
				t = &asmTool{local: len(order), id: tc.ID, name: tc.Function.Name}
				if t.id == "" {
					t.id = fmt.Sprintf("call_%d", tc.Index)
				}
				tools[tc.Index] = t
				order = append(order, t)
				if !send(llm.Event{Type: llm.EventToolUseStart, Index: t.local, ToolUseID: t.id, ToolName: t.name}) {
					return true, ctx.Err()
				}
			} else if t.name == "" && tc.Function.Name != "" {
				t.name = tc.Function.Name
			}
			if a := tc.Function.Arguments; a != "" {
				t.args.WriteString(a)
				if !send(llm.Event{Type: llm.EventToolUseDelta, Index: t.local, InputDelta: a}) {
					return true, ctx.Err()
				}
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			finish = *c.FinishReason
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if !gotEnd && finish == "" {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: stream ended unexpectedly", p.name)
	}
	out := &llm.Response{
		Model:      respModl,
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: mapStop(finish, len(tools) > 0),
		Usage:      usage,
	}
	for _, o := range order {
		switch v := o.(type) {
		case *strings.Builder:
			out.Message.Content = append(out.Message.Content, llm.TextBlock(v.String()))
		case *asmTool:
			if !send(llm.Event{Type: llm.EventToolUseStop, Index: v.local}) {
				return nil, ctx.Err()
			}
			out.Message.Content = append(out.Message.Content, llm.ToolUseBlock(v.id, v.name, argsJSON(v.args.String())))
		}
	}
	return out, nil
}

func readSSE(name string, r io.Reader, fn func(data string) (stop bool, err error)) error {
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
			return fmt.Errorf("%s: read stream: %w", name, err)
		}
	}
}
