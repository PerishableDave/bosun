package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// tick installs a clock that advances one second per call, so rows get
// distinct, ordered timestamps.
func tick(s *Store) {
	var mu sync.Mutex
	t := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(time.Second)
		return t
	})
}

func textMsg(t *testing.T, s *Store, convID, role, text string) {
	t.Helper()
	b, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	if _, err := s.AppendMessage(context.Background(), convID, role, b); err != nil {
		t.Fatal(err)
	}
}

func ids(cs []*Conversation) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Title)
	}
	return strings.Join(out, ",")
}

// TestConversationMigrationBackfill applies migrations up to 0005, adds
// conversations the way older versions did, then migrates.
func TestConversationMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir()+"/old.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.version <= 5 {
			if err := s.applyMigration(ctx, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	u := mustUser(t, s, "a")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO routines(id, user_id, name, trigger_kind, trigger_spec, prompt, created_at, updated_at) VALUES (1, ?, 'brief', 'cron', '@daily', 'p', 1, 1)`, u.ID)
	for _, c := range []struct {
		id, title string
		routine   any
	}{{"c_chat", "", nil}, {"c_named", "Trip", nil}, {"c_routine", "routine: brief", 1}, {"c_orphan", "routine: gone", nil}, {"c_tg", "telegram", nil}} {
		exec(`INSERT INTO conversations(id, user_id, title, routine_id, created_at, updated_at) VALUES (?, ?, ?, ?, 1000, 1000)`, c.id, u.ID, c.title, c.routine)
	}
	exec(`INSERT INTO messages(conversation_id, role, content, created_at) VALUES ('c_chat', 'user', '[{"type":"text","text":"hi"}]', 5000)`)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"c_chat": "chat/auto", "c_named": "chat/user", "c_routine": "routine/auto", "c_orphan": "routine/auto", "c_tg": "telegram/auto"}
	for id, w := range want {
		c, err := s.GetConversation(ctx, u.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Source + "/" + c.TitleSource; got != w {
			t.Errorf("%s: %s, want %s", id, got, w)
		}
		if (id == "c_chat") != !c.LastMessageAt.IsZero() {
			t.Errorf("%s: last_message_at = %v", id, c.LastMessageAt)
		}
	}
}

func TestConversationCreateDefaults(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a")
	r, err := s.CreateRoutine(ctx, &Routine{UserID: u.ID, Name: "brief", TriggerKind: "cron", TriggerSpec: "0 7 * * *", Prompt: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		nc                  NewConversation
		source, titleSource string
	}{
		{NewConversation{}, SourceChat, TitleAuto},
		{NewConversation{Title: "Trip"}, SourceChat, TitleUser},
		{NewConversation{Title: "routine: brief", RoutineID: r.ID}, SourceRoutine, TitleAuto},
		{NewConversation{Title: "telegram", Source: SourceTelegram}, SourceTelegram, TitleAuto},
		{NewConversation{Title: "x", TitleSource: TitleAuto}, SourceChat, TitleAuto},
	}
	for _, c := range cases {
		got, err := s.CreateConversationWith(ctx, u.ID, c.nc)
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != c.source || got.TitleSource != c.titleSource || got.Pinned || got.Archived || got.Unread ||
			got.MessageCount != 0 || !got.LastMessageAt.IsZero() || got.LastMessagePreview != "" {
			t.Errorf("%+v: got %+v", c.nc, got)
		}
		if c.nc.RoutineID != 0 && got.RoutineName != "brief" {
			t.Errorf("routine name = %q", got.RoutineName)
		}
	}
}

func TestConversationPreviewAndCount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a")
	c, _ := s.CreateConversation(ctx, u.ID, "", 0)
	textMsg(t, s, c.ID, "user", "first   question\nwith  lines")
	got, _ := s.GetConversation(ctx, u.ID, c.ID)
	if got.LastMessagePreview != "first question with lines" || got.MessageCount != 1 || got.LastMessageAt.IsZero() {
		t.Errorf("after user msg: %+v", got)
	}
	long := strings.Repeat("ab ", 100)
	textMsg(t, s, c.ID, "assistant", long)
	// A tool-only assistant message and a tool result do not change the preview.
	s.AppendMessage(ctx, c.ID, "assistant", json.RawMessage(`[{"type":"tool_use","id":"t1","name":"x.y","input":{}}]`))
	s.AppendMessage(ctx, c.ID, "tool", json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"secret tool output"}]`))
	got, _ = s.GetConversation(ctx, u.ID, c.ID)
	if n := len([]rune(got.LastMessagePreview)); n != PreviewLen || !strings.HasSuffix(got.LastMessagePreview, "…") || !strings.HasPrefix(got.LastMessagePreview, "ab ab") {
		t.Errorf("preview (%d) = %q", n, got.LastMessagePreview)
	}
	if got.MessageCount != 3 {
		t.Errorf("message count = %d, want 3 (user + 2 assistant)", got.MessageCount)
	}
	// Unicode survives and is counted in characters.
	textMsg(t, s, c.ID, "user", strings.Repeat("é", 200))
	got, _ = s.GetConversation(ctx, u.ID, c.ID)
	if r := []rune(got.LastMessagePreview); len(r) != PreviewLen || r[0] != 'é' {
		t.Errorf("unicode preview = %q", got.LastMessagePreview)
	}
}

func TestConversationListFiltersAndCursor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tick(s)
	a, b := mustUser(t, s, "a"), mustUser(t, s, "b")
	mk := func(title string) *Conversation {
		c, err := s.CreateConversationWith(ctx, a.ID, NewConversation{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1, c2, c3, c4, c5 := mk("one"), mk("two"), mk("three"), mk("four"), mk("five")
	textMsg(t, s, c1.ID, "user", "talk about Espresso machines") // c1 most recent activity
	textMsg(t, s, c3.ID, "assistant", "50% off_sale")
	s.AppendMessage(ctx, c2.ID, "tool", json.RawMessage(`[{"type":"tool_result","tool_use_id":"t","content":"espresso"}]`))
	tr, fa := true, false
	if _, err := s.UpdateConversation(ctx, a.ID, c4.ID, ConversationPatch{Pinned: &tr}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateConversation(ctx, a.ID, c5.ID, ConversationPatch{Archived: &tr}); err != nil {
		t.Fatal(err)
	}
	s.CreateConversation(ctx, b.ID, "other user", 0)

	list := func(f ConversationFilter) ([]*Conversation, string) {
		t.Helper()
		cs, next, err := s.ListConversationPage(ctx, a.ID, f)
		if err != nil {
			t.Fatal(err)
		}
		return cs, next
	}
	// Order: pinned, then activity (c1 and c3 have messages; c2's tool message counts as activity).
	if cs, next := list(ConversationFilter{}); ids(cs) != "four,two,three,one,five" || next != "" {
		t.Errorf("all = %s next %q", ids(cs), next)
	}
	if cs, _ := list(ConversationFilter{Archived: &fa}); ids(cs) != "four,two,three,one" {
		t.Errorf("unarchived = %s", ids(cs))
	}
	if cs, _ := list(ConversationFilter{Archived: &tr}); ids(cs) != "five" {
		t.Errorf("archived = %s", ids(cs))
	}
	// Search: title, message text (case-insensitive), not tool output, wildcards literal.
	if cs, _ := list(ConversationFilter{Query: "espresso"}); ids(cs) != "one" {
		t.Errorf("q=espresso = %s", ids(cs))
	}
	if cs, _ := list(ConversationFilter{Query: "THRE"}); ids(cs) != "three" {
		t.Errorf("q=THRE = %s", ids(cs))
	}
	if cs, _ := list(ConversationFilter{Query: "0% off_"}); ids(cs) != "three" {
		t.Errorf("q=0%% off_ = %s", ids(cs))
	}
	if cs, _ := list(ConversationFilter{Query: "%%"}); len(cs) != 0 {
		t.Errorf("q=%%%% = %s", ids(cs))
	}
	if cs, _ := list(ConversationFilter{Query: "text"}); len(cs) != 0 {
		t.Errorf("q matched JSON keys: %s", ids(cs))
	}
	// Pages of two walk the whole list without gaps or repeats.
	var seen []string
	cur := ""
	for i := 0; i < 5; i++ {
		cs, next := list(ConversationFilter{Limit: 2, Before: cur})
		seen = append(seen, ids(cs))
		if next == "" {
			break
		}
		cur = next
	}
	if got := strings.Join(seen, "|"); got != "four,two|three,one|five" {
		t.Errorf("pages = %s", got)
	}
	if _, _, err := s.ListConversationPage(ctx, a.ID, ConversationFilter{Before: "garbage!"}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("bad cursor err = %v", err)
	}
	if cs, _, _ := s.ListConversationPage(ctx, b.ID, ConversationFilter{}); ids(cs) != "other user" {
		t.Errorf("user b = %s", ids(cs))
	}
}

func TestConversationPatchTitleUnreadDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, b := mustUser(t, s, "a"), mustUser(t, s, "b")
	c, _ := s.CreateConversation(ctx, a.ID, "", 0)

	got, changed, err := s.SetAutoTitle(ctx, a.ID, c.ID, "Auto one")
	if err != nil || !changed || got.Title != "Auto one" || got.TitleSource != TitleAuto {
		t.Fatalf("auto title = %+v %v %v", got, changed, err)
	}
	if _, changed, _ := s.SetAutoTitle(ctx, a.ID, c.ID, "Auto one"); changed {
		t.Error("same auto title reported a change")
	}
	title := "Mine"
	if _, err := s.UpdateConversation(ctx, b.ID, c.ID, ConversationPatch{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user's patch: %v", err)
	}
	got, err = s.UpdateConversation(ctx, a.ID, c.ID, ConversationPatch{Title: &title})
	if err != nil || got.Title != "Mine" || got.TitleSource != TitleUser {
		t.Fatalf("patch = %+v %v", got, err)
	}
	if got, changed, _ := s.SetAutoTitle(ctx, a.ID, c.ID, "Auto two"); changed || got.Title != "Mine" {
		t.Errorf("auto title overwrote the user's: %+v", got)
	}

	got, changed, err = s.SetConversationUnread(ctx, a.ID, c.ID, true)
	if err != nil || !changed || !got.Unread {
		t.Fatalf("unread = %+v %v %v", got, changed, err)
	}
	if _, changed, _ := s.SetConversationUnread(ctx, a.ID, c.ID, true); changed {
		t.Error("unread twice reported a change")
	}
	if _, _, err := s.SetConversationUnread(ctx, b.ID, c.ID, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user's read: %v", err)
	}
	if got, changed, _ := s.SetConversationUnread(ctx, a.ID, c.ID, false); !changed || got.Unread {
		t.Errorf("read = %+v", got)
	}

	textMsg(t, s, c.ID, "user", "hi")
	if err := s.DeleteConversation(ctx, b.ID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user's delete: %v", err)
	}
	if err := s.DeleteConversation(ctx, a.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE conversation_id = ?`, c.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d messages left after delete", n)
	}
}
