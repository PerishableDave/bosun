package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/llm"
)

func req() llm.Request {
	return llm.Request{
		Model:  "claude-sonnet-5-5",
		System: "be brief",
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleUser, "weather?"),
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolUseBlock("tu_0", "wx", json.RawMessage(`{"c":"x"}`))}},
			{Role: llm.RoleTool, Content: []llm.ContentBlock{llm.ToolResultBlock("tu_0", "rain", true)}},
		},
		Tools: []llm.ToolSpec{{Name: "get_weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
}

func TestComplete_ToolUse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" || r.URL.Path != "/v1/messages" {
			t.Errorf("bad headers/path: %v %s", r.Header, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"model":"m","stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7},
		"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Oslo"}}]}`)
	}))
	defer srv.Close()
	p := New(Config{APIKey: "k", BaseURL: srv.URL})
	resp, err := p.Complete(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopToolUse || resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("resp = %+v", resp)
	}
	tu := resp.Message.ToolUses()
	if len(tu) != 1 || tu[0].ID != "tu_1" || string(tu[0].Input) != `{"city":"Oslo"}` || resp.Message.Text() != "hi" {
		t.Fatalf("message = %+v", resp.Message)
	}
	// Request mapping: tool message -> user role with tool_result.
	msgs := got["messages"].([]any)
	last := msgs[2].(map[string]any)
	blk := last["content"].([]any)[0].(map[string]any)
	if last["role"] != "user" || blk["type"] != "tool_result" || blk["tool_use_id"] != "tu_0" || blk["is_error"] != true {
		t.Fatalf("tool result mapping: %v", last)
	}
	if got["system"] != "be brief" || got["max_tokens"] == nil {
		t.Fatalf("request: %v", got)
	}
}

const sseBody = `event: message_start
data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":20,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_9","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Oslo\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStream_AssemblesTextAndToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody)
	}))
	defer srv.Close()
	ch, err := New(Config{BaseURL: srv.URL}).Stream(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	var types []llm.EventType
	var text, args string
	var done *llm.Response
	for ev := range ch {
		types = append(types, ev.Type)
		switch ev.Type {
		case llm.EventTextDelta:
			text += ev.Text
		case llm.EventToolUseDelta:
			args += ev.InputDelta
			if ev.Index != 1 {
				t.Errorf("tool index %d", ev.Index)
			}
		case llm.EventDone:
			done = ev.Response
		case llm.EventError:
			t.Fatal(ev.Err)
		}
	}
	if text != "Hello" || args != `{"city":"Oslo"}` || done == nil {
		t.Fatalf("text=%q args=%q done=%v types=%v", text, args, done, types)
	}
	if types[len(types)-1] != llm.EventDone {
		t.Fatalf("last event %v", types)
	}
	if done.StopReason != llm.StopToolUse || done.Usage.InputTokens != 20 || done.Usage.OutputTokens != 15 {
		t.Fatalf("done = %+v", done)
	}
	if done.Message.Text() != "Hello" || len(done.Message.ToolUses()) != 1 || string(done.Message.ToolUses()[0].Input) != `{"city":"Oslo"}` {
		t.Fatalf("message = %+v", done.Message)
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, strings.Repeat("x", 2000))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL})
	for name, f := range map[string]func() error{
		"complete": func() error { _, err := p.Complete(context.Background(), req()); return err },
		"stream":   func() error { _, err := p.Stream(context.Background(), req()); return err },
	} {
		err := f()
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != 429 || len(ae.Body) > 600 || !strings.Contains(err.Error(), "429") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestStream_ErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer srv.Close()
	ch, err := New(Config{BaseURL: srv.URL}).Stream(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for ev := range ch {
		n++
		if ev.Type != llm.EventError || ev.Err == nil || !strings.Contains(ev.Err.Error(), "Overloaded") {
			t.Fatalf("event %+v", ev)
		}
	}
	if n != 1 {
		t.Fatalf("events = %d", n)
	}
}

func TestStream_ContextCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{}}}\n\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := New(Config{BaseURL: srv.URL}).Stream(ctx, req())
	if err != nil {
		t.Fatal(err)
	}
	ev := <-ch
	if ev.Type != llm.EventTextDelta {
		t.Fatalf("first event %+v", ev)
	}
	cancel()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // closed without a terminal done event
			}
			if ev.Type == llm.EventDone {
				t.Fatal("unexpected done after cancel")
			}
		case <-timeout:
			t.Fatal("channel not closed after cancel")
		}
	}
}
