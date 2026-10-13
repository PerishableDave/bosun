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

func TestMemoryCommands(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/memory", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "GET "+r.URL.RawQuery+" "+r.Header.Get("Authorization"))
		if r.URL.Query().Get("kind") == "fact" {
			fmt.Fprint(w, `{"memories":[]}`)
			return
		}
		fmt.Fprint(w, `{"memories":[{"id":7,"kind":"note","content":"Boat is\ncalled Sea Breeze.","source":"conv:conv_1",`+
			`"created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"}]}`)
	})
	mux.HandleFunc("POST /v1/memory", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Kind, Content string }
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, "POST "+body.Kind+" "+body.Content)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":8,"kind":%q,"content":%q,"source":"user","created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"}`, body.Kind, body.Content)
	})
	mux.HandleFunc("PUT /v1/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Content string }
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, "PUT "+r.PathValue("id")+" "+body.Content)
		fmt.Fprintf(w, `{"id":%s,"kind":"note","content":%q,"source":"user","created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"}`, r.PathValue("id"), body.Content)
	})
	mux.HandleFunc("DELETE /v1/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "DELETE "+r.PathValue("id"))
		if r.PathValue("id") == "99" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"memory not found"}}`)
			return
		}
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

	if out := cli("memory", "list"); !strings.Contains(out, "7   note") || !strings.Contains(out, "Boat is called Sea Breeze.") ||
		!strings.Contains(out, "conv:conv_1") {
		t.Errorf("memory list output:\n%s", out)
	}
	if out := cli("memory", "list", "--kind", "FACT", "--search", "boat name"); !strings.Contains(out, "No memories.") {
		t.Errorf("memory list --kind output:\n%s", out)
	}
	if out := cli("memory", "add", "preference", "Prefers", "metric", "units."); !strings.Contains(out, "Remembered 8 (preference): Prefers metric units.") {
		t.Errorf("memory add output:\n%s", out)
	}
	if out := cli("memory", "edit", "7", "Boat is called Wave Runner."); !strings.Contains(out, "Updated 7 (note): Boat is called Wave Runner.") {
		t.Errorf("memory edit output:\n%s", out)
	}
	if out := cli("memory", "forget", "7"); !strings.Contains(out, "Forgot 7") {
		t.Errorf("memory forget output:\n%s", out)
	}
	if out, err := run("memory", "forget", "99"); err == nil || !strings.Contains(err.Error(), "memory not found") {
		t.Errorf("forget missing = %v\n%s", err, out)
	}
	for _, bad := range [][]string{
		{"memory", "add", "secret", "x"}, {"memory", "add", "note"}, {"memory", "edit", "abc", "x"},
		{"memory", "forget", "0"}, {"memory", "list", "--kind", "bogus"},
	} {
		if _, err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	want := "GET  Bearer bsn_tok|GET kind=fact&q=boat+name Bearer bsn_tok|POST preference Prefers metric units.|" +
		"PUT 7 Boat is called Wave Runner.|DELETE 7|DELETE 99"
	if s := strings.Join(got, "|"); s != want {
		t.Errorf("requests =\n%q\nwant\n%q", s, want)
	}
}
