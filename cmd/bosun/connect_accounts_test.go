package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/vault"
)

// fakeDaemon answers the credential test and store endpoints. Email
// passwords "good" and "abcdefghijklmnop" work; calendar passwords work
// when they also work for email; any https feed works.
type fakeDaemon struct {
	srv *httptest.Server
	mu  sync.Mutex
	log []string
}

func fieldsString(f map[string]string) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+f[k])
	}
	return strings.Join(parts, " ")
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	d := &fakeDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/integrations/{name}/test", func(w http.ResponseWriter, r *http.Request) {
		var req api.TestCredentialRequest
		json.NewDecoder(r.Body).Decode(&req)
		name := r.PathValue("name")
		d.record(fmt.Sprintf("TEST %s/%s %s [%s]", name, req.Kind, req.Secret, fieldsString(req.Fields)))
		ok := false
		switch {
		case strings.HasPrefix(req.Kind, "ics"):
			ok = strings.HasPrefix(req.Secret, "https://")
		default:
			ok = req.Secret == "good" || req.Secret == "abcdefghijklmnop"
		}
		if ok {
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		json.NewEncoder(w).Encode(api.TestCredentialResponse{Error: "wrong password or login refused: imap.gmail.com rejected the login. Gmail only accepts an app password here, which needs 2-Step Verification"})
	})
	mux.HandleFunc("PUT /v1/credentials/{integration}/{kind}", func(w http.ResponseWriter, r *http.Request) {
		var req api.PutCredentialRequest
		json.NewDecoder(r.Body).Decode(&req)
		d.record(fmt.Sprintf("PUT %s/%s scope=%s %s [%s]", r.PathValue("integration"), r.PathValue("kind"),
			r.URL.Query().Get("scope"), req.Secret, fieldsString(req.Fields)))
		fmt.Fprintf(w, `{"integration":%q,"kind":%q,"scope":"user","created_at":"2026-10-13T00:00:00Z","updated_at":"2026-10-13T00:00:00Z"}`,
			r.PathValue("integration"), r.PathValue("kind"))
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) record(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, s)
}

func (d *fakeDaemon) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

// clientEnv writes a CLI config and token pointing at d.
func clientEnv(t *testing.T, d *fakeDaemon) func(stdin string, args ...string) (string, error) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bosun.yaml")
	if err := os.WriteFile(cfgPath, fmt.Appendf(nil, "daemon: { data_dir: %q }\n", dir), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli-token"), []byte("bsn_tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(stdin string, args ...string) (string, error) {
		return runCLI(t, stdin, append([]string{"--config", cfgPath, "--server", d.srv.URL}, args...)...)
	}
}

func wantRequests(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func wantOutput(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestConnectEmailGmail(t *testing.T) {
	d := newFakeDaemon(t)
	cli := clientEnv(t, d)
	const feed = "https://calendar.google.com/calendar/ical/dave%40gmail.com/private-xyz/basic.ics"
	out, err := cli("dave@gmail.com\nwrong\ny\nabcd efgh ijkl mnop\ny\n"+feed+"\n\n", "connect", "email")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantOutput(t, out,
		"Provider: Gmail (IMAP imap.gmail.com:993, SMTP smtp.gmail.com:465 TLS)",
		"https://myaccount.google.com/apppasswords", "2-Step Verification",
		"That did not work: wrong password", "Try another password? [Y/n]",
		"Connection OK.", "Saved. Bosun can now read and send mail as dave@gmail.com",
		"Google Calendar does not accept app passwords", "bosun connect google --scopes caldav",
		"Add a read-only calendar from its private iCal (ICS) link now? [Y/n]",
		"Secret address in iCal format", "Name for this calendar (e.g. family, work) [google]",
		`Saved read-only calendar "google" (credential calendar/ics:google)`)
	if strings.Contains(out, "abcd efgh") || strings.Contains(out, "private-xyz") {
		t.Errorf("secret echoed:\n%s", out)
	}
	f := "[host=imap.gmail.com provider=gmail user=dave@gmail.com]"
	wantRequests(t, d.requests(),
		"TEST email/password wrong "+f,
		"TEST email/password abcdefghijklmnop "+f, // spaces removed
		"PUT email/password scope=user abcdefghijklmnop "+f,
		"TEST calendar/ics:google "+feed+" []",
		"PUT calendar/ics:google scope=user "+feed+" []")
}

func TestConnectEmailICloudOffersCalendar(t *testing.T) {
	d := newFakeDaemon(t)
	cli := clientEnv(t, d)
	out, err := cli("me@icloud.com\ngood\n\n", "connect", "email")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantOutput(t, out, "Provider: iCloud Mail (IMAP imap.mail.me.com:993, SMTP smtp.mail.me.com:587 STARTTLS)",
		"https://appleid.apple.com", "App-Specific Passwords",
		"iCloud Calendar works with the same app password. Set up your calendar too? [Y/n]",
		"Testing CalDAV at https://caldav.icloud.com", "add or change events")
	f := "[host=imap.mail.me.com provider=icloud smtp_port=587 user=me@icloud.com]"
	wantRequests(t, d.requests(),
		"TEST email/password good "+f,
		"PUT email/password scope=user good "+f,
		"TEST calendar/password good [url=https://caldav.icloud.com user=me@icloud.com]",
		"PUT calendar/password scope=user good [url=https://caldav.icloud.com user=me@icloud.com]")
}

func TestConnectEmailUnknownDomainAsksHosts(t *testing.T) {
	d := newFakeDaemon(t)
	cli := clientEnv(t, d)
	old := discoverDial
	defer func() { discoverDial = old }()
	discoverDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "mail.example.org:993" || addr == "mail.example.org:587" {
			a, b := net.Pipe()
			b.Close()
			return a, nil
		}
		return nil, errors.New("refused")
	}
	out, err := cli("me@example.org\n\nsmtp.relay.example.org\ngood\nn\n", "connect", "email")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantOutput(t, out, "example.org is not a provider I know; looking for its mail servers",
		"Found IMAP mail.example.org and SMTP mail.example.org.",
		"IMAP server [mail.example.org]:", "SMTP server [mail.example.org]:",
		"Add a read-only calendar from its private iCal (ICS) link now? [y/N]", "Skipped.")
	f := "[host=mail.example.org smtp_host=smtp.relay.example.org smtp_port=587 user=me@example.org]"
	wantRequests(t, d.requests(), "TEST email/password good "+f, "PUT email/password scope=user good "+f)
}

func TestConnectEmailFlags(t *testing.T) {
	d := newFakeDaemon(t)
	cli := clientEnv(t, d)
	out, err := cli("good\n", "connect", "email", "--user", "me@fastmail.com", "--password-stdin")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "?") || strings.Contains(out, "good") {
		t.Errorf("non-interactive run asked or echoed:\n%s", out)
	}
	out, err = cli("good", "connect", "email", "--user", "me@example.org", "--password-stdin", "--provider", "other",
		"--host", "imap.example.org", "--smtp-host", "out.example.org", "--imap-port", "143", "--smtp-port", "465")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := cli("good\n", "connect", "email", "--user", "me@example.org", "--password-stdin"); err == nil ||
		!strings.Contains(err.Error(), "--host") {
		t.Errorf("unknown domain without hosts: %v", err)
	}
	if _, err := cli("bad\n", "connect", "email", "--user", "me@yahoo.com", "--password-stdin"); err == nil ||
		!strings.Contains(err.Error(), "not saved") {
		t.Errorf("failing test: %v", err)
	}
	if _, err := cli("good\n", "connect", "email", "--user", "me@x.org", "--password-stdin", "--provider", "aol"); err == nil {
		t.Error("unknown --provider accepted")
	}
	if _, err := cli("", "connect", "email", "--password-stdin"); err == nil || !strings.Contains(err.Error(), "--user") {
		t.Errorf("missing --user: %v", err)
	}
	wantRequests(t, d.requests(),
		"TEST email/password good [host=imap.fastmail.com provider=fastmail user=me@fastmail.com]",
		"PUT email/password scope=user good [host=imap.fastmail.com provider=fastmail user=me@fastmail.com]",
		"TEST email/password good [host=imap.example.org imap_port=143 smtp_host=out.example.org smtp_port=465 user=me@example.org]",
		"PUT email/password scope=user good [host=imap.example.org imap_port=143 smtp_host=out.example.org smtp_port=465 user=me@example.org]",
		"TEST email/password bad [host=imap.mail.yahoo.com provider=yahoo user=me@yahoo.com]")
}

func TestConnectCalendarICS(t *testing.T) {
	d := newFakeDaemon(t)
	cli := clientEnv(t, d)
	if out, err := cli("", "connect", "calendar", "--ics", "webcal://example.com/family.ics", "--name", "family"); err != nil ||
		!strings.Contains(out, `Saved read-only calendar "family"`) {
		t.Fatalf("--ics URL: %v\n%s", err, out)
	}
	// URL omitted: hidden prompt (read from stdin here); no name: kind "ics".
	out, err := cli("https://example.com/work.ics\n\n", "connect", "calendar", "--ics")
	if err != nil {
		t.Fatalf("prompt: %v\n%s", err, out)
	}
	wantOutput(t, out, "Calendar link (input hidden)", "works like a password", `(credential calendar/ics)`)
	if strings.Contains(out, "work.ics") {
		t.Errorf("URL echoed:\n%s", out)
	}
	if _, err := cli("", "connect", "calendar", "--ics=http://example.com/a.ics", "--name", "x"); err == nil {
		t.Error("plain http accepted")
	}
	if _, err := cli("", "connect", "calendar", "--ics=https://example.com/a.ics", "--name", "Bad Name"); err == nil {
		t.Error("bad name accepted")
	}
	wantRequests(t, d.requests(),
		"TEST calendar/ics:family https://example.com/family.ics []",
		"PUT calendar/ics:family scope=user https://example.com/family.ics []",
		"TEST calendar/ics https://example.com/work.ics []",
		"PUT calendar/ics scope=user https://example.com/work.ics []")
}

func TestSetupConnectEmail(t *testing.T) {
	oldKC, oldTester := detectKeychain, localTester
	defer func() { detectKeychain, localTester = oldKC, oldTester }()
	detectKeychain = func() vault.Keychain { return nil }
	var tested []string
	localTester = func(_ context.Context, integ string, c integration.Credential) error {
		tested = append(tested, integ+"/"+c.Kind+" "+fieldsString(c.Fields))
		if c.Secret != "fm-pass" {
			return fmt.Errorf("login rejected for %s", c.Secret)
		}
		return nil
	}

	setup := func(stdin string) (string, *config.Config) {
		t.Helper()
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "bosun.yaml")
		out, err := runCLI(t, stdin, "--config", cfgPath, "setup", "--data-dir", filepath.Join(dir, "data"), "--listen", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out)
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return out, cfg
	}

	// Declined: nothing tested, the vault is not touched.
	out, cfg := setup("dave\nlongenough\nlongenough\nn\nn\n")
	wantOutput(t, out, "Connect an email account now?", "Skipped. Later: `bosun connect email`.",
		"bosun connect google --scopes caldav")
	if len(tested) != 0 {
		t.Errorf("declined but tested %v", tested)
	}
	for _, name := range []string{"email", "calendar", "websearch"} {
		if ic, ok := cfg.Integrations[name]; !ok || !ic.IsEnabled() {
			t.Errorf("setup should enable %s: %+v", name, cfg.Integrations)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Daemon.DataDir, "master.key")); err == nil {
		t.Error("declining created a master key")
	}

	// Accepted: Fastmail, a wrong password first, then calendar too.
	out, cfg = setup("dave\nlongenough\nlongenough\ny\nme@fastmail.com\nwrong\ny\nfm-pass\ny\n")
	wantOutput(t, out, "Provider: Fastmail", "That did not work: login rejected for [redacted]",
		"Saved. Bosun can now read and send mail as me@fastmail.com", "Fastmail Calendar works with the same app password")
	if strings.Contains(out, "fm-pass") {
		t.Errorf("password echoed:\n%s", out)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := auth.New(st, auth.Options{})
	if _, err := a.LoginWithLabel(ctx, "dave", "longenough", "t"); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByUsername(ctx, "dave")
	if err != nil {
		t.Fatal(err)
	}
	v := vault.New(st, vault.NewFileKeySource(cfg.Daemon.DataDir))
	mail, err := v.Get(ctx, &u.ID, "email", "password")
	if err != nil || mail.Secret != "fm-pass" || mail.Fields["host"] != "imap.fastmail.com" || mail.Fields["user"] != "me@fastmail.com" {
		t.Errorf("email credential = %+v, %v", mail, err)
	}
	cal, err := v.Get(ctx, &u.ID, "calendar", "password")
	if err != nil || cal.Secret != "fm-pass" || cal.Fields["url"] != "https://caldav.fastmail.com" {
		t.Errorf("calendar credential = %+v, %v", cal, err)
	}
	want := []string{
		"email/password host=imap.fastmail.com provider=fastmail user=me@fastmail.com",
		"email/password host=imap.fastmail.com provider=fastmail user=me@fastmail.com",
		"calendar/password url=https://caldav.fastmail.com user=me@fastmail.com",
	}
	if strings.Join(tested, "\n") != strings.Join(want, "\n") {
		t.Errorf("tested:\n%s", strings.Join(tested, "\n"))
	}
}
