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

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/doctor"
	"github.com/perishabledave/bosun/internal/vault"
)

func doctorFixture(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "bosun.yaml")
	// Ollama only, pointed at a closed port, so no test talks to the internet.
	yaml := fmt.Sprintf(`daemon: { listen: "127.0.0.1:1", data_dir: %q, master_key: file }
llm: { default: ollama/llama3, providers: { ollama: { base_url: "http://127.0.0.1:1" } } }
`, dataDir)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

func TestDoctorDaemonDown(t *testing.T) {
	old := detectKeychain
	defer func() { detectKeychain = old }()
	detectKeychain = func() vault.Keychain { return nil }
	cfgPath, _ := doctorFixture(t)
	out, err := runCLI(t, "", "--config", cfgPath, "doctor")
	if err == nil {
		t.Fatalf("doctor should fail with the daemon down:\n%s", out)
	}
	for _, want := range []string{
		"ok      config", "fail    daemon", "not reachable", "ok      data dir", "warn    master key",
		"fail    llm ollama", "warn    integrations", "ok      tls", "warn    routines", "disk space", "ok      reverse proxy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	out, _ = runCLI(t, "", "--config", cfgPath, "doctor", "--json")
	var rep struct {
		Status doctor.Status  `json:"status"`
		Checks []doctor.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Status != doctor.Fail || len(rep.Checks) < 8 {
		t.Errorf("json: %v %+v\n%s", err, rep, out)
	}
}

func TestDoctorUsesDaemonReport(t *testing.T) {
	cfgPath, dataDir := doctorFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.HealthResponse{Status: "ok", Version: version})
	})
	mux.HandleFunc("GET /v1/doctor", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(api.ErrorBody{Error: api.ErrorDetail{Code: "forbidden", Message: "admin only"}})
			return
		}
		json.NewEncoder(w).Encode(api.DoctorResponse{Version: version, Status: doctor.Warn, Checks: []doctor.Check{
			{Name: "llm anthropic", Status: doctor.OK, Detail: "key from the vault (llm shared credential); reachable"},
			{Name: "routines", Status: doctor.OK, Detail: "scheduler running; 2 routines (1 enabled)"},
			{Name: "tls", Status: doctor.Warn, Detail: "certificate expires soon"},
		}})
	})
	mux.HandleFunc("GET /v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.IntegrationListResponse{Integrations: []api.IntegrationResponse{
			{Name: "email", Status: "degraded", Message: "imap slow", ToolCount: 3},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tokenPath := filepath.Join(dataDir, "cli-token")
	if err := os.WriteFile(tokenPath, []byte("admin-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, want := range []string{"ok      daemon", "reachable at " + srv.URL, "scheduler running; 2 routines", "warn    tls", "4 ok, 1 warn, 0 fail"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// A member falls back to local checks plus the integration list.
	if err := os.WriteFile(tokenPath, []byte("member-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ = runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "doctor")
	if !strings.Contains(out, "warn    integration email") || !strings.Contains(out, "needs an admin login") {
		t.Errorf("member doctor:\n%s", out)
	}
}

func TestVersionCommand(t *testing.T) {
	cfgPath, _ := doctorFixture(t)
	out, err := runCLI(t, "", "--config", cfgPath, "version")
	if err != nil || !strings.Contains(out, "bosun   "+version) || !strings.Contains(out, "not reachable") {
		t.Fatalf("version: %v\n%s", err, out)
	}
}
