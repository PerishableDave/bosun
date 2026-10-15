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

func TestScopedPolicyApprovalAndBrowserCommands(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/policy", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"defaults":{"read":"allow","write":"ask","destructive":"ask"},"rules":[`+
			`{"capability":"browser.act","decision":"ask","updated_at":"2026-10-12T22:00:00Z"},`+
			`{"capability":"browser.act","scope":"opentable.com","decision":"allow","updated_at":"2026-10-12T22:00:00Z"}],`+
			`"integration_defaults":{"browser.read":"ask"}}`)
	})
	mux.HandleFunc("PUT /v1/policy/{cap}", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Decision string }
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, "PUT "+r.PathValue("cap")+" scope="+r.URL.Query().Get("scope")+" "+body.Decision)
		fmt.Fprintf(w, `{"capability":%q,"scope":%q,"decision":%q,"updated_at":"2026-10-12T22:00:00Z"}`,
			r.PathValue("cap"), r.URL.Query().Get("scope"), body.Decision)
	})
	mux.HandleFunc("DELETE /v1/policy/{cap}", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "DELETE "+r.PathValue("cap")+" scope="+r.URL.Query().Get("scope"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"approvals":[{"id":"appr_1","tool":"browser.submit","capability":"browser.submit","risk":"destructive",`+
			`"scope":"opentable.com","has_preview":true,"args":{},"status":"pending","expires_at":"2026-10-12T22:00:00Z"}]}`)
	})
	mux.HandleFunc("POST /v1/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Decision string
			Always   bool
		}
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, fmt.Sprintf("POST %s %s always=%v", r.PathValue("id"), body.Decision, body.Always))
		fmt.Fprint(w, `{"id":"appr_1","capability":"browser.act","scope":"opentable.com","status":"approved"}`)
	})
	mux.HandleFunc("GET /v1/approvals/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "\x89PNG-preview")
	})
	mux.HandleFunc("GET /v1/browser", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"enabled":true,"running":true,"url":"https://www.opentable.com/","title":"OpenTable","steps":4,"step_budget":60}`)
	})
	mux.HandleFunc("POST /v1/browser/stop", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "STOP")
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
	if out := cli("policy", "list"); !strings.Contains(out, "SITE") || !strings.Contains(out, "browser.act  opentable.com  allow") ||
		!strings.Contains(out, "browser.act  (any)          ask") || !strings.Contains(out, "Integration defaults") ||
		!strings.Contains(out, "browser.read=ask") {
		t.Errorf("policy list:\n%s", out)
	}
	if out := cli("policy", "set", "browser.act", "allow", "--scope", "opentable.com"); !strings.Contains(out, "browser.act on opentable.com: allow") {
		t.Errorf("policy set --scope:\n%s", out)
	}
	cli("policy", "unset", "browser.act", "--scope", "opentable.com")
	if out := cli("approvals", "list"); !strings.Contains(out, "on opentable.com") || !strings.Contains(out, "/v1/approvals/appr_1/preview") {
		t.Errorf("approvals list:\n%s", out)
	}
	if out := cli("approvals", "appr_1", "approve", "--always"); !strings.Contains(out, "browser.act is now allowed on opentable.com") {
		t.Errorf("approve --always:\n%s", out)
	}
	png := filepath.Join(dir, "p.png")
	cli("approvals", "preview", "appr_1", "-o", png)
	if b, _ := os.ReadFile(png); string(b) != "\x89PNG-preview" {
		t.Errorf("preview file = %q", b)
	}
	if out := cli("browser"); !strings.Contains(out, "https://www.opentable.com/") || !strings.Contains(out, "4 of 60") {
		t.Errorf("browser status:\n%s", out)
	}
	cli("browser", "stop")
	want := []string{
		"PUT browser.act scope=opentable.com allow",
		"DELETE browser.act scope=opentable.com",
		"POST appr_1 approve always=true",
		"STOP",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "approvals", "appr_1", "deny", "--always"); err == nil {
		t.Error("deny --always accepted")
	}
}
