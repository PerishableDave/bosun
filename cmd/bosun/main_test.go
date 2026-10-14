package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/store"
)

func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestSetupLoginStatus(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "conf", "bosun.yaml")
	dataDir := filepath.Join(dir, "data")

	out, err := runCLI(t, "dave\nlongenough\nlongenough\n",
		"--config", cfgPath, "setup", "--data-dir", dataDir, "--listen", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, `Created admin user "dave"`) {
		t.Errorf("setup output:\n%s", out)
	}
	fi, err := os.Stat(dataDir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode = %v, %v", fi.Mode().Perm(), err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if ic, ok := cfg.Integrations["websearch"]; !ok || !ic.IsEnabled() {
		t.Errorf("setup should enable websearch: %+v", cfg.Integrations)
	}

	// Re-running setup is idempotent.
	if out, err := runCLI(t, "", "--config", cfgPath, "setup"); err != nil || !strings.Contains(out, "Users already exist") {
		t.Fatalf("second setup: %v\n%s", err, out)
	}

	// Serve the API over the same DB.
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := auth.New(st, auth.Options{})
	s, _ := api.New(api.Options{Auth: a, Bus: events.NewBus(), Version: "t"})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if out, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "status"); err != nil ||
		!strings.Contains(out, "bosund ok") || !strings.Contains(out, "not logged in") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if _, err := runCLI(t, "wrong-password\n", "--config", cfgPath, "--server", srv.URL, "login", "-u", "dave"); err == nil {
		t.Fatal("login with wrong password succeeded")
	}
	if out, err := runCLI(t, "longenough\n", "--config", cfgPath, "--server", srv.URL, "login", "-u", "dave"); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	fi, err = os.Stat(cfg.CLITokenPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, %v", fi.Mode().Perm(), err)
	}
	if out, err := runCLI(t, "", "--config", cfgPath, "--server", srv.URL, "status"); err != nil ||
		!strings.Contains(out, "logged in as dave (admin)") {
		t.Fatalf("status after login: %v\n%s", err, out)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "chat"); err == nil {
		t.Error("chat stub should fail")
	}
}
