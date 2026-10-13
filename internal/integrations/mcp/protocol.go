package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ProtocolVersion is the MCP revision Bosun asks for in initialize. If the
// server answers with another revision, Bosun proceeds anyway: the parts it
// uses (tools/list, tools/call) are stable across revisions.
const ProtocolVersion = "2025-06-18"

const (
	clientName = "bosun"
	// maxListPages bounds tools/list pagination against a looping cursor.
	maxListPages = 100
	// maxResultChars bounds the text handed to the model per call.
	maxResultChars = 100_000
)

// transport carries JSON-RPC 2.0 messages to one MCP server.
type transport interface {
	// request sends a request and waits for its response's result.
	request(ctx context.Context, method string, params any) (json.RawMessage, error)
	// notify sends a notification (no response expected).
	notify(ctx context.Context, method string, params any) error
	// setProtocolVersion records the negotiated revision (HTTP sends it as a
	// header on every later request).
	setProtocolVersion(v string)
	// err is non-nil once the connection can no longer be used.
	err() error
	close() error
}

// rpcRequest is an outgoing request or notification (ID nil).
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is an outgoing response to a server-initiated request.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcMessage is any incoming message. Unknown fields are ignored.
type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

func (m rpcMessage) hasID() bool { return len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null")) }

// isResponse reports whether m answers one of our requests.
func (m rpcMessage) isResponse() bool { return m.Method == "" && m.hasID() }

// isRequest reports whether m is a server-initiated request.
func (m rpcMessage) isRequest() bool { return m.Method != "" && m.hasID() }

// numericID parses the response id. Bosun sends integer ids; servers echo
// them, some as strings.
func (m rpcMessage) numericID() (int64, bool) {
	s := strings.Trim(string(m.ID), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message) }

// replyTo answers a server-initiated request: ping succeeds, everything else
// (sampling, roots, elicitation) is not supported by Bosun.
func replyTo(m rpcMessage) rpcResponse {
	if m.Method == "ping" {
		return rpcResponse{JSONRPC: "2.0", ID: m.ID, Result: struct{}{}}
	}
	return rpcResponse{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not supported by client: " + m.Method}}
}

// ---- MCP messages ----

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type remoteTool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations *struct {
		Title           string `json:"title,omitempty"`
		ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
		DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	} `json:"annotations,omitempty"`
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	Resource *struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
		Blob     string `json:"blob,omitempty"`
	} `json:"resource,omitempty"`
}

type callResult struct {
	Content           []contentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// handshake runs initialize + notifications/initialized and returns the
// server's answer.
func handshake(ctx context.Context, t transport, clientVersion string) (initializeResult, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	}
	raw, err := t.request(ctx, "initialize", params)
	if err != nil {
		return initializeResult{}, fmt.Errorf("initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return initializeResult{}, fmt.Errorf("initialize: decode result: %w", err)
	}
	if res.ProtocolVersion == "" {
		res.ProtocolVersion = ProtocolVersion
	}
	t.setProtocolVersion(res.ProtocolVersion)
	if err := t.notify(ctx, "notifications/initialized", nil); err != nil {
		return initializeResult{}, fmt.Errorf("notifications/initialized: %w", err)
	}
	return res, nil
}

// listTools fetches every page of tools/list.
func listTools(ctx context.Context, t transport) ([]remoteTool, error) {
	var all []remoteTool
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("tools/list: more than %d pages", maxListPages)
		}
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := t.request(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var res struct {
			Tools      []remoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor,omitempty"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("tools/list: decode result: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" || res.NextCursor == cursor {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

// callTool runs tools/call.
func callTool(ctx context.Context, t transport, name string, args json.RawMessage) (callResult, error) {
	raw, err := t.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return callResult{}, err
	}
	var res callResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return callResult{}, fmt.Errorf("tools/call: decode result: %w", err)
	}
	return res, nil
}

// renderContent converts content blocks to text for the model: text blocks
// verbatim, everything else summarized.
func renderContent(res callResult) string {
	var parts []string
	for _, b := range res.Content {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s %s]", b.Type, orUnknown(b.MimeType)))
		case "resource":
			if b.Resource == nil {
				parts = append(parts, "[resource]")
				continue
			}
			head := fmt.Sprintf("[resource %s %s]", b.Resource.URI, orUnknown(b.Resource.MimeType))
			if b.Resource.Text != "" {
				head += "\n" + b.Resource.Text
			}
			parts = append(parts, head)
		case "resource_link":
			parts = append(parts, fmt.Sprintf("[resource link %s %s]", b.URI, b.Name))
		default:
			parts = append(parts, fmt.Sprintf("[%s content]", orUnknown(b.Type)))
		}
	}
	if len(parts) == 0 && len(res.StructuredContent) > 0 {
		parts = append(parts, string(res.StructuredContent))
	}
	out := strings.Join(parts, "\n")
	if len(out) > maxResultChars {
		out = out[:maxResultChars] + "\n[truncated]"
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// errNotRunning is wrapped by every error from a dead connection.
var errNotRunning = errors.New("not running")

func notRunning(server string, cause error) error {
	if cause == nil {
		return fmt.Errorf("mcp server %s is %w", server, errNotRunning)
	}
	return fmt.Errorf("mcp server %s is %w (%v)", server, errNotRunning, cause)
}
