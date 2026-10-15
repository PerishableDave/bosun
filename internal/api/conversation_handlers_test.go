package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/agent"
	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/titles"
)

// stallLLM streams one text chunk and then waits for cancellation.
type stallLLM struct{}

func (stallLLM) Name() string { return "stall" }
func (stallLLM) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}
func (stallLLM) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 1)
	go func() {
		defer close(ch)
		ch <- llm.Event{Type: llm.EventTextDelta, Text: "thinking"}
		<-ctx.Done()
		ch <- llm.Event{Type: llm.EventError, Err: ctx.Err()}
	}()
	return ch, nil
}

type convEnv struct {
	*env
	st     *store.Store
	s      *Server
	chat   *fake.Provider // the turn model ("fake/m")
	titler *fake.Provider // the title model
	appr   *approval.StoreManager
}

func setupConv(t *testing.T, model string) *convEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(st, auth.Options{Argon2: &auth.Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.CreateUser(ctx, "dave", "longenough", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateUser(ctx, "eve", "longenough", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	chat := fake.New("fake")
	tp := fake.New("small")
	reg := llm.NewRegistry()
	reg.Register(chat)
	reg.Register(stallLLM{})
	ag := agent.New(reg, model, nil, nil, st, bus)
	t.Cleanup(ag.Close)
	appr := approval.NewManager(st, bus, approval.Options{})
	s, err := New(Options{Auth: a, Bus: bus, Version: "test", Heartbeat: 50 * time.Millisecond,
		Conversations: st, Agent: ag, Approvals: appr, Titler: titles.NewGenerator(tp, "small", nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &convEnv{env: &env{srv: srv, auth: a, bus: bus, user: u}, st: st, s: s, chat: chat, titler: tp, appr: appr}
}

func (e *convEnv) loginAs(t *testing.T, name string) string {
	t.Helper()
	resp := e.do(t, "POST", "/v1/login", "", `{"username":"`+name+`","password":"longenough"}`)
	var lr LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil || lr.Token == "" {
		t.Fatalf("login %s: %d %v", name, resp.StatusCode, err)
	}
	return lr.Token
}

func jsonOf[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func (e *convEnv) create(t *testing.T, tok, body string) ConversationResponse {
	t.Helper()
	resp := e.do(t, "POST", "/v1/conversations", tok, body)
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	return jsonOf[ConversationResponse](t, resp)
}

type sseEvent struct{ typ, data string }

// readSSE reads a stream until it ends.
func readSSE(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(body)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "" && cur.typ != "":
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func (e *convEnv) turn(t *testing.T, tok, convID, text string) []sseEvent {
	t.Helper()
	resp := e.do(t, "POST", "/v1/conversations/"+convID+"/messages", tok, `{"content":`+mustJSON(text)+`}`)
	if resp.StatusCode != 200 {
		t.Fatalf("post message = %d", resp.StatusCode)
	}
	return readSSE(t, resp.Body)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// updates returns the conversation_updated payloads buffered on sub.
func updates(sub *events.Subscription) []events.ConversationUpdatedData {
	var out []events.ConversationUpdatedData
	for {
		select {
		case ev := <-sub.C:
			if d, ok := ev.Data.(events.ConversationUpdatedData); ok {
				out = append(out, d)
			}
		default:
			return out
		}
	}
}

func TestConversationAutoTitle(t *testing.T) {
	e := setupConv(t, "fake/m")
	tok := e.login(t)
	sub := e.bus.Subscribe(e.user.ID, 0)
	defer sub.Close()

	c := e.create(t, tok, `{"title":"New chat"}`)
	if c.TitleSource != "auto" || c.Source != "chat" || c.MessageCount != 0 || c.LastMessageAt != nil || c.Pinned || c.Archived || c.Unread {
		t.Fatalf("created = %+v", c)
	}
	e.chat.Push(fake.Text("Lisbon in October is lovely."))
	e.titler.Push(fake.Text(`{"title":"October Lisbon trip ideas"}`))
	evs := e.turn(t, tok, c.ID, "Plan a three day trip to Lisbon in October with kids please")
	// The provisional title is announced on the turn's own stream too.
	if len(evs) == 0 || evs[0].typ != "conversation_updated" || !strings.Contains(evs[0].data, `"title":"Plan a three day trip to Lisbon in…"`) {
		t.Fatalf("stream = %+v", evs)
	}
	if last := evs[len(evs)-1]; last.typ != "done" || strings.Contains(last.data, "stopped") {
		t.Errorf("last = %+v", last)
	}
	e.s.waitBackground()
	ups := updates(sub)
	if len(ups) != 2 || ups[0].Title != "Plan a three day trip to Lisbon in…" || ups[1].Title != "October Lisbon trip ideas" ||
		ups[1].TitleSource != "auto" || ups[1].ID != c.ID {
		t.Fatalf("updates = %+v", ups)
	}
	if req := e.titler.Requests(); len(req) != 1 || !strings.Contains(req[0].Messages[0].Text(), "Lisbon in October is lovely.") {
		t.Errorf("title requests = %+v", req)
	}
	got := jsonOf[ConversationDetailResponse](t, e.do(t, "GET", "/v1/conversations/"+c.ID, tok, ""))
	if got.Title != "October Lisbon trip ideas" || got.MessageCount != 2 || got.LastMessagePreview != "Lisbon in October is lovely." ||
		got.LastMessageAt == nil || len(got.Messages) != 2 {
		t.Errorf("detail = %+v", got.ConversationResponse)
	}

	// A second turn does not retitle.
	e.chat.Push(fake.Text("ok"))
	e.turn(t, tok, c.ID, "thanks")
	e.s.waitBackground()
	if n := len(e.titler.Requests()); n != 1 {
		t.Errorf("title calls = %d", n)
	}

	// Garbage from the title model keeps the provisional title.
	c2 := e.create(t, tok, `{}`)
	e.chat.Push(fake.Text("Hello!"))
	e.titler.Push(fake.Text("Sure, here is a title: Greetings"))
	e.turn(t, tok, c2.ID, "hi")
	e.s.waitBackground()
	if got := jsonOf[ConversationResponse](t, e.do(t, "GET", "/v1/conversations/"+c2.ID, tok, "")); got.Title != "hi" {
		t.Errorf("garbage title: %q", got.Title)
	}

	// The user's title wins: renamed before the async pass finishes...
	c3 := e.create(t, tok, `{}`)
	resp := e.do(t, "PATCH", "/v1/conversations/"+c3.ID, tok, `{"title":"  My   own\u0007 "}`)
	p := jsonOf[ConversationResponse](t, resp)
	if resp.StatusCode != 200 || p.Title != "My   own" || p.TitleSource != "user" {
		t.Fatalf("patch = %d %+v", resp.StatusCode, p)
	}
	e.chat.Push(fake.Text("Sure."))
	e.titler.Push(fake.Text(`{"title":"Should not apply"}`))
	e.turn(t, tok, c3.ID, "something")
	e.s.waitBackground()
	if got := jsonOf[ConversationResponse](t, e.do(t, "GET", "/v1/conversations/"+c3.ID, tok, "")); got.Title != "My   own" || got.TitleSource != "user" {
		t.Errorf("user title overwritten: %+v", got)
	}
	// ...and a title given at creation is the user's too.
	c4 := e.create(t, tok, `{"title":"Budget"}`)
	if c4.TitleSource != "user" {
		t.Errorf("created with title: %+v", c4)
	}
}

func TestConversationListPatchRead(t *testing.T) {
	e := setupConv(t, "fake/m")
	tok := e.login(t)
	ctx := context.Background()
	var ids []string
	for _, title := range []string{"alpha", "beta", "gamma"} {
		ids = append(ids, e.create(t, tok, `{"title":"`+title+`"}`).ID)
	}
	b, _ := json.Marshal([]llm.ContentBlock{llm.TextBlock("espresso grinder")})
	e.st.AppendMessage(ctx, ids[0], "user", b)

	sub := e.bus.Subscribe(e.user.ID, 0)
	defer sub.Close()
	resp := e.do(t, "PATCH", "/v1/conversations/"+ids[1], tok, `{"archived":true}`)
	if resp.StatusCode != 200 || !jsonOf[ConversationResponse](t, resp).Archived {
		t.Fatalf("archive = %d", resp.StatusCode)
	}
	resp = e.do(t, "PATCH", "/v1/conversations/"+ids[2], tok, `{"pinned":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("pin = %d", resp.StatusCode)
	}
	if ups := updates(sub); len(ups) != 2 || !ups[0].Archived || !ups[1].Pinned || ups[1].Title != "gamma" {
		t.Errorf("updates = %+v", ups)
	}
	for _, body := range []string{`{}`, `{"title":"   "}`, `{"color":"red"}`} {
		if resp := e.do(t, "PATCH", "/v1/conversations/"+ids[0], tok, body); resp.StatusCode != 400 {
			t.Errorf("patch %s = %d", body, resp.StatusCode)
		}
	}

	titlesOf := func(path string) (string, *string) {
		t.Helper()
		resp := e.do(t, "GET", path, tok, "")
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		l := jsonOf[ConversationListResponse](t, resp)
		var ts []string
		for _, c := range l.Conversations {
			ts = append(ts, c.Title)
		}
		return strings.Join(ts, ","), l.NextCursor
	}
	if got, next := titlesOf("/v1/conversations"); got != "gamma,alpha" || next != nil {
		t.Errorf("default list = %s %v", got, next)
	}
	if got, _ := titlesOf("/v1/conversations?archived=true"); got != "beta" {
		t.Errorf("archived = %s", got)
	}
	if got, _ := titlesOf("/v1/conversations?archived=all"); got != "gamma,alpha,beta" {
		t.Errorf("all = %s", got)
	}
	if got, _ := titlesOf("/v1/conversations?archived=all&q=GRINDER"); got != "alpha" {
		t.Errorf("q = %s", got)
	}
	got, next := titlesOf("/v1/conversations?archived=all&limit=2")
	if got != "gamma,alpha" || next == nil {
		t.Fatalf("page 1 = %s %v", got, next)
	}
	if got, next := titlesOf("/v1/conversations?archived=all&limit=2&before=" + *next); got != "beta" || next != nil {
		t.Errorf("page 2 = %s %v", got, next)
	}
	for _, q := range []string{"archived=maybe", "limit=0", "limit=201", "limit=x", "before=%21%21"} {
		if resp := e.do(t, "GET", "/v1/conversations?"+q, tok, ""); resp.StatusCode != 400 {
			t.Errorf("?%s = %d", q, resp.StatusCode)
		}
	}

	// Unread: set by routines (store), cleared by read.
	if _, _, err := e.st.SetConversationUnread(ctx, e.user.ID, ids[0], true); err != nil {
		t.Fatal(err)
	}
	if l := jsonOf[ConversationListResponse](t, e.do(t, "GET", "/v1/conversations?q=alpha", tok, "")); !l.Conversations[0].Unread {
		t.Error("unread not listed")
	}
	if resp := e.do(t, "POST", "/v1/conversations/"+ids[0]+"/read", tok, ""); resp.StatusCode != 204 {
		t.Errorf("read = %d", resp.StatusCode)
	}
	if ups := updates(sub); len(ups) != 1 || ups[0].Unread || ups[0].ID != ids[0] {
		t.Errorf("read updates = %+v", ups)
	}
	if resp := e.do(t, "POST", "/v1/conversations/"+ids[0]+"/read", tok, ""); resp.StatusCode != 204 || len(updates(sub)) != 0 {
		t.Errorf("second read = %d (or published again)", resp.StatusCode)
	}
}

func TestConversationOtherUsers404(t *testing.T) {
	e := setupConv(t, "fake/m")
	tok, eve := e.login(t), e.loginAs(t, "eve")
	c := e.create(t, tok, `{"title":"private"}`)
	cases := []struct{ method, path, body string }{
		{"GET", "/v1/conversations/" + c.ID, ""},
		{"PATCH", "/v1/conversations/" + c.ID, `{"pinned":true}`},
		{"DELETE", "/v1/conversations/" + c.ID, ""},
		{"POST", "/v1/conversations/" + c.ID + "/stop", ""},
		{"POST", "/v1/conversations/" + c.ID + "/read", ""},
		{"GET", "/v1/conversations/" + c.ID + "/export", ""},
		{"POST", "/v1/conversations/" + c.ID + "/messages", `{"content":"hi"}`},
		{"GET", "/v1/conversations/conv_nope/export", ""},
	}
	for _, k := range cases {
		if resp := e.do(t, k.method, k.path, eve, k.body); resp.StatusCode != 404 {
			t.Errorf("eve %s %s = %d", k.method, k.path, resp.StatusCode)
		}
	}
	if l := jsonOf[ConversationListResponse](t, e.do(t, "GET", "/v1/conversations?archived=all", eve, "")); len(l.Conversations) != 0 {
		t.Errorf("eve lists %d", len(l.Conversations))
	}
	if got := jsonOf[ConversationResponse](t, e.do(t, "GET", "/v1/conversations/"+c.ID, tok, "")); got.Pinned {
		t.Error("eve's patch applied")
	}
}

func TestConversationStopAndDelete(t *testing.T) {
	e := setupConv(t, "stall/m")
	tok := e.login(t)
	c := e.create(t, tok, `{"title":"long one"}`)
	if r := jsonOf[StopResponse](t, e.do(t, "POST", "/v1/conversations/"+c.ID+"/stop", tok, "")); r.Stopped {
		t.Error("stop with no turn reported stopped")
	}

	// Start a turn that only ends when stopped.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/conversations/"+c.ID+"/messages", strings.NewReader(`{"content":"go"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("post = %v %v", resp, err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for { // wait until the turn is streaming
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "event: text\n" {
			break
		}
	}
	if r := e.do(t, "DELETE", "/v1/conversations/"+c.ID, tok, ""); r.StatusCode != 409 {
		t.Errorf("delete during turn = %d", r.StatusCode)
	}
	r := e.do(t, "POST", "/v1/conversations/"+c.ID+"/stop", tok, "")
	if r.StatusCode != 202 || !jsonOf[StopResponse](t, r).Stopped {
		t.Errorf("stop = %d", r.StatusCode)
	}
	evs := readSSE(t, br)
	if len(evs) == 0 || evs[len(evs)-1].typ != "done" || !strings.Contains(evs[len(evs)-1].data, `"stopped":true`) {
		t.Fatalf("after stop: %+v", evs)
	}
	// The streamed text was kept.
	got := jsonOf[ConversationDetailResponse](t, e.do(t, "GET", "/v1/conversations/"+c.ID, tok, ""))
	if len(got.Messages) != 2 || got.LastMessagePreview != "thinking" {
		t.Errorf("after stop: %+v", got)
	}

	// Delete cancels the conversation's pending approvals (and no others).
	ctx := context.Background()
	other := e.create(t, tok, `{}`)
	mine, _ := e.st.CreateApproval(ctx, store.Approval{UserID: e.user.ID, ConversationID: c.ID, Tool: "x.y", Capability: "x.y", Risk: "write", ExpiresAt: time.Now().Add(time.Hour)})
	keep, _ := e.st.CreateApproval(ctx, store.Approval{UserID: e.user.ID, ConversationID: other.ID, Tool: "x.y", Capability: "x.y", Risk: "write", ExpiresAt: time.Now().Add(time.Hour)})
	sub := e.bus.Subscribe(e.user.ID, 0)
	defer sub.Close()
	if r := e.do(t, "DELETE", "/v1/conversations/"+c.ID, tok, ""); r.StatusCode != 204 {
		t.Fatalf("delete = %d", r.StatusCode)
	}
	if a, _ := e.st.GetApproval(ctx, mine.ID); a.Status != store.ApprovalCanceled {
		t.Errorf("approval in deleted conversation: %s", a.Status)
	}
	if a, _ := e.st.GetApproval(ctx, keep.ID); a.Status != store.ApprovalPending {
		t.Errorf("other approval: %s", a.Status)
	}
	var sawResolved, sawDeleted bool
	for len(sub.C) > 0 {
		ev := <-sub.C
		switch d := ev.Data.(type) {
		case events.ApprovalResolvedData:
			sawResolved = d.ApprovalID == mine.ID && d.Decision == "canceled"
		case events.ConversationDeletedData:
			sawDeleted = d.ID == c.ID
		}
	}
	if !sawResolved || !sawDeleted {
		t.Errorf("events: resolved %v deleted %v", sawResolved, sawDeleted)
	}
	if r := e.do(t, "GET", "/v1/conversations/"+c.ID, tok, ""); r.StatusCode != 404 {
		t.Errorf("get after delete = %d", r.StatusCode)
	}
	if r := e.do(t, "DELETE", "/v1/conversations/"+c.ID, tok, ""); r.StatusCode != 404 {
		t.Errorf("second delete = %d", r.StatusCode)
	}
}

func TestConversationExport(t *testing.T) {
	e := setupConv(t, "fake/m")
	tok := e.login(t)
	ctx := context.Background()
	c := e.create(t, tok, `{"title":"Coffee: plans/ideas"}`)
	add := func(role string, blocks ...llm.ContentBlock) {
		b, _ := json.Marshal(blocks)
		if _, err := e.st.AppendMessage(ctx, c.ID, role, b); err != nil {
			t.Fatal(err)
		}
	}
	add("user", llm.TextBlock("Find a grinder"))
	add("assistant", llm.ToolUseBlock("t1", "websearch.search", json.RawMessage(`{"q":"grinder"}`)))
	add("tool", llm.ToolResultBlock("t1", strings.Repeat("r", 600), false))
	add("assistant", llm.TextBlock("The **Baratza** is good."))

	resp := e.do(t, "GET", "/v1/conversations/"+c.ID+"/export?format=markdown", tok, "")
	raw, _ := io.ReadAll(resp.Body)
	md := string(raw)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="coffee-plans-ideas.md"` {
		t.Fatalf("export = %d %v", resp.StatusCode, resp.Header)
	}
	for _, want := range []string{"# Coffee: plans/ideas\n", "## You · ", "Find a grinder", "## Bosun · ",
		"Tool call `websearch.search` `{\"q\":\"grinder\"}`", "> Result: " + strings.Repeat("r", 500) + "…", "The **Baratza** is good."} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Count(md, "## Bosun") != 1 {
		t.Errorf("Bosun's turn split into several headings:\n%s", md)
	}
	if resp := e.do(t, "GET", "/v1/conversations/"+c.ID+"/export", tok, ""); resp.StatusCode != 200 ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Errorf("default format = %d", resp.StatusCode)
	}
	resp = e.do(t, "GET", "/v1/conversations/"+c.ID+"/export?format=json", tok, "")
	j := jsonOf[ConversationDetailResponse](t, resp)
	if resp.Header.Get("Content-Disposition") != `attachment; filename="coffee-plans-ideas.json"` || len(j.Messages) != 4 || j.ID != c.ID {
		t.Errorf("json export = %v %+v", resp.Header, j.ConversationResponse)
	}
	if resp := e.do(t, "GET", "/v1/conversations/"+c.ID+"/export?format=pdf", tok, ""); resp.StatusCode != 400 {
		t.Errorf("pdf = %d", resp.StatusCode)
	}
}
