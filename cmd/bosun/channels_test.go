package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestChannelsCommands(t *testing.T) {
	var mu sync.Mutex
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/channels/{channel}/link", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("channel") != "telegram" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"channel \"slack\" is not enabled"}}`)
			return
		}
		w.WriteHeader(201)
		fmt.Fprint(w, `{"channel":"telegram","code":"ABC234","expires_at":"2026-10-13T10:10:00Z","instructions":"Send /link ABC234 to @bosun_bot on Telegram."}`)
	})
	mux.HandleFunc("GET /v1/channels", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"channels":["telegram"],"links":[{"channel":"telegram","external_id":"123456789","display_name":"Alice (@alice)","created_at":"2026-10-13T10:00:00Z"}]}`)
	})
	mux.HandleFunc("DELETE /v1/channels/{channel}/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, "DELETE "+r.PathValue("channel")+" "+r.PathValue("id"))
		mu.Unlock()
		w.WriteHeader(204)
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
	run := func(args ...string) (string, error) {
		return runCLI(t, "", append([]string{"--config", cfgPath, "--server", srv.URL}, args...)...)
	}

	out, err := run("channels", "link", "telegram")
	if err != nil || !strings.Contains(out, "Link code: ABC234") || !strings.Contains(out, "@bosun_bot") {
		t.Fatalf("link: %v\n%s", err, out)
	}
	if _, err := run("channels", "link", "slack"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("link slack err = %v", err)
	}
	out, err = run("channels", "list")
	if err != nil || !strings.Contains(out, "Enabled channels: telegram") || !strings.Contains(out, "123456789") || !strings.Contains(out, "Alice (@alice)") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = run("channels", "unlink", "telegram", "123456789")
	if err != nil || !strings.Contains(out, "Unlinked telegram chat 123456789") {
		t.Fatalf("unlink: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0] != "DELETE telegram 123456789" {
		t.Fatalf("calls = %v", got)
	}
}
