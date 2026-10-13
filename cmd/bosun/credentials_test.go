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
)

func TestCredentialsCommands(t *testing.T) {
	var mu sync.Mutex
	var got []string
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"integrations":[{"name":"email","scope":"per_user","status":"ok","tool_count":1},{"name":"mcp","scope":"shared","status":"ok","tool_count":2}]}`)
	})
	mux.HandleFunc("GET /v1/credentials", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"credentials":[{"integration":"email","kind":"apikey","scope":"user","created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"},`+
			`{"integration":"mcp","kind":"bearer:github","scope":"shared","expiry":"2027-01-01T00:00:00Z","created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"}]}`)
	})
	mux.HandleFunc("PUT /v1/credentials/{integration}/{kind}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Secret string
			Fields map[string]string
			Expiry string
		}
		json.NewDecoder(r.Body).Decode(&body)
		record(fmt.Sprintf("PUT %s/%s scope=%s secret=%q fields=%v expiry=%s", r.PathValue("integration"), r.PathValue("kind"),
			r.URL.Query().Get("scope"), body.Secret, body.Fields, body.Expiry))
		fmt.Fprintf(w, `{"integration":%q,"kind":%q,"scope":%q,"created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"}`,
			r.PathValue("integration"), r.PathValue("kind"), r.URL.Query().Get("scope"))
	})
	mux.HandleFunc("DELETE /v1/credentials/{integration}/{kind}", func(w http.ResponseWriter, r *http.Request) {
		record(fmt.Sprintf("DELETE %s/%s scope=%s", r.PathValue("integration"), r.PathValue("kind"), r.URL.Query().Get("scope")))
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
	cli := func(stdin string, args ...string) (string, error) {
		t.Helper()
		return runCLI(t, stdin, append([]string{"--config", cfgPath, "--server", srv.URL}, args...)...)
	}
	mustCLI := func(stdin string, args ...string) string {
		t.Helper()
		out, err := cli(stdin, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}

	out := mustCLI("", "credentials", "list")
	if !strings.Contains(out, "email        apikey") || !strings.Contains(out, "bearer:github  shared") {
		t.Errorf("list output:\n%s", out)
	}

	// Per-user, secret from stdin (one trailing newline dropped).
	out = mustCLI("s3cret\n", "credentials", "set", "email", "apikey", "--field", "username=dave", "--expires", "2027-01-01T00:00:00Z")
	if !strings.Contains(out, "Stored email/apikey (user)") || strings.Contains(out, "s3cret") {
		t.Errorf("set output:\n%s", out)
	}
	// Shared integration: --shared is implied, with a notice.
	out = mustCLI("ghp_token", "credentials", "set", "mcp", "bearer:github")
	if !strings.Contains(out, "mcp is a shared integration") || !strings.Contains(out, "restart bosund") {
		t.Errorf("shared set output:\n%s", out)
	}
	// Multi-line secrets survive.
	mustCLI("line1\nline2\n", "credentials", "set", "mcp", "bearer", "--shared")

	if _, err := cli("x", "credentials", "set", "email", "apikey", "--shared"); err == nil || !strings.Contains(err.Error(), "per-user") {
		t.Errorf("--shared on per-user integration: %v", err)
	}
	if _, err := cli("x", "credentials", "set", "nope", "apikey"); err == nil || !strings.Contains(err.Error(), "unknown integration") {
		t.Errorf("unknown integration: %v", err)
	}
	if _, err := cli("", "credentials", "set", "email", "apikey"); err == nil || !strings.Contains(err.Error(), "empty secret") {
		t.Errorf("empty secret: %v", err)
	}
	if _, err := cli("x", "credentials", "set", "email", "apikey", "--field", "novalue"); err == nil {
		t.Error("bad --field accepted")
	}

	mustCLI("", "credentials", "remove", "mcp", "bearer:github")
	mustCLI("", "credentials", "rm", "calendar", "apikey") // not enabled any more: still removable
	mustCLI("", "credentials", "remove", "email", "apikey")

	want := []string{
		`PUT email/apikey scope=user secret="s3cret" fields=map[username:dave] expiry=2027-01-01T00:00:00Z`,
		`PUT mcp/bearer:github scope=shared secret="ghp_token" fields=map[] expiry=`,
		`PUT mcp/bearer scope=shared secret="line1\nline2" fields=map[] expiry=`,
		`DELETE mcp/bearer:github scope=shared`,
		`DELETE calendar/apikey scope=user`,
		`DELETE email/apikey scope=user`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The secret flag does not exist.
	if _, err := cli("", "credentials", "set", "email", "apikey", "--secret", "x"); err == nil {
		t.Error("--secret flag accepted")
	}
}
