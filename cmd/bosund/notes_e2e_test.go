package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/fake"
)

// TestEndToEndNotes runs the notes integration in per-user mode through the
// daemon: notes live under <dir>/<username>/ and each user sees only theirs.
func TestEndToEndNotes(t *testing.T) {
	dir := t.TempDir()
	notesDir := filepath.Join(dir, "Notes")
	cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q }
llm:
  default: fake/test-model
  providers: { fake: {} }
memory: { enabled: false }
integrations:
  notes: { enabled: true, dir: %q }
`, filepath.Join(dir, "data"), notesDir))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, llm: fake.New("fake")}
	reg := llm.NewRegistry()
	if err := reg.Register(h.llm); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.d, err = newDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), deps{LLM: reg, Argon2: cheapArgon()})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if h.alice, err = h.d.auth.CreateUser(ctx, "alice", "alice-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if h.bob, err = h.d.auth.CreateUser(ctx, "bob", "bob-password", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.base = "http://" + ln.Addr().String()
	served := make(chan error, 1)
	go func() { served <- h.d.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		h.d.Close()
	})

	if err := os.MkdirAll(filepath.Join(notesDir, "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notesDir, "alice", "plan.md"), []byte("alice's plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	alice := h.login("alice", "alice-password")
	bob := h.login("bob", "bob-password")

	var il api.IntegrationListResponse
	if c := h.do("GET", "/v1/integrations", alice, nil, &il); c != 200 || len(il.Integrations) != 1 ||
		il.Integrations[0].Name != "notes" || il.Integrations[0].Scope != "per_user" || il.Integrations[0].ToolCount != 6 {
		t.Fatalf("integrations = %d %+v", c, il)
	}

	h.llm.Push(fake.ToolCall("r1", "notes__read", `{"path":"plan.md"}`), fake.Text("done"))
	h.turn(alice, h.newConversation(alice), "read my plan", nil)
	reqs := h.llm.Requests()
	if tr := lastToolResult(t, reqs[len(reqs)-1]); tr.IsError || tr.Content != "alice's plan" {
		t.Errorf("alice read = %+v (per-user notes are trusted: no data wrapper expected)", tr)
	}

	h.llm.Push(fake.ToolCall("l1", "notes__list", `{}`), fake.Text("done"))
	h.turn(bob, h.newConversation(bob), "list my notes", nil)
	reqs = h.llm.Requests()
	if tr := lastToolResult(t, reqs[len(reqs)-1]); tr.IsError || strings.Contains(tr.Content, "plan.md") {
		t.Errorf("bob list = %+v", tr)
	}
	if _, err := os.Stat(filepath.Join(notesDir, "bob")); err != nil {
		t.Errorf("bob's folder not created by username: %v", err)
	}
}
