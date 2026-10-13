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

func TestToolsAndPolicyCommands(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tools", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tools":[{"name":"websearch.fetch","integration":"websearch","description":"Fetch a page","capability":"websearch.fetch","risk":"read","decision":"allow"}]}`)
	})
	mux.HandleFunc("GET /v1/policy", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"defaults":{"read":"allow","write":"ask","destructive":"ask"},"rules":[{"capability":"websearch.search","decision":"deny","updated_at":"2026-10-12T22:00:00Z"}]}`)
	})
	mux.HandleFunc("PUT /v1/policy/{cap}", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Decision string }
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, "PUT "+r.PathValue("cap")+" "+body.Decision+" "+r.Header.Get("Authorization"))
		fmt.Fprintf(w, `{"capability":%q,"decision":%q,"updated_at":"2026-10-12T22:00:00Z"}`, r.PathValue("cap"), body.Decision)
	})
	mux.HandleFunc("DELETE /v1/policy/{cap}", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "DELETE "+r.PathValue("cap"))
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
	cli := func(args ...string) string {
		t.Helper()
		out, err := runCLI(t, "", append([]string{"--config", cfgPath, "--server", srv.URL}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	if out := cli("tools"); !strings.Contains(out, "websearch.fetch") || !strings.Contains(out, "allow") {
		t.Errorf("tools output:\n%s", out)
	}
	if out := cli("policy", "list"); !strings.Contains(out, "write=ask") || !strings.Contains(out, "websearch.search  deny") {
		t.Errorf("policy list output:\n%s", out)
	}
	if out := cli("policy", "set", "websearch.fetch", "ASK"); !strings.Contains(out, "websearch.fetch: ask") {
		t.Errorf("policy set output:\n%s", out)
	}
	cli("policy", "unset", "websearch.fetch")
	if _, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "policy", "set", "x.y", "maybe"); err == nil {
		t.Error("invalid decision accepted")
	}
	if want := "PUT websearch.fetch ask Bearer bsn_tok|DELETE websearch.fetch"; strings.Join(got, "|") != want {
		t.Errorf("requests = %q", got)
	}
}
