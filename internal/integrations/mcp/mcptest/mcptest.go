// Package mcptest is a tiny fake MCP server for tests. It speaks JSON-RPC 2.0
// over stdio (Main, used by re-executing a test binary) or streamable HTTP
// (Handler), and serves a fixed set of tools that exercise the bridge:
//
//	echo          readOnlyHint      returns arguments.message
//	Create Issue! (no annotations)  returns "created <title>"
//	delete_all    destructiveHint   returns "deleted"
//	fail          readOnlyHint      isError result "boom"
//	env           readOnlyHint      returns the sorted environment, one per line
//	mixed         readOnlyHint      text, image and resource blocks
//	sleep         readOnlyHint      blocks until the request is cancelled
//	crash         (stdio only)      exits the process with status 3
//
// tools/list pages two tools at a time using a numeric cursor.
//
// It is not imported by production code.
package mcptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// EnvServe is the environment variable that makes a test binary act as the
// fake server: TestMain calls Main when it is set to "1".
const EnvServe = "BOSUN_MCPTEST_SERVE"

// EnvStubborn, when "1", makes Main ignore SIGTERM and stdin EOF so tests can
// check that Close escalates to SIGKILL.
const EnvStubborn = "BOSUN_MCPTEST_STUBBORN"

// EnvFailInit, when "1", makes initialize return a JSON-RPC error.
const EnvFailInit = "BOSUN_MCPTEST_FAIL_INIT"

// ProtocolVersion is what the fake answers to initialize.
const ProtocolVersion = "2025-06-18"

// PageSize is the number of tools per tools/list page.
const PageSize = 2

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations map[string]any  `json:"annotations,omitempty"`
}

var ro = map[string]any{"readOnlyHint": true, "title": "ro"}

// Tools is the fixed tool list.
var tools = []tool{
	{Name: "echo", Description: "Echo a message", Annotations: ro,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`)},
	{Name: "Create Issue!", Description: "Create an issue",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)},
	{Name: "delete_all", Description: "Delete everything", Annotations: map[string]any{"destructiveHint": true}},
	{Name: "fail", Description: "Always fails", Annotations: ro},
	{Name: "env", Description: "Dump environment", Annotations: ro},
	{Name: "mixed", Description: "Mixed content", Annotations: ro},
	{Name: "sleep", Description: "Sleep until cancelled", Annotations: ro},
	{Name: "crash", Description: "Exit the server", Annotations: ro},
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func textResult(s string, isErr bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isErr}
}

// server holds state shared by both transports.
type server struct {
	failInit bool
	exit     func(code int) // crash tool; nil over HTTP

	mu        sync.Mutex
	cancelled map[string]chan struct{} // request id -> closed on notifications/cancelled
}

func newServer() *server { return &server{cancelled: map[string]chan struct{}{}} }

// handle processes one request and returns its result or error. done is
// closed if the client cancels the request.
func (s *server) handle(method string, params json.RawMessage, done <-chan struct{}) (any, *rpcError) {
	switch method {
	case "initialize":
		if s.failInit {
			return nil, &rpcError{Code: -32603, Message: "init refused"}
		}
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}, "extra": true},
			"serverInfo":      map[string]any{"name": "mcptest", "version": "1.0.0", "client_asked": p.ProtocolVersion},
			"instructions":    "fake server",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		json.Unmarshal(params, &p)
		start := 0
		if p.Cursor != "" {
			n, err := strconv.Atoi(p.Cursor)
			if err != nil || n < 0 || n > len(tools) {
				return nil, &rpcError{Code: -32602, Message: "bad cursor"}
			}
			start = n
		}
		end := min(start+PageSize, len(tools))
		res := map[string]any{"tools": tools[start:end]}
		if end < len(tools) {
			res["nextCursor"] = strconv.Itoa(end)
		}
		return res, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		switch p.Name {
		case "echo":
			return textResult(fmt.Sprint(p.Arguments["message"]), false), nil
		case "Create Issue!":
			return textResult("created "+fmt.Sprint(p.Arguments["title"]), false), nil
		case "delete_all":
			return textResult("deleted", false), nil
		case "fail":
			return textResult("boom", true), nil
		case "env":
			env := os.Environ()
			sort.Strings(env)
			return textResult(strings.Join(env, "\n"), false), nil
		case "mixed":
			return map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"},
				map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///a.txt", "mimeType": "text/plain", "text": "inside"}},
				map[string]any{"type": "resource_link", "uri": "file:///b.bin", "name": "b"},
				map[string]any{"type": "future_kind"},
			}}, nil
		case "sleep":
			select {
			case <-done:
			case <-time.After(time.Minute):
			}
			return textResult("woke", false), nil
		case "crash":
			if s.exit != nil {
				s.exit(3)
			}
			return nil, &rpcError{Code: -32603, Message: "cannot crash over http"}
		}
		return nil, &rpcError{Code: -32602, Message: "unknown tool " + p.Name}
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
}

func (s *server) track(id json.RawMessage) chan struct{} {
	ch := make(chan struct{})
	s.mu.Lock()
	s.cancelled[string(id)] = ch
	s.mu.Unlock()
	return ch
}

func (s *server) untrack(id json.RawMessage) {
	s.mu.Lock()
	delete(s.cancelled, string(id))
	s.mu.Unlock()
}

func (s *server) cancel(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	json.Unmarshal(params, &p)
	s.mu.Lock()
	if ch, ok := s.cancelled[string(p.RequestID)]; ok {
		close(ch)
		delete(s.cancelled, string(p.RequestID))
	}
	s.mu.Unlock()
}

// Main runs the fake server on stdin/stdout and exits. Call it from TestMain
// when os.Getenv(EnvServe) == "1".
func Main() {
	s := newServer()
	s.failInit = os.Getenv(EnvFailInit) == "1"
	s.exit = os.Exit
	stubborn := os.Getenv(EnvStubborn) == "1"
	if stubborn {
		signal.Ignore(syscall.SIGTERM)
	}
	fmt.Fprintln(os.Stderr, "mcptest: starting")
	serveStdio(s, os.Stdin, os.Stdout)
	if stubborn {
		time.Sleep(time.Hour) // wait for SIGKILL
	}
	os.Exit(0)
}

func serveStdio(s *server, r io.Reader, w io.Writer) {
	var wmu sync.Mutex
	write := func(m message) {
		m.JSONRPC = "2.0"
		b, _ := json.Marshal(m)
		wmu.Lock()
		w.Write(append(b, '\n'))
		wmu.Unlock()
	}
	// A server-initiated request the client must answer (or reject).
	write(message{ID: json.RawMessage(`"srv-1"`), Method: "ping"})
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var wg sync.WaitGroup
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			write(message{ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		switch {
		case m.Method == "notifications/cancelled":
			s.cancel(m.Params)
		case m.Method != "" && len(m.ID) == 0:
			// other notifications (notifications/initialized): ignore
		case m.Method != "":
			done := s.track(m.ID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer s.untrack(m.ID)
				res, rerr := s.handle(m.Method, m.Params, done)
				if m.Method == "tools/list" {
					// An unsolicited notification in the middle of traffic.
					write(message{Method: "notifications/message", Params: json.RawMessage(`{"level":"info","data":"listing"}`)})
				}
				write(message{ID: m.ID, Result: res, Error: rerr})
			}()
		default:
			// a response to our ping; ignore
		}
	}
	wg.Wait()
}

// HTTPOptions configure Handler.
type HTTPOptions struct {
	// SSE answers requests with a text/event-stream body instead of JSON.
	SSE bool
	// Token, if set, is the required bearer token.
	Token string
	// SeenAuth, if set, receives every Authorization header value.
	SeenAuth func(string)
}

// SessionID is the Mcp-Session-Id the HTTP handler issues.
const SessionID = "sess-123"

// Handler returns a streamable-HTTP fake MCP server.
func Handler(o HTTPOptions) http.Handler {
	s := newServer()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.SeenAuth != nil {
			o.SeenAuth(r.Header.Get("Authorization"))
		}
		if o.Token != "" && r.Header.Get("Authorization") != "Bearer "+o.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") || !strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Error(w, "bad accept", http.StatusNotAcceptable)
			return
		}
		var m message
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if m.Method != "initialize" {
			if r.Header.Get("Mcp-Session-Id") != SessionID {
				http.Error(w, "missing session", http.StatusBadRequest)
				return
			}
			if r.Header.Get("MCP-Protocol-Version") != ProtocolVersion {
				http.Error(w, "missing protocol version", http.StatusBadRequest)
				return
			}
		}
		if len(m.ID) == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		res, rerr := s.handle(m.Method, m.Params, r.Context().Done())
		resp := message{JSONRPC: "2.0", ID: m.ID, Result: res, Error: rerr}
		if m.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", SessionID)
		}
		b, _ := json.Marshal(resp)
		if !o.SSE {
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// A notification and a server request first, then the response,
		// split over two data lines (between tokens; SSE joins them with a
		// newline) to exercise multi-line events.
		fmt.Fprint(w, ": comment\n\n")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"srv-9\",\"method\":\"ping\"}\n\n")
		half := strings.Index(string(b), `,"id":`) + 1
		fmt.Fprintf(w, "id: 1\nevent: message\ndata: %s\ndata: %s\n\n", b[:half], b[half:])
	})
}
