package channels

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestLinkerCodes(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	alice, _ := st.CreateUser(ctx, "alice", "h", "member")
	bob, _ := st.CreateUser(ctx, "bob", "h", "member")
	clk := &clock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	l := NewLinker(st, LinkerOptions{Now: clk.now})
	var changes []string
	l.OnChange(func(ch string, uid int64) { changes = append(changes, ch) })

	code, exp, err := l.NewCode(ctx, alice.ID, "telegram")
	if err != nil || len(code) != CodeLength || !exp.Equal(clk.now().Add(DefaultCodeTTL)) {
		t.Fatalf("NewCode = %q, %v, %v", code, exp, err)
	}
	for _, r := range code {
		if !strings.ContainsRune(codeAlphabet, r) {
			t.Fatalf("code %q has character %q outside the alphabet", code, r)
		}
	}
	// Wrong channel does not redeem (and does not consume) the code.
	if _, err := l.Redeem(ctx, "slack", code, "1", ""); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong channel err = %v", err)
	}
	// Lower case and separators are tolerated.
	lc := strings.ToLower(code[:3]) + "-" + code[3:]
	link, err := l.Redeem(ctx, "telegram", lc, "100", "Alice")
	if err != nil || link.UserID != alice.ID || link.ExternalID != "100" || link.DisplayName != "Alice" {
		t.Fatalf("Redeem = %+v, %v", link, err)
	}
	if len(changes) != 1 {
		t.Fatalf("OnChange calls = %v", changes)
	}
	// Reuse is rejected.
	if _, err := l.Redeem(ctx, "telegram", code, "200", ""); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("reuse err = %v", err)
	}
	if got, err := l.Lookup(ctx, "telegram", "100"); err != nil || got.UserID != alice.ID {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	if _, err := l.Lookup(ctx, "telegram", "200"); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("Lookup unlinked err = %v", err)
	}

	// Expiry.
	code2, _, _ := l.NewCode(ctx, bob.ID, "telegram")
	clk.add(DefaultCodeTTL + time.Second)
	if _, err := l.Redeem(ctx, "telegram", code2, "300", ""); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired code err = %v", err)
	}

	// A new code replaces the user's previous one.
	c1, _, _ := l.NewCode(ctx, bob.ID, "telegram")
	c2, _, _ := l.NewCode(ctx, bob.ID, "telegram")
	if _, err := l.Redeem(ctx, "telegram", c1, "300", ""); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("superseded code err = %v", err)
	}
	// An identity linked to alice cannot be taken over by bob's code.
	if _, err := l.Redeem(ctx, "telegram", c2, "100", ""); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("takeover err = %v", err)
	}
	// Relinking to the same user returns the existing link.
	c3, _, _ := l.NewCode(ctx, alice.ID, "telegram")
	if got, err := l.Redeem(ctx, "telegram", c3, "100", "x"); err != nil || got.DisplayName != "Alice" {
		t.Fatalf("relink same user = %+v, %v", got, err)
	}

	// Unlink is scoped to the owner.
	if err := l.Unlink(ctx, bob.ID, "telegram", "100"); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("unlink by other user err = %v", err)
	}
	if err := l.Unlink(ctx, alice.ID, "telegram", "100"); err != nil {
		t.Fatal(err)
	}
	if links, _ := l.Links(ctx, alice.ID, ""); len(links) != 0 {
		t.Fatalf("links after unlink = %v", links)
	}
}

func TestLinkerBruteForceLimit(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	alice, _ := st.CreateUser(ctx, "alice", "h", "member")
	clk := &clock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	l := NewLinker(st, LinkerOptions{Now: clk.now})
	code, _, _ := l.NewCode(ctx, alice.ID, "telegram")
	for i := range MaxFailedRedeems {
		if _, err := l.Redeem(ctx, "telegram", "ZZZZZ"+string(rune('2'+i)), "666", ""); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("attempt %d err = %v", i, err)
		}
	}
	if _, err := l.Redeem(ctx, "telegram", code, "666", ""); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("after failures err = %v", err)
	}
	// Other identities are unaffected; the window expires.
	clk.add(FailedRedeemWindow)
	code, _, _ = l.NewCode(ctx, alice.ID, "telegram")
	if _, err := l.Redeem(ctx, "telegram", code, "666", ""); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

type fakeChannel struct {
	name    string
	started bool
	closed  bool
}

func (f *fakeChannel) Name() string                    { return f.name }
func (f *fakeChannel) Start(ctx context.Context) error { f.started = true; return nil }
func (f *fakeChannel) Close() error                    { f.closed = true; return nil }
func (f *fakeChannel) CredentialKinds() []string       { return []string{f.name + "_token"} }
func (f *fakeChannel) Status() integration.Status {
	return integration.Status{State: integration.StatusDown, Message: "no token"}
}

func TestManager(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	alice, _ := st.CreateUser(ctx, "alice", "h", "member")
	m := NewManager(NewLinker(st, LinkerOptions{}), nil)
	fc := &fakeChannel{name: "telegram"}
	if err := m.Add(fc); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(&fakeChannel{name: "telegram"}); err == nil {
		t.Fatal("duplicate channel accepted")
	}
	m.Start(ctx)
	if !fc.started {
		t.Fatal("not started")
	}
	if _, err := m.NewLinkCode(ctx, alice.ID, "slack"); !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("unknown channel err = %v", err)
	}
	lc, err := m.NewLinkCode(ctx, alice.ID, "telegram")
	if err != nil || !strings.Contains(lc.Instructions, lc.Code) {
		t.Fatalf("NewLinkCode = %+v, %v", lc, err)
	}
	in := m.Integration()
	if mf := in.Manifest(); mf.Name != CredentialIntegration || mf.Scope != integration.ScopeShared ||
		len(mf.CredentialKinds) != 1 || mf.CredentialKinds[0] != "telegram_token" || len(in.Tools()) != 0 {
		t.Fatalf("manifest = %+v", mf)
	}
	if s := in.(integration.StatusReporter).Status(); s.State != integration.StatusDown || !strings.Contains(s.Message, "telegram: no token") {
		t.Fatalf("status = %+v", s)
	}
	if err := m.Close(); err != nil || !fc.closed {
		t.Fatalf("Close = %v, closed=%v", err, fc.closed)
	}
}
