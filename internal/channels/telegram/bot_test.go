package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/channels"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

const testToken = "123456:TEST-token"

type runCall struct {
	userID     int64
	conv, text string
}

// fakeRunner stands in for the agent: it records turns and runs fn, which
// publishes events the way agent.Run does.
type fakeRunner struct {
	mu   sync.Mutex
	runs []runCall
	fn   func(ctx context.Context, userID int64, conv, text string) error
}

func (r *fakeRunner) Run(ctx context.Context, userID int64, conv, text string) error {
	r.mu.Lock()
	r.runs = append(r.runs, runCall{userID, conv, text})
	fn := r.fn
	r.mu.Unlock()
	return fn(ctx, userID, conv, text)
}

func (r *fakeRunner) calls() []runCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runCall(nil), r.runs...)
}

type env struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	bus    *events.Bus
	appr   *approval.StoreManager
	linker *channels.Linker
	api    *fakeAPI
	bot    *Bot
	runner *fakeRunner
	alice  *store.User
	bob    *store.User
}

func newEnv(t *testing.T, mutate func(*Options)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "bosun.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, st: st, bus: events.NewBus(), api: newFakeAPI(t, testToken)}
	e.alice, _ = st.CreateUser(ctx, "alice", "h", "member")
	e.bob, _ = st.CreateUser(ctx, "bob", "h", "member")
	e.appr = approval.NewManager(st, e.bus, approval.Options{Timeout: time.Minute})
	e.linker = channels.NewLinker(st, channels.LinkerOptions{})
	e.runner = &fakeRunner{fn: func(ctx context.Context, userID int64, conv, text string) error {
		e.bus.Publish(userID, events.Event{Type: events.TypeText, Data: events.TextData{Delta: "echo: " + text, ConversationID: conv}})
		e.bus.Publish(userID, events.Event{Type: events.TypeDone, Data: events.DoneData{ConversationID: conv}})
		return nil
	}}
	o := Options{
		Token:        func(context.Context) (string, error) { return testToken, nil },
		Linker:       e.linker,
		Bus:          e.bus,
		Runner:       e.runner,
		Store:        st,
		Approvals:    e.appr,
		APIURL:       e.api.srv.URL,
		PollTimeout:  time.Second,
		EditInterval: 50 * time.Millisecond,
		TokenRetry:   20 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&o)
	}
	if e.bot, err = New(o); err != nil {
		t.Fatal(err)
	}
	if err := e.bot.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.bot.Close() })
	waitFor(t, "bot polling", func() bool { return e.bot.Status().State == integration.StatusOK })
	return e
}

// link links chatID to user through the /link flow.
func (e *env) link(chatID int64, username string, user *store.User) {
	e.t.Helper()
	code, _, err := e.linker.NewCode(e.ctx, user.ID, Name)
	if err != nil {
		e.t.Fatal(err)
	}
	e.api.send(chatID, username, "/link "+code)
	e.api.waitSent(e.t, chatID, "Linked to Bosun user "+user.Username)
}

// barrier makes sure every update queued so far has been handled: updates
// are processed in order, so once a fresh unlinked chat got its reply, the
// earlier ones are done.
var barrierChat atomic.Int64

func (e *env) barrier() {
	e.t.Helper()
	id := 900000 + barrierChat.Add(1)
	e.api.send(id, "", "ping")
	e.api.waitSent(e.t, id, "private Bosun assistant")
}

func TestUnlinkedChatGetsHelpOnly(t *testing.T) {
	e := newEnv(t, nil)
	e.api.send(501, "stranger", "hello")
	e.api.waitSent(t, 501, "bosun channels link telegram")
	e.api.send(501, "stranger", "run my errands")
	e.api.send(501, "stranger", "/whoami")
	e.api.press(501, 1, "a:appr_x:y")
	e.barrier()
	if got := e.api.sentTexts(501); len(got) != 1 {
		t.Fatalf("unlinked chat got %d replies, want 1 (rate limited): %q", len(got), got)
	}
	if n := len(e.runner.calls()); n != 0 {
		t.Fatalf("agent ran %d times for an unlinked chat", n)
	}
	cb := e.api.callsTo("answerCallbackQuery", 0)
	if len(cb) != 1 || !strings.Contains(cb[0].text(), "not linked") {
		t.Fatalf("callback answers = %+v", cb)
	}
}

func TestLinkFlow(t *testing.T) {
	e := newEnv(t, nil)
	code, _, _ := e.linker.NewCode(e.ctx, e.alice.ID, Name)

	e.api.send(600, "alice_tg", "/link")
	e.api.waitSent(t, 600, "Usage: /link CODE")
	e.api.send(600, "alice_tg", "/link WRONG2")
	e.api.waitSent(t, 600, "invalid or expired")
	e.api.send(600, "alice_tg", "/link "+strings.ToLower(code))
	e.api.waitSent(t, 600, "Linked to Bosun user alice")
	l, err := e.st.GetChannelLink(e.ctx, Name, "600")
	if err != nil || l.UserID != e.alice.ID || !strings.Contains(l.DisplayName, "@alice_tg") {
		t.Fatalf("link = %+v, %v", l, err)
	}
	// The code is single-use.
	e.api.send(601, "mallory", "/link "+code)
	e.api.waitSent(t, 601, "invalid or expired")
	if _, err := e.st.GetChannelLink(e.ctx, Name, "601"); err == nil {
		t.Fatal("code reused")
	}

	e.api.send(600, "alice_tg", "/whoami")
	e.api.waitSent(t, 600, "You are alice on Bosun")
	e.api.send(600, "alice_tg", "/link ABCDEF")
	e.api.waitSent(t, 600, "already linked")

	// A message in the linked chat whose sender is someone else is dropped.
	e.api.push(Update{Message: &Message{From: &User{ID: 777}, Chat: Chat{ID: 600, Type: "private"}, Text: "hi"}})
	e.barrier()
	if n := len(e.runner.calls()); n != 0 {
		t.Fatalf("agent ran for a mismatched sender")
	}

	// /unlink removes the link; the chat is a stranger again.
	e.api.send(600, "alice_tg", "/unlink")
	e.api.waitSent(t, 600, "Unlinked")
	if _, err := e.st.GetChannelLink(e.ctx, Name, "600"); err == nil {
		t.Fatal("still linked after /unlink")
	}
	e.api.send(600, "alice_tg", "hello?")
	e.api.waitSent(t, 600, "private Bosun assistant")
	if n := len(e.runner.calls()); n != 0 {
		t.Fatalf("agent ran after unlink")
	}
}

func TestChatTurnStreamsEditsAndFinalMessage(t *testing.T) {
	e := newEnv(t, nil)
	e.link(700, "alice_tg", e.alice)
	pub := func(userID int64, typ string, data any) { e.bus.Publish(userID, events.Event{Type: typ, Data: data}) }
	e.runner.fn = func(ctx context.Context, userID int64, conv, text string) error {
		for _, chunk := range []string{"Hello", ", world", "! Run `ls` now."} {
			pub(userID, events.TypeText, events.TextData{Delta: chunk, ConversationID: conv})
			time.Sleep(120 * time.Millisecond)
		}
		pub(userID, events.TypeToolCall, events.ToolCallData{ID: "t1", Name: "notes.read", Args: json.RawMessage(`{}`), ConversationID: conv})
		pub(userID, events.TypeText, events.TextData{Delta: "Done reading.", ConversationID: conv})
		// Events of other conversations (e.g. a web UI turn) are not mirrored.
		pub(userID, events.TypeText, events.TextData{Delta: "OTHER", ConversationID: "conv_other"})
		pub(userID, events.TypeDone, events.DoneData{ConversationID: conv})
		return nil
	}
	e.api.send(700, "alice_tg", "hi there")
	e.api.waitSent(t, 700, "Done reading.")

	first := e.api.waitSent(t, 700, "Hello")
	if first.text() != "Hello" {
		t.Fatalf("first streamed message = %q, want the first chunk", first.text())
	}
	var edits []apiCall
	for _, c := range e.api.callsTo("editMessageText", 700) {
		if c.msgID() == first.ID {
			edits = append(edits, c)
		}
	}
	if len(edits) < 2 {
		t.Fatalf("want streamed edits, got %d: %+v", len(edits), edits)
	}
	final := edits[len(edits)-1]
	if final.text() != "Hello, world! Run ls now." {
		t.Fatalf("final text = %q", final.text())
	}
	ents, _ := json.Marshal(final.Params["entities"])
	if !strings.Contains(string(ents), `"type":"code"`) || !strings.Contains(string(ents), `"offset":18`) {
		t.Fatalf("final entities = %s", ents)
	}
	for i := 1; i < len(edits); i++ {
		if gap := edits[i].At.Sub(edits[i-1].At); gap < 40*time.Millisecond {
			t.Fatalf("edits %d and %d only %v apart (throttle is 50ms)", i-1, i, gap)
		}
	}
	tool := e.api.waitSent(t, 700, "⚙ notes.read")
	if ents, _ := json.Marshal(tool.Params["entities"]); !strings.Contains(string(ents), "italic") {
		t.Fatalf("tool line entities = %s", ents)
	}
	for _, s := range e.api.sentTexts(700) {
		if strings.Contains(s, "OTHER") {
			t.Fatal("text from another conversation was mirrored")
		}
	}

	// The turn ran in a lazily created "telegram" conversation, reused by
	// the next message; /new starts another.
	calls := e.runner.calls()
	conv, err := e.st.GetConversation(e.ctx, e.alice.ID, calls[0].conv)
	if err != nil || conv.Title != ConversationTitle || conv.Source != store.SourceTelegram || calls[0].text != "hi there" {
		t.Fatalf("conversation = %+v, %v (call %+v)", conv, err, calls[0])
	}
	e.runner.fn = func(ctx context.Context, userID int64, c, text string) error {
		pub(userID, events.TypeDone, events.DoneData{ConversationID: c})
		return nil
	}
	e.api.send(700, "alice_tg", "again")
	e.api.waitSent(t, 700, "(no reply)")
	e.api.send(700, "alice_tg", "/new")
	e.api.waitSent(t, 700, "Started a new conversation")
	e.api.send(700, "alice_tg", "third")
	waitFor(t, "third turn", func() bool { return len(e.runner.calls()) == 3 })
	calls = e.runner.calls()
	if calls[1].conv != calls[0].conv || calls[2].conv == calls[0].conv {
		t.Fatalf("conversations = %q %q %q", calls[0].conv, calls[1].conv, calls[2].conv)
	}
}

func TestOneTurnAtATime(t *testing.T) {
	e := newEnv(t, nil)
	e.link(710, "alice_tg", e.alice)
	release := make(chan struct{})
	e.runner.fn = func(ctx context.Context, userID int64, conv, text string) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		e.bus.Publish(userID, events.Event{Type: events.TypeText, Data: events.TextData{Delta: "finished " + text, ConversationID: conv}})
		e.bus.Publish(userID, events.Event{Type: events.TypeDone, Data: events.DoneData{ConversationID: conv}})
		return nil
	}
	e.api.send(710, "alice_tg", "first")
	waitFor(t, "first turn", func() bool { return len(e.runner.calls()) == 1 })
	e.api.send(710, "alice_tg", "second")
	e.api.waitSent(t, 710, "I will answer this after the current reply")
	e.api.send(710, "alice_tg", "third")
	e.api.send(710, "alice_tg", "fourth")
	e.api.send(710, "alice_tg", "fifth")
	e.api.waitSent(t, 710, "Still working on your earlier messages")
	if n := len(e.runner.calls()); n != 1 {
		t.Fatalf("runs = %d, want 1 while the first turn runs", n)
	}
	close(release)
	e.api.waitSent(t, 710, "finished fourth")
	var order []string
	for _, c := range e.runner.calls() {
		order = append(order, c.text)
	}
	if strings.Join(order, ",") != "first,second,third,fourth" {
		t.Fatalf("turn order = %v", order)
	}
	var replies []string
	for _, s := range e.api.sentTexts(710) {
		if strings.HasPrefix(s, "finished") {
			replies = append(replies, s)
		}
	}
	if strings.Join(replies, ",") != "finished first,finished second,finished third,finished fourth" {
		t.Fatalf("replies = %q", replies)
	}
}

func TestApprovalKeyboardRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	e.link(720, "alice_tg", e.alice)
	decided := make(chan approval.Decision, 1)
	e.runner.fn = func(ctx context.Context, userID int64, conv, text string) error {
		e.bus.Publish(userID, events.Event{Type: events.TypeText, Data: events.TextData{Delta: "Let me write that.", ConversationID: conv}})
		a, err := e.appr.Request(ctx, approval.Params{UserID: userID, ConversationID: conv, Tool: "notes.write",
			Capability: "notes.write", Risk: integration.RiskWrite, Args: json.RawMessage(`{"path":"todo.md"}`)})
		if err != nil {
			return err
		}
		d, err := e.appr.Wait(ctx, a.ID)
		decided <- d
		e.bus.Publish(userID, events.Event{Type: events.TypeText, Data: events.TextData{Delta: fmt.Sprintf("decision=%s err=%v", d, err), ConversationID: conv}})
		e.bus.Publish(userID, events.Event{Type: events.TypeDone, Data: events.DoneData{ConversationID: conv}})
		return nil
	}
	e.api.send(720, "alice_tg", "add milk to my todo")
	msg := e.api.waitSent(t, 720, "Approval needed: notes.write (write risk)")
	if !strings.Contains(msg.text(), `"path": "todo.md"`) {
		t.Fatalf("approval text = %q", msg.text())
	}
	data := msg.callbackData()
	if len(data) != 2 || !strings.HasSuffix(data[0], ":y") || !strings.HasSuffix(data[1], ":n") {
		t.Fatalf("keyboard = %v", data)
	}
	// The streamed text before the approval was finalized first.
	texts := e.api.sentTexts(720)
	if texts[len(texts)-2] != "Let me write that." {
		t.Fatalf("messages = %q", texts)
	}
	msgID := msg.ID
	e.api.press(720, msgID, data[0])
	select {
	case d := <-decided:
		if d != approval.Approve {
			t.Fatalf("decision = %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval not resolved")
	}
	e.api.waitSent(t, 720, "decision=approve err=<nil>")
	waitFor(t, "approval message updated", func() bool {
		for _, c := range e.api.callsTo("editMessageText", 720) {
			if c.msgID() == msgID && strings.HasSuffix(c.text(), "Approved by you") && c.Params["reply_markup"] == nil {
				return true
			}
		}
		return false
	})
	if a := e.api.callsTo("answerCallbackQuery", 0); len(a) != 1 || a[0].text() != "Approved" {
		t.Fatalf("callback answers = %+v", a)
	}
	// A second press is answered without changing anything.
	e.api.press(720, msgID, data[1])
	waitFor(t, "second answer", func() bool {
		a := e.api.callsTo("answerCallbackQuery", 0)
		return len(a) == 2 && a[1].text() == "Already answered."
	})
	// Bob cannot answer alice's approvals from his own linked chat.
	e.link(721, "bob_tg", e.bob)
	e.api.press(721, msgID, data[0])
	waitFor(t, "bob's answer", func() bool {
		a := e.api.callsTo("answerCallbackQuery", 0)
		return len(a) == 3 && a[2].text() == "Unknown approval."
	})
}

func TestApprovalsFromElsewhereAndRoutineDelivery(t *testing.T) {
	e := newEnv(t, nil)
	e.link(730, "alice_tg", e.alice)

	// An approval raised by a routine (no Telegram turn) is pushed with a
	// keyboard, and answering it in the CLI updates the message.
	a, err := e.appr.Request(e.ctx, approval.Params{UserID: e.alice.ID, Tool: "email.send", Capability: "email.send",
		Risk: integration.RiskWrite, Args: json.RawMessage(`{"to":"bob@example.com"}`)})
	if err != nil {
		t.Fatal(err)
	}
	msg := e.api.waitSent(t, 730, "Approval needed: email.send")
	if d := msg.callbackData(); len(d) != 2 || d[0] != "a:"+a.ID+":y" {
		t.Fatalf("keyboard = %v", d)
	}
	msgID := msg.ID
	// /approvals lists it again.
	e.api.send(730, "alice_tg", "/approvals")
	waitFor(t, "approvals listing", func() bool {
		n := 0
		for _, s := range e.api.sentTexts(730) {
			if strings.Contains(s, "Approval needed: email.send") {
				n++
			}
		}
		return n == 2
	})
	if err := e.appr.Resolve(e.ctx, a.ID, e.alice.ID, approval.Deny); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "approval messages updated", func() bool {
		n, seen := 0, map[int64]bool{}
		for _, c := range e.api.callsTo("editMessageText", 730) {
			if strings.HasSuffix(c.text(), "Denied") && c.Params["reply_markup"] == nil {
				n++
				seen[c.msgID()] = true
			}
		}
		return n == 2 && seen[msgID]
	})
	e.api.send(730, "alice_tg", "/approvals")
	e.api.waitSent(t, 730, "No approvals are waiting")

	// routine_finished delivers the run's reply.
	r, err := e.st.CreateRoutine(e.ctx, &store.Routine{UserID: e.alice.ID, Name: "morning-brief", TriggerKind: "cron",
		TriggerSpec: "0 7 * * *", Prompt: "brief me", Deliver: []string{"web"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.st.StartRoutineRun(e.ctx, &store.RoutineRun{RoutineID: r.ID, UserID: e.alice.ID, Source: "manual", Status: "running"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishRoutineRun(e.ctx, run.ID, "done", "", "Good morning! Nothing urgent today.", time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.bus.Publish(e.alice.ID, events.Event{Type: events.TypeRoutineFinished, Data: events.RoutineFinishedData{
		RoutineID: r.ID, RunID: run.ID, Status: "done"}})
	got := e.api.waitSent(t, 730, "Routine morning-brief")
	if !strings.HasSuffix(got.text(), "Good morning! Nothing urgent today.") {
		t.Fatalf("routine message = %q", got.text())
	}
	// Bob's routines never reach alice's chat.
	e.bus.Publish(e.bob.ID, events.Event{Type: events.TypeRoutineFinished, Data: events.RoutineFinishedData{RoutineID: r.ID, RunID: run.ID}})
	e.barrier()
	if n := len(e.runner.calls()); n != 0 {
		t.Fatalf("agent ran %d times", n)
	}
}

func TestGroupChatsIgnored(t *testing.T) {
	e := newEnv(t, nil)
	code, _, _ := e.linker.NewCode(e.ctx, e.alice.ID, Name)
	group := Chat{ID: -100123, Type: "group"}
	e.api.push(Update{Message: &Message{From: &User{ID: 740, Username: "alice_tg"}, Chat: group, Text: "/link " + code}})
	e.api.push(Update{Message: &Message{From: &User{ID: 740, Username: "alice_tg"}, Chat: group, Text: "hello bot"}})
	e.api.push(Update{Message: &Message{From: &User{ID: 740, Username: "alice_tg"}, Chat: group, Text: "/link " + code}})
	e.barrier()
	if got := e.api.sentTexts(group.ID); len(got) != 1 || !strings.Contains(got[0], "only works in private chats") {
		t.Fatalf("group replies = %q", got)
	}
	if links, _ := e.linker.ChannelLinks(e.ctx, Name); len(links) != 0 {
		t.Fatalf("group got linked: %+v", links)
	}
	if n := len(e.runner.calls()); n != 0 {
		t.Fatalf("agent ran for a group")
	}
	// The code was not consumed by the refused attempt.
	e.api.send(740, "alice_tg", "/link "+code)
	e.api.waitSent(t, 740, "Linked to Bosun user alice")
}

func TestAllowlist(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.AllowedUsernames = []string{"@Alice_TG"} })
	code, _, _ := e.linker.NewCode(e.ctx, e.alice.ID, Name)
	e.api.send(750, "mallory", "/link "+code)
	e.api.waitSent(t, 750, "not allowed")
	e.api.send(751, "", "/link "+code) // no username at all
	e.api.waitSent(t, 751, "not allowed")
	if links, _ := e.linker.ChannelLinks(e.ctx, Name); len(links) != 0 {
		t.Fatalf("disallowed user linked: %+v", links)
	}
	e.api.send(752, "alice_tg", "/link "+code)
	e.api.waitSent(t, 752, "Linked to Bosun user alice")
}

func TestWaitsForTokenAndInstructions(t *testing.T) {
	var ready atomic.Bool
	api := newFakeAPI(t, testToken)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Migrate(context.Background())
	bus := events.NewBus()
	bot, err := New(Options{
		Token: func(context.Context) (string, error) {
			if ready.Load() {
				return testToken, nil
			}
			return "", nil
		},
		Linker: channels.NewLinker(st, channels.LinkerOptions{}), Bus: bus, Runner: &fakeRunner{}, Store: st,
		Approvals: approval.NewManager(st, bus, approval.Options{}), APIURL: api.srv.URL, TokenRetry: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bot.LinkInstructions("ABC234"), "your Bosun bot") {
		t.Fatalf("instructions before getMe = %q", bot.LinkInstructions("ABC234"))
	}
	if err := bot.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bot.Close()
	waitFor(t, "missing token status", func() bool {
		s := bot.Status()
		return s.State == integration.StatusDown && strings.Contains(s.Message, "no bot token")
	})
	ready.Store(true)
	waitFor(t, "polling", func() bool { return bot.Status().State == integration.StatusOK })
	if got := bot.LinkInstructions("ABC234"); got != "Send /link ABC234 to @bosun_test_bot on Telegram." {
		t.Fatalf("instructions = %q", got)
	}
}

func TestRejectedTokenNotLeaked(t *testing.T) {
	api := newFakeAPI(t, testToken)
	c := NewClient(api.srv.URL, "999:wrong-secret", nil)
	_, err := c.GetMe(context.Background())
	if ae, ok := err.(*APIError); !ok || !ae.Unauthorized() {
		t.Fatalf("err = %v", err)
	}
	c = NewClient("http://127.0.0.1:1", "999:wrong-secret", nil)
	_, err = c.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("network error leaks token or is nil: %v", err)
	}
}
