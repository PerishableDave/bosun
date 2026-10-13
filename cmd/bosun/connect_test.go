package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConnectCommands(t *testing.T) {
	var mu sync.Mutex
	var got []string
	polls := 0
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/{provider}/start", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Scopes []string }
		json.NewDecoder(r.Body).Decode(&body)
		record(fmt.Sprintf("start %s %v", r.PathValue("provider"), body.Scopes))
		fmt.Fprintf(w, `{"provider":"google","state":"st-1","auth_url":"https://idp.example/auth?x=1","redirect_url":"http://127.0.0.1:7777/oauth/callback","scopes":["openid"],"expires_at":%q}`,
			time.Now().Add(10*time.Minute).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /v1/oauth/{provider}/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		if r.URL.Query().Get("state") != "st-1" {
			http.Error(w, "bad state", 404)
			return
		}
		if n < 2 {
			fmt.Fprint(w, `{"status":"pending"}`)
			return
		}
		fmt.Fprint(w, `{"status":"complete","account":{"provider":"google","email":"dave@example.com","scopes":["openid","email"],"expiry":"2026-10-13T12:00:00Z","linked_at":"2026-10-13T11:00:00Z","updated_at":"2026-10-13T11:00:00Z"}}`)
	})
	mux.HandleFunc("GET /v1/oauth", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accounts":[{"provider":"google","email":"dave@example.com","scopes":["openid","email"],"expiry":"2026-10-13T12:00:00Z","linked_at":"2026-10-13T11:00:00Z","updated_at":"2026-10-13T11:00:00Z"}],"providers":[{"name":"google","configured":true}]}`)
	})
	mux.HandleFunc("DELETE /v1/oauth/{provider}", func(w http.ResponseWriter, r *http.Request) {
		record("delete " + r.PathValue("provider"))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bosun.yaml")
	if err := os.WriteFile(cfgPath, fmt.Appendf(nil, "daemon: { data_dir: %q }\n", dir), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli-token"), []byte("bsn_tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var opened []string
	oldOpen, oldPoll := openBrowser, connectPollInterval
	openBrowser = func(u string) error { opened = append(opened, u); return nil }
	connectPollInterval = 10 * time.Millisecond
	defer func() { openBrowser, connectPollInterval = oldOpen, oldPoll }()

	cli := func(args ...string) string {
		t.Helper()
		out, err := runCLI(t, "", append([]string{"--config", cfgPath, "--server", srv.URL}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	out := cli("connect", "google", "--scopes", "email, calendar")
	if !strings.Contains(out, "https://idp.example/auth?x=1") || !strings.Contains(out, "Connected google account dave@example.com") {
		t.Errorf("connect output:\n%s", out)
	}
	if len(opened) != 1 || opened[0] != "https://idp.example/auth?x=1" {
		t.Errorf("opened %v", opened)
	}
	cli("connect", "google", "--no-browser")
	if len(opened) != 1 {
		t.Errorf("--no-browser opened %v", opened)
	}
	out = cli("connect", "list")
	if !strings.Contains(out, "google") || !strings.Contains(out, "dave@example.com") {
		t.Errorf("list output:\n%s", out)
	}
	cli("connect", "remove", "google")
	want := "start google [email calendar]\nstart google []\ndelete google"
	if strings.Join(got, "\n") != want {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
}
