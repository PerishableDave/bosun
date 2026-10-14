package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
)

// tgFake is a minimal Telegram Bot API: queued updates go out through
// getUpdates, everything else is recorded.
type tgFake struct {
	token string
	mu    sync.Mutex
	queue []map[string]any
	next  int64
	msgID int64
	calls []tgCall
	gotMe bool
}

type tgCall struct {
	method string
	params map[string]any
	id     int64
}

func (c tgCall) text() string { s, _ := c.params["text"].(string); return s }
func (c tgCall) chat() int64  { f, _ := c.params["chat_id"].(float64); return int64(f) }

func (f *tgFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
	w.Header().Set("Content-Type", "application/json")
	if tok != f.token {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	var params map[string]any
	json.NewDecoder(r.Body).Decode(&params)
	ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
	switch method {
	case "getMe":
		f.mu.Lock()
		f.gotMe = true
		f.mu.Unlock()
		ok(map[string]any{"id": 1, "is_bot": true, "first_name": "Bosun", "username": "bosun_e2e_bot"})
		return
	case "getUpdates":
		off, _ := params["offset"].(float64)
		deadline := time.Now().Add(100 * time.Millisecond)
		for {
			f.mu.Lock()
			var out []map[string]any
			for _, u := range f.queue {
				if u["update_id"].(int64) >= int64(off) {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			if len(out) > 0 || time.Now().After(deadline) || r.Context().Err() != nil {
				if out == nil {
					out = []map[string]any{}
				}
				ok(out)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	f.mu.Lock()
	f.msgID++
	id := f.msgID
	f.calls = append(f.calls, tgCall{method, params, id})
	f.mu.Unlock()
	if method == "sendMessage" {
		ok(map[string]any{"message_id": id, "chat": map[string]any{"id": params["chat_id"], "type": "private"}, "date": 0})
		return
	}
	ok(true)
}

func (f *tgFake) push(u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	u["update_id"] = f.next
	f.queue = append(f.queue, u)
}

func (f *tgFake) say(chat int64, text string) {
	f.push(map[string]any{"message": map[string]any{"message_id": 1, "date": 0, "text": text,
		"from": map[string]any{"id": chat, "is_bot": false, "first_name": "A", "username": "alice_tg"},
		"chat": map[string]any{"id": chat, "type": "private"}}})
}

func (f *tgFake) press(chat, msgID int64, data string) {
	f.push(map[string]any{"callback_query": map[string]any{"id": "cb1", "data": data,
		"from":    map[string]any{"id": chat, "is_bot": false, "first_name": "A"},
		"message": map[string]any{"message_id": msgID, "date": 0, "text": "x", "chat": map[string]any{"id": chat, "type": "private"}}}})
}

func (f *tgFake) find(t *testing.T, method string, chat int64, substr string) tgCall {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		var texts []string
		for _, c := range f.calls {
			if c.method == method && c.chat() == chat {
				if strings.Contains(c.text(), substr) {
					f.mu.Unlock()
					return c
				}
				texts = append(texts, c.text())
			}
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no %s to %d containing %q; got %q", method, chat, substr, texts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *tgFake) count(method string, chat int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method && c.chat() == chat {
			n++
		}
	}
	return n
}

// TestTelegramEndToEnd drives the whole daemon through a fake Bot API: the
// admin stores the bot token through /v1/credentials, alice links her chat
// with a code from /v1/channels, chats, approves a tool call with the
// inline keyboard, and receives a routine's result.
func TestTelegramEndToEnd(t *testing.T) {
	const token = "4242:e2e-secret"
	tg := &tgFake{token: token}
	tgSrv := httptest.NewServer(tg)
	defer tgSrv.Close()

	dir := t.TempDir()
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm: { default: fake/m, providers: { fake: {} } }
memory: { enabled: false }
policy:
  defaults: { read: allow, write: ask, destructive: deny }
channels:
  telegram: { enabled: true, api_url: %q }
`, filepath.Join(dir, "data"), tgSrv.URL))
	if err != nil {
		t.Fatal(err)
	}
	model := fake.New("fake")
	reg := llm.NewRegistry()
	if err := reg.Register(model); err != nil {
		t.Fatal(err)
	}
	st := &stub{}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := newDaemon(ctx, cfg, log, deps{LLM: reg, Argon2: cheapArgon(), Integrations: []integration.Integration{st},
		ChannelTokenRetry: 20 * time.Millisecond})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		d.Close()
	}()
	h := &harness{t: t, d: d, base: "http://" + ln.Addr().String()}
	if _, err := d.auth.CreateUser(ctx, "alice", "alice-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	alice := h.login("alice", "alice-password")

	// Without a token the channel reports itself down.
	var il api.IntegrationListResponse
	h.do("GET", "/v1/integrations", alice, nil, &il)
	found := false
	for _, in := range il.Integrations {
		if in.Name == "channels" {
			found = true
			if in.Status != integration.StatusDown || !strings.Contains(in.Message, "no bot token") {
				t.Fatalf("channels status = %+v", in)
			}
		}
	}
	if !found {
		t.Fatalf("channels integration not listed: %+v", il)
	}
	if c := h.do("PUT", "/v1/credentials/channels/telegram_bot_token?scope=shared", alice, api.PutCredentialRequest{Secret: token}, nil); c != 200 {
		t.Fatalf("PUT token = %d", c)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		tg.mu.Lock()
		ok := tg.gotMe
		tg.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bot never called getMe")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// An unlinked chat gets instructions, and the model is never called.
	tg.say(5000, "hello, who are you?")
	tg.find(t, "sendMessage", 5000, "bosun channels link telegram")
	if n := len(model.Requests()); n != 0 {
		t.Fatalf("model called %d times for an unlinked chat", n)
	}

	var lc api.ChannelLinkCodeResponse
	if c := h.do("POST", "/v1/channels/telegram/link", alice, nil, &lc); c != 201 || !strings.Contains(lc.Instructions, "@bosun_e2e_bot") {
		t.Fatalf("link code = %d %+v", c, lc)
	}
	tg.say(4242, "/link "+lc.Code)
	tg.find(t, "sendMessage", 4242, "Linked to Bosun user alice")

	// A turn that needs approval: the keyboard resolves it.
	model.Push(fake.ToolCall("w1", "stub.write", `{"note":"buy milk"}`), fake.Text("Saved your note."))
	tg.say(4242, "remember to buy milk")
	appr := tg.find(t, "sendMessage", 4242, "Approval needed: stub.write")
	kb, _ := json.Marshal(appr.params["reply_markup"])
	var markup struct {
		InlineKeyboard [][]struct {
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	json.Unmarshal(kb, &markup)
	if len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 2 {
		t.Fatalf("keyboard = %s", kb)
	}
	tg.find(t, "sendMessage", 4242, "⚙ stub.write")
	tg.press(4242, appr.id, markup.InlineKeyboard[0][0].CallbackData)
	tg.find(t, "sendMessage", 4242, "Saved your note.")
	tg.find(t, "editMessageText", 4242, "Approved by you")
	if n := st.writes.Load(); n != 1 {
		t.Fatalf("stub.write ran %d times", n)
	}

	// The conversation is a normal Bosun conversation, visible in the API.
	var cl api.ChannelListResponse
	if c := h.do("GET", "/v1/channels", alice, nil, &cl); c != 200 || len(cl.Links) != 1 || cl.Links[0].ConversationID == "" {
		t.Fatalf("channels = %d %+v", c, cl)
	}

	// A routine's result is delivered to the chat.
	var rt api.RoutineResponse
	if c := h.do("POST", "/v1/routines", alice, api.CreateRoutineRequest{Name: "brief", Prompt: "Brief me.",
		Trigger: api.RoutineTrigger{Cron: "0 7 * * *"}}, &rt); c != 201 {
		t.Fatalf("create routine = %d", c)
	}
	model.Push(fake.Text("All clear today."))
	if c := h.do("POST", fmt.Sprintf("/v1/routines/%d/run", rt.ID), alice, nil, nil); c != 202 {
		t.Fatalf("run routine = %d", c)
	}
	msg := tg.find(t, "sendMessage", 4242, "Routine brief")
	if !strings.Contains(msg.text(), "All clear today.") {
		t.Fatalf("routine message = %q", msg.text())
	}

	// Unlinking through the API cuts the chat off.
	if c := h.do("DELETE", "/v1/channels/telegram/4242", alice, nil, nil); c != 204 {
		t.Fatalf("unlink = %d", c)
	}
	before := len(model.Requests())
	tg.say(4242, "are you there?")
	tg.find(t, "sendMessage", 4242, "private Bosun assistant")
	if n := len(model.Requests()); n != before {
		t.Fatal("model called after unlink")
	}
	if n := tg.count("sendMessage", 5000); n != 1 {
		t.Fatalf("unlinked chat got %d messages", n)
	}
}
