package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConversationsCommands(t *testing.T) {
	var got []string
	conv := func(id, title string, extra string) string {
		return fmt.Sprintf(`{"id":%q,"title":%q,"title_source":"user","source":"chat","pinned":false,"archived":false,"unread":false,`+
			`"message_count":2,"last_message_at":"2026-10-12T22:00:00Z","last_message_preview":"hi","created_at":"2026-10-12T21:00:00Z","updated_at":"2026-10-12T22:00:00Z"%s}`, id, title, extra)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "GET "+r.URL.RawQuery+" "+r.Header.Get("Authorization"))
		if r.URL.Query().Get("archived") == "true" {
			fmt.Fprint(w, `{"conversations":[],"next_cursor":null}`)
			return
		}
		fmt.Fprintf(w, `{"conversations":[%s,%s],"next_cursor":"abc"}`,
			conv("conv_1", "Lisbon trip", `,"pinned":true,"unread":true`), conv("conv_2", "", `,"source":"routine","routine_name":"brief"`))
	})
	mux.HandleFunc("PATCH /v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		got = append(got, "PATCH "+r.PathValue("id")+" "+string(b))
		title := "Lisbon trip"
		if t, ok := body["title"].(string); ok {
			title = t
		}
		fmt.Fprint(w, conv(r.PathValue("id"), title, ""))
	})
	mux.HandleFunc("DELETE /v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "DELETE "+r.PathValue("id"))
		if r.PathValue("id") == "conv_busy" {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":{"code":"conflict","message":"a turn is running in this conversation; stop it first"}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/conversations/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "EXPORT "+r.PathValue("id")+" "+r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/markdown")
		fmt.Fprint(w, "# Lisbon trip\n")
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
	cli := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}

	out := cli("conversations", "list")
	for _, want := range []string{"conv_1", "Lisbon trip", "pinned,unread", "conv_2", "(untitled)", "routine", "(more:"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
	if out := cli("conversations", "list", "--archived", "true", "-q", "lisbon"); !strings.Contains(out, "No conversations.") {
		t.Errorf("archived list:\n%s", out)
	}
	if out := cli("conversations", "rename", "conv_1", "Porto", "trip"); !strings.Contains(out, `conv_1: renamed to "Porto trip"`) {
		t.Errorf("rename: %s", out)
	}
	cli("conversations", "pin", "conv_1")
	cli("conversations", "unpin", "conv_1")
	cli("conversations", "archive", "conv_1")
	cli("conversations", "unarchive", "conv_1")
	if out := cli("conversations", "delete", "conv_2"); !strings.Contains(out, "conv_2: deleted") {
		t.Errorf("delete: %s", out)
	}
	if _, err := run("conversations", "delete", "conv_busy"); err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("busy delete: %v", err)
	}
	if out := cli("conversations", "export", "conv_1"); out != "# Lisbon trip\n" {
		t.Errorf("export: %q", out)
	}
	p := filepath.Join(dir, "out.json")
	cli("conversations", "export", "conv_1", "--format", "json", "-o", p)
	if b, _ := os.ReadFile(p); string(b) != "# Lisbon trip\n" {
		t.Errorf("export file = %q", b)
	}
	if _, err := run("conversations", "export", "conv_1", "--format", "pdf"); err == nil {
		t.Error("bad format accepted")
	}
	if _, err := run("conversations", "pin"); err == nil {
		t.Error("pin without id accepted")
	}

	want := []string{
		"GET archived=false&limit=50 Bearer bsn_tok",
		"GET archived=true&limit=50&q=lisbon Bearer bsn_tok",
		`PATCH conv_1 {"title":"Porto trip"}`,
		`PATCH conv_1 {"pinned":true}`,
		`PATCH conv_1 {"pinned":false}`,
		`PATCH conv_1 {"archived":true}`,
		`PATCH conv_1 {"archived":false}`,
		"DELETE conv_2",
		"DELETE conv_busy",
		"EXPORT conv_1 format=markdown",
		"EXPORT conv_1 format=json",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
