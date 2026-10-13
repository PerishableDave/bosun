package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPStatusCommand(t *testing.T) {
	body := ""
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bsn_tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
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
	cli := func() string {
		t.Helper()
		out, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "mcp", "status")
		if err != nil {
			t.Fatalf("mcp status: %v\n%s", err, out)
		}
		return out
	}

	body = `{"integrations":[{"name":"mcp","version":"0.1.0","scope":"shared","status":"degraded","message":"broken failed","tool_count":3,
	  "details":[{"name":"github","transport":"stdio","state":"ready","tools":3,"server_name":"gh","server_version":"1.2","protocol_version":"2025-06-18"},
	             {"name":"broken","transport":"http","state":"failed","error":"HTTP 401: unauthorized","tools":0}]}]}`
	out := cli()
	for _, want := range []string{"MCP bridge 0.1.0: degraded, 3 tool(s) (broken failed)", "github", "ready", "gh 1.2 (protocol 2025-06-18)", "broken", "HTTP 401"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	body = `{"integrations":[{"name":"mcp","version":"0.1.0","scope":"shared","status":"ok","tool_count":3}]}`
	if out := cli(); !strings.Contains(out, "only shown to admins") {
		t.Errorf("member output:\n%s", out)
	}

	body = `{"integrations":[{"name":"websearch","version":"0.1.0","scope":"shared","status":"ok","tool_count":2}]}`
	if out := cli(); !strings.Contains(out, "not enabled") {
		t.Errorf("disabled output:\n%s", out)
	}
}
