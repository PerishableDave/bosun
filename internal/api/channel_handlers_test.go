package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/channels"
	"github.com/dperedo/bosun/internal/store"
)

type nopChannel struct{}

func (nopChannel) Name() string                     { return "telegram" }
func (nopChannel) Start(context.Context) error      { return nil }
func (nopChannel) Close() error                     { return nil }
func (nopChannel) LinkInstructions(c string) string { return "Send /link " + c + " to @testbot" }

func TestChannelEndpoints(t *testing.T) {
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
	tok := func(name string) (string, int64) {
		u, err := a.CreateUser(ctx, name, "longenough", auth.RoleMember)
		if err != nil {
			t.Fatal(err)
		}
		s, err := a.LoginWithLabel(ctx, name, "longenough", "test")
		if err != nil {
			t.Fatal(err)
		}
		return s.Token, u.ID
	}
	alice, aliceID := tok("alice")
	bob, _ := tok("bob")
	linker := channels.NewLinker(st, channels.LinkerOptions{})
	mgr := channels.NewManager(linker, nil)
	if err := mgr.Add(nopChannel{}); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Auth: a, Bus: events.NewBus(), Channels: mgr})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{srv: srv, auth: a}

	if c := e.do(t, "POST", "/v1/channels/telegram/link", "", "").StatusCode; c != 401 {
		t.Fatalf("unauthenticated = %d", c)
	}
	if c := e.do(t, "POST", "/v1/channels/slack/link", alice, "").StatusCode; c != 404 {
		t.Fatalf("unknown channel = %d", c)
	}
	resp := e.do(t, "POST", "/v1/channels/telegram/link", alice, "")
	var lc ChannelLinkCodeResponse
	if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&lc) != nil {
		t.Fatalf("link code = %d", resp.StatusCode)
	}
	if len(lc.Code) != channels.CodeLength || lc.Channel != "telegram" || lc.ExpiresAt.IsZero() ||
		lc.Instructions != "Send /link "+lc.Code+" to @testbot" {
		t.Fatalf("link code = %+v", lc)
	}
	if _, err := linker.Redeem(ctx, "telegram", lc.Code, "4242", "Alice (@alice)"); err != nil {
		t.Fatal(err)
	}

	var list ChannelListResponse
	resp = e.do(t, "GET", "/v1/channels", alice, "")
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&list) != nil {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	if len(list.Channels) != 1 || list.Channels[0] != "telegram" || len(list.Links) != 1 ||
		list.Links[0].ExternalID != "4242" || list.Links[0].DisplayName != "Alice (@alice)" {
		t.Fatalf("list = %+v", list)
	}
	resp = e.do(t, "GET", "/v1/channels", bob, "")
	list = ChannelListResponse{}
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Links) != 0 {
		t.Fatalf("bob sees alice's links: %+v", list)
	}
	if c := e.do(t, "DELETE", "/v1/channels/telegram/4242", bob, "").StatusCode; c != 404 {
		t.Fatalf("bob unlinking alice = %d", c)
	}
	if c := e.do(t, "DELETE", "/v1/channels/telegram/4242", alice, "").StatusCode; c != 204 {
		t.Fatalf("unlink = %d", c)
	}
	if links, _ := linker.Links(ctx, aliceID, ""); len(links) != 0 {
		t.Fatalf("still linked: %+v", links)
	}
	if c := e.do(t, "DELETE", "/v1/channels/telegram/4242", alice, "").StatusCode; c != 404 {
		t.Fatalf("second unlink = %d", c)
	}

	// Without channels configured the list is empty and linking is 404.
	s2, _ := New(Options{Auth: a, Bus: events.NewBus()})
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	e2 := &env{srv: srv2, auth: a}
	resp = e2.do(t, "GET", "/v1/channels", alice, "")
	var empty map[string]any
	json.NewDecoder(resp.Body).Decode(&empty)
	if b, _ := json.Marshal(empty); !strings.Contains(string(b), `"channels":[]`) {
		t.Fatalf("empty list = %s", b)
	}
	if c := e2.do(t, "POST", "/v1/channels/telegram/link", alice, "").StatusCode; c != 404 {
		t.Fatalf("link without channels = %d", c)
	}
}
