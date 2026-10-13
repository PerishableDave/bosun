package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxHTTPBody bounds one HTTP response from an MCP server.
const maxHTTPBody = 16 << 20

// httpTransport implements the MCP streamable HTTP transport: every message
// is a POST; the server answers with application/json or a text/event-stream
// that eventually carries the response. The optional standalone GET stream
// for server-initiated messages is not used.
type httpTransport struct {
	name   string
	url    string
	client *http.Client
	log    *slog.Logger
	// token returns the bearer token to send; nil means no auth.
	token func(ctx context.Context) (string, error)

	nextID atomic.Int64
	mu     sync.Mutex
	sessID string
	proto  string
	closed bool
}

func newHTTPTransport(name, url string, token func(context.Context) (string, error), log *slog.Logger) *httpTransport {
	// No client-wide timeout: SSE responses can legitimately take as long as
	// the tool runs. Every request is bounded by its context instead.
	return &httpTransport{name: name, url: url, client: &http.Client{}, token: token, log: log}
}

func (t *httpTransport) setProtocolVersion(v string) {
	t.mu.Lock()
	t.proto = v
	t.mu.Unlock()
}

func (t *httpTransport) err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("mcp server %s is %w (closed)", t.name, errNotRunning)
	}
	return nil
}

func (t *httpTransport) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	if t.sessID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessID)
	}
	if t.proto != "" {
		req.Header.Set("MCP-Protocol-Version", t.proto)
	}
	t.mu.Unlock()
	if t.token != nil {
		tok, err := t.token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req, nil
}

// post sends msg and, if wantID > 0, returns the matching response.
func (t *httpTransport) post(ctx context.Context, msg any, wantID int64) (rpcMessage, error) {
	if err := t.err(); err != nil {
		return rpcMessage{}, err
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return rpcMessage{}, err
	}
	req, err := t.newRequest(ctx, http.MethodPost, body)
	if err != nil {
		return rpcMessage{}, fmt.Errorf("mcp server %s: %w", t.name, err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return rpcMessage{}, fmt.Errorf("mcp server %s: %w", t.name, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessID = sid
		t.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// TODO: on 404 with a session id the server has dropped the session;
		// the spec says to re-initialize. Out of scope for v1.
		return rpcMessage{}, fmt.Errorf("mcp server %s: HTTP %d: %s", t.name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if wantID == 0 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPBody))
		return rpcMessage{}, nil
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body2 := io.LimitReader(resp.Body, maxHTTPBody)
	switch ct {
	case "text/event-stream":
		return t.readSSE(ctx, body2, wantID)
	case "application/json", "":
		raw, err := io.ReadAll(body2)
		if err != nil {
			return rpcMessage{}, fmt.Errorf("mcp server %s: read response: %w", t.name, err)
		}
		return t.matchJSON(raw, wantID)
	default:
		return rpcMessage{}, fmt.Errorf("mcp server %s: unexpected content type %q", t.name, ct)
	}
}

// matchJSON finds the response to wantID in a single message or a batch.
func (t *httpTransport) matchJSON(raw []byte, wantID int64) (rpcMessage, error) {
	raw = bytes.TrimSpace(raw)
	var msgs []rpcMessage
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return rpcMessage{}, fmt.Errorf("mcp server %s: decode response: %w", t.name, err)
		}
	} else {
		var m rpcMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return rpcMessage{}, fmt.Errorf("mcp server %s: decode response: %w", t.name, err)
		}
		msgs = []rpcMessage{m}
	}
	for _, m := range msgs {
		if id, ok := m.numericID(); ok && m.isResponse() && id == wantID {
			return m, nil
		}
	}
	return rpcMessage{}, fmt.Errorf("mcp server %s: response did not contain id %d", t.name, wantID)
}

// readSSE reads events until the response to wantID arrives. Other messages
// (progress notifications, server requests) are logged and skipped: answering
// server requests needs the GET stream, which this client does not open.
func (t *httpTransport) readSSE(ctx context.Context, r io.Reader, wantID int64) (rpcMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxHTTPBody)
	var data strings.Builder
	dispatch := func() (rpcMessage, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return rpcMessage{}, false
		}
		var m rpcMessage
		if err := json.Unmarshal([]byte(data.String()), &m); err != nil {
			t.log.Debug("mcp: ignoring undecodable SSE event", "server", t.name, "err", err)
			return rpcMessage{}, false
		}
		if id, ok := m.numericID(); ok && m.isResponse() && id == wantID {
			return m, true
		}
		t.log.Debug("mcp: skipping SSE message", "server", t.name, "method", m.Method)
		return rpcMessage{}, false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m, ok := dispatch(); ok {
				return m, nil
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(v)
		default:
			// event:, id:, retry: are not needed
		}
	}
	if m, ok := dispatch(); ok {
		return m, nil
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return rpcMessage{}, fmt.Errorf("mcp server %s: %w", t.name, ctx.Err())
		}
		return rpcMessage{}, fmt.Errorf("mcp server %s: read event stream: %w", t.name, err)
	}
	// TODO: resumable streams (Last-Event-ID) are not supported.
	return rpcMessage{}, fmt.Errorf("mcp server %s: event stream ended without a response", t.name)
}

func (t *httpTransport) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	m, err := t.post(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}, id)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
			return nil, fmt.Errorf("mcp server %s: %s: %w", t.name, method, ctx.Err())
		}
		return nil, err
	}
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Result, nil
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	_, err := t.post(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params}, 0)
	return err
}

// close ends the session (best effort DELETE) and releases connections.
func (t *httpTransport) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	sid := t.sessID
	t.mu.Unlock()
	if sid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if req, err := t.newRequest(ctx, http.MethodDelete, nil); err == nil {
			if resp, err := t.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		cancel()
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.client.CloseIdleConnections()
	return nil
}
