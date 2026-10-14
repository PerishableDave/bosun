package openai

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
		Model:  "gpt-x",
		System: "be brief",
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleUser, "weather?"),
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolUseBlock("c0", "wx", json.RawMessage(`{"c":"x"}`))}},
			{Role: llm.RoleTool, Content: []llm.ContentBlock{llm.ToolResultBlock("c0", "rain", false)}},
		},
		Tools:     []llm.ToolSpec{{Name: "get_weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens: 100,
	}
}

func mk(t *testing.T, url string) *Provider {
	t.Helper()
	p, err := New(Config{BaseURL: url, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNew_RequiresBaseURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error")
	}
}

func TestComplete_ToolCall(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"model":"m","usage":{"prompt_tokens":5,"completion_tokens":3},"choices":[{"finish_reason":"tool_calls",
		"message":{"content":"hi","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]}}]}`)
	}))
	defer srv.Close()
	resp, err := mk(t, srv.URL).Complete(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	tu := resp.Message.ToolUses()
	if resp.StopReason != llm.StopToolUse || resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 3 ||
		len(tu) != 1 || tu[0].ID != "call_1" || string(tu[0].Input) != `{"city":"Oslo"}` || resp.Message.Text() != "hi" {
		t.Fatalf("resp = %+v", resp)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages: %v", msgs)
	}
	asst := msgs[2].(map[string]any)
	tc := asst["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "c0" || tc["function"].(map[string]any)["arguments"] != `{"c":"x"}` {
		t.Fatalf("assistant mapping: %v", asst)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c0" || tool["content"] != "rain" {
		t.Fatalf("tool mapping: %v", tool)
	}
	if got["max_tokens"] == nil || got["tools"] == nil {
		t.Fatalf("request: %v", got)
	}
}

const sseBody = `data: {"model":"m","choices":[{"delta":{"role":"assistant","content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}

data: [DONE]

`

func TestStream_AssemblesTextAndToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody)
	}))
	defer srv.Close()
	ch, err := mk(t, srv.URL).Stream(context.Background(), req())
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
		case llm.EventDone:
			done = ev.Response
		case llm.EventError:
			t.Fatal(ev.Err)
		}
	}
	if text != "Hello" || args != `{"city":"Oslo"}` || done == nil || types[len(types)-1] != llm.EventDone {
		t.Fatalf("text=%q args=%q done=%v types=%v", text, args, done, types)
	}
	tu := done.Message.ToolUses()
	if done.StopReason != llm.StopToolUse || done.Usage.InputTokens != 9 || done.Usage.OutputTokens != 4 ||
		len(tu) != 1 || tu[0].ID != "call_7" || string(tu[0].Input) != `{"city":"Oslo"}` || done.Message.Text() != "Hello" {
		t.Fatalf("done = %+v", done)
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, strings.Repeat("y", 2000))
	}))
	defer srv.Close()
	p := mk(t, srv.URL)
	for name, f := range map[string]func() error{
		"complete": func() error { _, err := p.Complete(context.Background(), req()); return err },
		"stream":   func() error { _, err := p.Stream(context.Background(), req()); return err },
	} {
		err := f()
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != 500 || len(ae.Body) > 600 || !strings.Contains(err.Error(), "500") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestStream_ErrorOnTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
	}))
	defer srv.Close()
	ch, err := mk(t, srv.URL).Stream(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	var last llm.Event
	for ev := range ch {
		last = ev
	}
	if last.Type != llm.EventError {
		t.Fatalf("last = %+v", last)
	}
}

func TestStream_ContextCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := mk(t, srv.URL).Stream(ctx, req())
	if err != nil {
		t.Fatal(err)
	}
	if ev := <-ch; ev.Type != llm.EventTextDelta {
		t.Fatalf("first event %+v", ev)
	}
	cancel()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Type == llm.EventDone {
				t.Fatal("unexpected done after cancel")
			}
		case <-timeout:
			t.Fatal("channel not closed after cancel")
		}
	}
}
