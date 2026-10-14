package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	run := func(path string) (string, error) {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"config", "check", "--config", path})
		err := root.ExecuteContext(context.Background())
		return out.String(), err
	}
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("daemon:\n  listen: 127.0.0.1:7777\n  data_dir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(good); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("good config: out=%q err=%v", out, err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("daemon:\n  listen: 0.0.0.0:7777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(bad); err == nil {
		t.Fatal("expected validation error for non-loopback listen without TLS")
	}
}
