package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/store"
)

var inviteRe = regexp.MustCompile(`(?m)^\s+(bsi_[a-z0-9]+)$`)

func TestUsersCommands(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(st, auth.Options{Argon2: &auth.Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateUser(ctx, "dave", "longenough", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	s, _ := api.New(api.Options{Auth: a, Bus: events.NewBus(), Version: "t", Users: a})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Separate CLI homes for the admin and the new member.
	home := func() string {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "bosun.yaml")
		if err := os.WriteFile(cfg, fmt.Appendf(nil, "daemon: { data_dir: %q }\n", dir), 0o600); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	adminCfg, aliceCfg := home(), home()
	as := func(cfg, stdin string, args ...string) (string, error) {
		t.Helper()
		return runCLI(t, stdin, append([]string{"--config", cfg, "--server", srv.URL}, args...)...)
	}
	must := func(out string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	must(as(adminCfg, "longenough\n", "login", "-u", "dave"))

	out := must(as(adminCfg, "", "users", "add", "alice"))
	m := inviteRe.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "Created member alice") || !strings.Contains(out, "bosun accept-invite "+m[1]) {
		t.Fatalf("users add output:\n%s", out)
	}
	if strings.Contains(out, "--server") {
		t.Errorf("loopback daemon: command should not need --server:\n%s", out)
	}
	if out := must(as(adminCfg, "", "users", "list")); !regexp.MustCompile(`alice\s+member\s+invited`).MatchString(out) {
		t.Errorf("list after add:\n%s", out)
	}

	// Mismatched confirmation, then a short password, then success.
	if _, err := as(aliceCfg, "alicepass1\nnope-nope\n", "accept-invite", m[1]); err == nil {
		t.Error("mismatched passwords accepted")
	}
	out = must(as(aliceCfg, "short\nalicepass1\nalicepass1\n", "accept-invite", m[1]))
	if !strings.Contains(out, "Welcome, alice") || !strings.Contains(out, "at least 8") {
		t.Errorf("accept-invite output:\n%s", out)
	}
	if out := must(as(aliceCfg, "", "status")); !strings.Contains(out, "logged in as alice (member)") {
		t.Errorf("member status:\n%s", out)
	}
	if _, err := as(aliceCfg, "alicepass2\nalicepass2\n", "accept-invite", m[1]); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("reused invite: %v", err)
	}
	if _, err := as(aliceCfg, "", "users", "list"); err == nil || !strings.Contains(err.Error(), "admin only") {
		t.Errorf("member users list: %v", err)
	}

	// passwd keeps the current session.
	if _, err := as(aliceCfg, "wrong-pass\nalicepass2\nalicepass2\n", "passwd"); err == nil {
		t.Error("passwd with wrong current password succeeded")
	}
	must(as(aliceCfg, "alicepass1\nalicepass2\nalicepass2\n", "passwd"))
	if out := must(as(aliceCfg, "", "status")); !strings.Contains(out, "logged in as alice") {
		t.Errorf("status after passwd:\n%s", out)
	}

	// Admin: disable, enable, reset-password, remove.
	if out := must(as(adminCfg, "", "users", "disable", "alice")); !strings.Contains(out, "alice is now disabled") {
		t.Errorf("disable:\n%s", out)
	}
	if out := must(as(aliceCfg, "", "status")); !strings.Contains(out, "not valid") {
		t.Errorf("disabled member status:\n%s", out)
	}
	must(as(adminCfg, "", "users", "enable", "alice"))
	if out := must(as(adminCfg, "", "users", "set-role", "alice", "admin")); !strings.Contains(out, "alice is now admin") {
		t.Errorf("set-role:\n%s", out)
	}
	must(as(adminCfg, "", "users", "set-role", "alice", "member"))
	out = must(as(adminCfg, "", "users", "reset-password", "alice"))
	m2 := inviteRe.FindStringSubmatch(out)
	if m2 == nil || m2[1] == m[1] {
		t.Fatalf("reset-password output:\n%s", out)
	}
	must(as(aliceCfg, "alicepass3\nalicepass3\n", "accept-invite", "  "+strings.ToUpper(m2[1])+" "))
	if _, err := as(adminCfg, "dave\n", "users", "remove", "dave"); err == nil || !strings.Contains(err.Error(), "last") {
		t.Errorf("remove last admin: %v", err)
	}
	if _, err := as(adminCfg, "bob\n", "users", "remove", "alice"); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Errorf("remove without confirmation: %v", err)
	}
	if out := must(as(adminCfg, "alice\n", "users", "remove", "alice")); !strings.Contains(out, "Removed alice") {
		t.Errorf("remove:\n%s", out)
	}
	if _, err := as(adminCfg, "", "users", "disable", "alice"); err == nil || !strings.Contains(err.Error(), "no user named") {
		t.Errorf("disable removed user: %v", err)
	}

	// --password-stdin creates a user directly.
	if out := must(as(adminCfg, "bobspass1\n", "users", "add", "bob", "--role", "admin", "--password-stdin")); !strings.Contains(out, "Created admin bob") || inviteRe.MatchString(out) {
		t.Errorf("add with password:\n%s", out)
	}
	if _, err := as(adminCfg, "", "users", "add", "carol", "--role", "root"); err == nil {
		t.Error("bad role accepted")
	}
}

func TestAcceptInviteCommandLine(t *testing.T) {
	g := &globals{configPath: filepath.Join(t.TempDir(), "none.yaml")}
	if got := acceptInviteCommand(g, "http://127.0.0.1:7777", "bsi_x"); got != "bosun accept-invite bsi_x" {
		t.Errorf("loopback = %q", got)
	}
	if got := acceptInviteCommand(g, "http://localhost:7777", "bsi_x"); got != "bosun accept-invite bsi_x" {
		t.Errorf("localhost = %q", got)
	}
	g.fingerprint = "AA:BB"
	if got := acceptInviteCommand(g, "https://host.lan:7777", "bsi_x"); got != "bosun accept-invite bsi_x --server https://host.lan:7777 --insecure-fingerprint AA:BB" {
		t.Errorf("lan = %q", got)
	}
}
