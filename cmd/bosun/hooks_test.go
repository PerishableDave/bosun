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

func TestHooksListCommand(t *testing.T) {
	body := ""
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hooks", func(w http.ResponseWriter, r *http.Request) {
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
		out, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "hooks", "list")
		if err != nil {
			t.Fatalf("hooks list: %v\n%s", err, out)
		}
		return out
	}

	body = `{"hooks":{"pre_model_call":[{"name":"builtin.redact_secrets","type":"go"}],
	  "pre_tool_call":[{"name":"script-1-guard","type":"script"}],"post_tool_call":[],"on_approval":[]}}`
	out := cli()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "POINT") ||
		!strings.Contains(lines[1], "pre_model_call") || !strings.Contains(lines[1], "builtin.redact_secrets") ||
		!strings.Contains(lines[2], "script-1-guard") || !strings.Contains(lines[2], "script") {
		t.Errorf("output:\n%s", out)
	}

	body = `{"hooks":{"pre_model_call":[],"pre_tool_call":[],"post_tool_call":[],"on_approval":[]}}`
	if out := cli(); !strings.Contains(out, "No hooks registered") {
		t.Errorf("empty output:\n%s", out)
	}
}
