package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/config"
)

func TestStartupChecksEnvFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Daemon.DataDir = dir
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	if err := startupChecks(cfg, log); err != nil {
		t.Fatalf("no env file: %v", err)
	}
	env := filepath.Join(dir, "env")
	if err := os.WriteFile(env, []byte("BOSUN_STARTUP_TEST_KEY=sk-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := startupChecks(cfg, log); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("0644 env file: %v", err)
	}
	os.Chmod(env, 0o600)
	t.Cleanup(func() { os.Unsetenv("BOSUN_STARTUP_TEST_KEY") })
	if err := startupChecks(cfg, log); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("BOSUN_STARTUP_TEST_KEY") != "sk-secret" {
		t.Error("variable not loaded")
	}
	if strings.Contains(logs.String(), "sk-secret") || !strings.Contains(logs.String(), "BOSUN_STARTUP_TEST_KEY") {
		t.Errorf("logs should name the variable, never the value:\n%s", logs.String())
	}

	logs.Reset()
	cfg.Daemon.BehindProxy = true
	cfg.Daemon.Listen = "0.0.0.0:7777"
	if err := startupChecks(cfg, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "without daemon.trusted_proxies") || !strings.Contains(logs.String(), "plain HTTP") {
		t.Errorf("proxy warnings missing:\n%s", logs.String())
	}
}
