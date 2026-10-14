package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/store"
)

type env struct {
	st    *store.Store
	m     *Store
	alice int64
	bob   int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateUser(ctx, "alice", "x", "admin")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateUser(ctx, "bob", "x", "member")
	if err != nil {
		t.Fatal(err)
	}
	return &env{st: st, m: NewStore(st), alice: a.ID, bob: b.ID}
}

func (e *env) add(t *testing.T, user int64, kind Kind, content string) Memory {
	t.Helper()
	m, err := e.m.Add(context.Background(), user, kind, content, "user")
	if err != nil {
		t.Fatalf("Add(%q): %v", content, err)
	}
	return m
}

func contents(ms []Memory) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Content)
	}
	return out
}

func TestCRUD(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.add(t, e.alice, KindFact, "  The user lives in Lisbon.  ")
	if m.ID == 0 || m.Content != "The user lives in Lisbon." || m.Kind != KindFact || m.Source != "user" || m.CreatedAt.IsZero() {
		t.Fatalf("Add = %+v", m)
	}
	got, err := e.m.Get(ctx, e.alice, m.ID)
	if err != nil || got.Content != m.Content || got.UserID != e.alice {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	up, err := e.m.Update(ctx, e.alice, m.ID, "The user lives in Porto.")
	if err != nil || up.Content != "The user lives in Porto." || up.Kind != KindFact {
		t.Fatalf("Update = %+v, %v", up, err)
	}
	if hits, _ := e.m.Search(ctx, e.alice, "porto", 10); len(hits) != 1 {
		t.Errorf("FTS index not updated: %v", contents(hits))
	}
	if hits, _ := e.m.Search(ctx, e.alice, "lisbon", 10); len(hits) != 0 {
		t.Errorf("stale FTS entry: %v", contents(hits))
	}
	if err := e.m.Delete(ctx, e.alice, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Get(ctx, e.alice, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	if err := e.m.Delete(ctx, e.alice, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if hits, _ := e.m.Search(ctx, e.alice, "porto", 10); len(hits) != 0 {
		t.Errorf("deleted memory still searchable: %v", contents(hits))
	}

	for _, bad := range []struct {
		kind    Kind
		content string
	}{{"secret", "x"}, {KindNote, "   "}, {KindNote, strings.Repeat("a", MaxContentLen+1)}} {
		if _, err := e.m.Add(ctx, e.alice, bad.kind, bad.content, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("Add(%q, %d bytes) err = %v, want ErrInvalid", bad.kind, len(bad.content), err)
		}
	}
	if _, err := e.m.List(ctx, e.alice, "bogus", 0, 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("List bad kind: %v", err)
	}
}

func TestListKindLimitOffset(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for i := range 5 {
		e.add(t, e.alice, KindNote, fmt.Sprintf("note %d", i))
	}
	e.add(t, e.alice, KindPreference, "Prefers metric units.")
	all, err := e.m.List(ctx, e.alice, "", 0, 0)
	if err != nil || len(all) != 6 || all[0].Content != "Prefers metric units." {
		t.Fatalf("List all = %v, %v", contents(all), err)
	}
	notes, _ := e.m.List(ctx, e.alice, KindNote, 2, 1)
	if got := strings.Join(contents(notes), "|"); got != "note 3|note 2" {
		t.Errorf("List notes limit 2 offset 1 = %q", got)
	}
}

func TestUserIsolation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	am := e.add(t, e.alice, KindFact, "Alice's dentist is Dr. Lee.")
	e.add(t, e.alice, KindPreference, "Alice prefers tea.")
	bm := e.add(t, e.bob, KindFact, "Bob's dentist is Dr. Kim.")

	for _, q := range []string{"dentist", "Dr", "dentist Kim", "Kim"} {
		hits, err := e.m.Search(ctx, e.alice, q, 50)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		for _, h := range hits {
			if h.UserID != e.alice || strings.Contains(h.Content, "Bob") {
				t.Errorf("alice's Search(%q) returned %+v", q, h)
			}
		}
	}
	if hits, _ := e.m.Search(ctx, e.bob, "dentist", 50); len(hits) != 1 || hits[0].ID != bm.ID {
		t.Errorf("bob's search = %v", contents(hits))
	}
	// LIKE fallback path is scoped too.
	if hits, _ := e.m.Search(ctx, e.alice, "%%%", 50); len(hits) != 0 {
		t.Errorf("alice's LIKE search = %v", contents(hits))
	}
	rec, _ := e.m.Recall(ctx, e.bob, "who is my dentist? and do I like tea", 10)
	for _, m := range rec {
		if m.UserID != e.bob {
			t.Errorf("bob's Recall returned %+v", m)
		}
	}
	if l, _ := e.m.List(ctx, e.bob, "", 0, 0); len(l) != 1 {
		t.Errorf("bob's List = %v", contents(l))
	}

	if _, err := e.m.Get(ctx, e.bob, am.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob Get alice's memory: %v", err)
	}
	if _, err := e.m.Update(ctx, e.bob, am.ID, "pwned"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob Update alice's memory: %v", err)
	}
	if err := e.m.Delete(ctx, e.bob, am.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob Delete alice's memory: %v", err)
	}
	if got, err := e.m.Get(ctx, e.alice, am.ID); err != nil || got.Content != am.Content {
		t.Errorf("alice's memory changed: %+v, %v", got, err)
	}
}

func TestSearchFTSSyntaxIsSafe(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.add(t, e.alice, KindNote, `Gate code is "4512" (front door) - AND the alarm: NEAR the stairs*`)
	e.add(t, e.alice, KindNote, "Wifi password is on the fridge.")
	for _, q := range []string{
		`"`, `""`, `"unterminated`, `*`, `foo*`, `AND`, `OR NOT`, `NEAR(`, `NEAR(gate code, 2)`, `(`, `)`,
		`-door`, `+`, `^gate`, `content:gate`, `gate:`, `{content}: x`, `'`, `\`, `%`, `_`, `a OR`, `NOT`,
		"\x00", "gate\x00code", `col:"x" AND (`, `🙂`, `"gate" "code"`,
	} {
		if _, err := e.m.Search(ctx, e.alice, q, 10); err != nil {
			t.Errorf("Search(%q): %v", q, err)
		}
		if _, err := e.m.Recall(ctx, e.alice, q, 10); err != nil {
			t.Errorf("Recall(%q): %v", q, err)
		}
	}
	for q, want := range map[string]int{
		`gate code`:          1,
		`"gate" (code)`:      1,
		`AND`:                1, // matched as the literal word, not an operator
		`stairs*`:            1,
		`fridge`:             1,
		`gate fridge`:        0, // all words must match
		`NEAR`:               1,
		`front-door`:         1,
		`wifi:`:              1,
		`nothing-like-this`:  0,
		`"4512"`:             1,
		`  `:                 0,
		`password fridge OR`: 0, // "OR" is a literal word here and absent
	} {
		hits, err := e.m.Search(ctx, e.alice, q, 10)
		if err != nil || len(hits) != want {
			t.Errorf("Search(%q) = %v, %v; want %d hits", q, contents(hits), err, want)
		}
	}
}

func TestSearchRanking(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.add(t, e.alice, KindNote, "garden garden garden: tomatoes in the garden")
	e.add(t, e.alice, KindNote, "The garden shed key is under the mat, near the car and the bikes and the old boxes.")
	hits, err := e.m.Search(ctx, e.alice, "garden", 10)
	if err != nil || len(hits) != 2 || !strings.HasPrefix(hits[0].Content, "garden garden") {
		t.Errorf("ranking = %v, %v", contents(hits), err)
	}
}

func TestRecall(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	dentist := e.add(t, e.alice, KindFact, "The user's dentist is Dr. Lee.")
	e.add(t, e.alice, KindNote, "Spare key is under the blue pot.")
	pref := e.add(t, e.alice, KindPreference, "Prefers short answers.")
	e.add(t, e.alice, KindPreference, "Uses metric units; dentist appointments in the morning.")

	got, err := e.m.Recall(ctx, e.alice, "When is my next dentist appointment?", 8)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]int{}
	for _, m := range got {
		ids[m.ID]++
	}
	if ids[dentist.ID] != 1 || ids[pref.ID] != 1 || len(got) != 3 {
		t.Errorf("Recall = %v", contents(got))
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("memory %d recalled %d times", id, n)
		}
	}
	if got[0].Kind == KindPreference && got[0].ID == pref.ID {
		t.Errorf("relevance hits should come first: %v", contents(got))
	}
	// Only stopwords: preferences only.
	got, _ = e.m.Recall(ctx, e.alice, "what is it?", 8)
	if len(got) != 2 {
		t.Errorf("Recall(stopwords) = %v", contents(got))
	}
	// max bounds the relevance hits.
	got, _ = e.m.Recall(ctx, e.alice, "dentist key blue pot", 1)
	hits := 0
	for _, m := range got {
		if m.Kind != KindPreference {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("Recall max=1 = %v", contents(got))
	}
	if got, _ := e.m.Recall(ctx, e.alice, "dentist", 0); len(got) != 0 {
		t.Errorf("Recall max=0 = %v", contents(got))
	}
}

func TestRecallCapsPreferences(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for i := range MaxRecallPreferences + 5 {
		e.add(t, e.alice, KindPreference, fmt.Sprintf("preference %d", i))
	}
	got, err := e.m.Recall(ctx, e.alice, "hello", 8)
	if err != nil || len(got) != MaxRecallPreferences {
		t.Errorf("Recall = %d memories, %v", len(got), err)
	}
}
