package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a scripted Telegram Bot API server. Tests queue updates; the
// server hands them to getUpdates and records every other call.
type fakeAPI struct {
	t     *testing.T
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	updates  []Update
	nextUpd  int64
	nextMsg  int64
	calls    []apiCall
	wake     chan struct{}
	pollWait time.Duration
}

type apiCall struct {
	Method string
	Params map[string]any
	At     time.Time
	ID     int64 // message id assigned to a sendMessage
}

func (c apiCall) chatID() int64 {
	v, _ := c.Params["chat_id"].(float64)
	return int64(v)
}
func (c apiCall) text() string { s, _ := c.Params["text"].(string); return s }
func (c apiCall) msgID() int64 {
	v, _ := c.Params["message_id"].(float64)
	return int64(v)
}

// callbackData returns the inline keyboard's callback data, if any.
func (c apiCall) callbackData() []string {
	rm, _ := c.Params["reply_markup"].(map[string]any)
	rows, _ := rm["inline_keyboard"].([]any)
	var out []string
	for _, r := range rows {
		btns, _ := r.([]any)
		for _, b := range btns {
			m, _ := b.(map[string]any)
			if d, ok := m["callback_data"].(string); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

func newFakeAPI(t *testing.T, token string) *fakeAPI {
	f := &fakeAPI{t: t, token: token, nextUpd: 1, nextMsg: 1000, wake: make(chan struct{}, 1), pollWait: 100 * time.Millisecond}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) reply(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/bot")
	tok, method, _ := strings.Cut(path, "/")
	if tok != f.token {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	params := map[string]any{}
	_ = json.Unmarshal(body, &params)
	switch method {
	case "getMe":
		f.reply(w, User{ID: 999, IsBot: true, FirstName: "Bosun", Username: "bosun_test_bot"})
		return
	case "getUpdates":
		offset := int64(0)
		if v, ok := params["offset"].(float64); ok {
			offset = int64(v)
		}
		deadline := time.After(f.pollWait)
		for {
			f.mu.Lock()
			var out []Update
			for _, u := range f.updates {
				if u.UpdateID >= offset {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			if len(out) > 0 {
				f.reply(w, out)
				return
			}
			select {
			case <-f.wake:
			case <-deadline:
				f.reply(w, []Update{})
				return
			case <-r.Context().Done():
				return
			}
		}
	}
	f.mu.Lock()
	id := f.nextMsg
	f.nextMsg++
	f.calls = append(f.calls, apiCall{Method: method, Params: params, At: time.Now(), ID: id})
	f.mu.Unlock()
	switch method {
	case "sendMessage":
		f.reply(w, Message{MessageID: id, Chat: Chat{ID: apiCall{Params: params}.chatID(), Type: "private"}, Text: apiCall{Params: params}.text()})
	default:
		f.reply(w, true)
	}
}

func (f *fakeAPI) push(u Update) {
	f.mu.Lock()
	u.UpdateID = f.nextUpd
	f.nextUpd++
	f.updates = append(f.updates, u)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// send queues a private-chat text message from user chatID.
func (f *fakeAPI) send(chatID int64, username, text string) {
	f.push(Update{Message: &Message{From: &User{ID: chatID, FirstName: "U", Username: username},
		Chat: Chat{ID: chatID, Type: "private"}, Text: text, Date: time.Now().Unix()}})
}

func (f *fakeAPI) press(chatID, msgID int64, data string) {
	f.push(Update{CallbackQuery: &CallbackQuery{ID: fmt.Sprintf("cb%d", msgID), From: User{ID: chatID},
		Message: &Message{MessageID: msgID, Chat: Chat{ID: chatID, Type: "private"}, Text: "x"}, Data: data}})
}

func (f *fakeAPI) all() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiCall(nil), f.calls...)
}

// callsTo returns the calls of method to chatID (0 = any chat).
func (f *fakeAPI) callsTo(method string, chatID int64) []apiCall {
	var out []apiCall
	for _, c := range f.all() {
		if c.Method == method && (chatID == 0 || c.chatID() == chatID) {
			out = append(out, c)
		}
	}
	return out
}

// sentTexts returns the texts of messages sent to chatID.
func (f *fakeAPI) sentTexts(chatID int64) []string {
	var out []string
	for _, c := range f.callsTo("sendMessage", chatID) {
		out = append(out, c.text())
	}
	return out
}

// waitFor polls cond until it holds or 5s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitSent waits for a message to chatID containing substr and returns it.
func (f *fakeAPI) waitSent(t *testing.T, chatID int64, substr string) apiCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, c := range f.callsTo("sendMessage", chatID) {
			if strings.Contains(c.text(), substr) {
				return c
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a message to %d containing %q; sent: %q", chatID, substr, f.sentTexts(chatID))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
