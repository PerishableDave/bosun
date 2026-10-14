package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/vault"
)

// memKeychain is an in-memory vault.Keychain.
type memKeychain map[string]string

func (m memKeychain) Name() string { return "test keychain" }
func (m memKeychain) Get(_ context.Context, s, a string) (string, error) {
	v, ok := m[s+"/"+a]
	if !ok {
		return "", vault.ErrKeychainItemNotFound
	}
	return v, nil
}
func (m memKeychain) Set(_ context.Context, s, a, v string) error { m[s+"/"+a] = v; return nil }
func (m memKeychain) Delete(_ context.Context, s, a string) error { delete(m, s+"/"+a); return nil }

// newBackupFixture builds a config and a populated data dir: a user, a
// file master key, integration state, a TLS dir, an env file, plus files a
// backup must leave out (CLI token, logs).
func newBackupFixture(t *testing.T, masterKey string) (cfgPath string, cfg *config.Config) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	cfgPath = filepath.Join(dir, "conf", "bosun.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf("# my config\ndaemon: { listen: \"127.0.0.1:1\", data_dir: %q, master_key: %s }\n", data, masterKey)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := auth.New(st, auth.Options{Argon2: &auth.Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}})
	if _, err := a.CreateUser(ctx, "dave", "longenough", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if masterKey == "file" {
		fk := vault.NewFileKeySource(data)
		key, err := fk.Key(ctx)
		if err != nil {
			t.Fatal(err)
		}
		v := vault.New(st, vault.StaticKeySource(key))
		if err := v.Put(ctx, nil, "llm", "anthropic", integration.Credential{Kind: "anthropic", Secret: "sk-test"}); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"integrations/email/poll_state.json": `{"1/INBOX":{"uidvalidity":1,"last_uid":5}}`,
		"tls/cert.pem":                       "CERT",
		"env":                                "ANTHROPIC_API_KEY=sk-env\n",
		"cli-token":                          "session-token",
		"logs/bosund.log":                    "log line\n",
	} {
		p := filepath.Join(data, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfgPath, cfg
}

func archiveEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = b
	}
	return out
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	cfgPath, cfg := newBackupFixture(t, "file")
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	out, err := runCLI(t, "", "--config", cfgPath, "backup", "--out", archive)
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	fi, err := os.Stat(archive)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode: %v %v", fi, err)
	}
	ents := archiveEntries(t, archive)
	var names []string
	for n := range ents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, want := range []string{"bosun-backup.json", "bosun.yaml", "data/bosun.db", "data/master.key", "data/env",
		"data/integrations/email/poll_state.json", "data/tls/cert.pem"} {
		if _, ok := ents[want]; !ok {
			t.Errorf("archive lacks %s (has %v)", want, names)
		}
	}
	for _, bad := range []string{"data/cli-token", "data/logs/bosund.log", "data/bosun.db-wal", "data/bosun.db-shm"} {
		if _, ok := ents[bad]; ok {
			t.Errorf("archive should not contain %s", bad)
		}
	}
	var m backupManifest
	if err := json.Unmarshal(ents["bosun-backup.json"], &m); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.LatestSchemaVersion()
	if m.SchemaVersion != latest || m.MasterKey != keyInArchive || m.Format != backupFormat {
		t.Errorf("manifest = %+v", m)
	}

	// Restore into a fresh location.
	dir := t.TempDir()
	newData := filepath.Join(dir, "restored-data")
	newCfg := filepath.Join(dir, "restored.yaml")
	out, err = runCLI(t, "", "--config", newCfg, "restore", archive, "--data-dir", newData)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	rc, err := config.Load(newCfg)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Daemon.DataDir != newData {
		t.Errorf("restored data_dir = %s", rc.Daemon.DataDir)
	}
	for _, f := range []string{"master.key", "env", "integrations/email/poll_state.json", "tls/cert.pem"} {
		want, _ := os.ReadFile(filepath.Join(cfg.Daemon.DataDir, f))
		got, err := os.ReadFile(filepath.Join(newData, f))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %q, %v", f, got, err)
		}
		if fi, _ := os.Stat(filepath.Join(newData, f)); fi == nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v", f, fi)
		}
	}
	if _, err := os.Stat(filepath.Join(newData, "cli-token")); !os.IsNotExist(err) {
		t.Error("cli-token restored")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, rc.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetUserByUsername(ctx, "dave"); err != nil {
		t.Errorf("restored user: %v", err)
	}
	// The credential decrypts with the restored key.
	key, err := vault.NewFileKeySource(newData).Key(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := vault.New(st, vault.StaticKeySource(key)).Get(ctx, nil, "llm", "anthropic")
	st.Close()
	if err != nil || c.Secret != "sk-test" {
		t.Errorf("restored credential: %+v %v", c, err)
	}

	// A second restore over the non-empty data dir needs --force, which
	// moves the old one aside.
	if _, err := runCLI(t, "", "--config", newCfg, "restore", archive, "--data-dir", newData); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("restore over non-empty dir: %v", err)
	}
	if out, err := runCLI(t, "", "--config", newCfg, "restore", archive, "--data-dir", newData, "--force"); err != nil {
		t.Fatalf("forced restore: %v\n%s", err, out)
	}
	matches, _ := filepath.Glob(newData + ".before-restore-*")
	if len(matches) != 1 {
		t.Errorf("old data dir not moved aside: %v", matches)
	}
}

func TestRestoreRefusesNewerSchema(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "future.tar.gz")
	f, _ := os.Create(archive)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	mb, _ := json.Marshal(backupManifest{Format: backupFormat, SchemaVersion: 9999})
	tw.WriteHeader(&tar.Header{Name: backupManifestName, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg})
	tw.Write(mb)
	tw.Close()
	gz.Close()
	f.Close()
	dir := t.TempDir()
	_, err := runCLI(t, "", "--config", filepath.Join(dir, "c.yaml"), "restore", archive, "--data-dir", filepath.Join(dir, "d"))
	if err == nil || !strings.Contains(err.Error(), "schema version 9999") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d")); !os.IsNotExist(err) {
		t.Error("data dir created for a refused archive")
	}
}

func TestRestoreRejectsUnsafePaths(t *testing.T) {
	for _, name := range []string{"../evil", "/abs", "data/../../x", ""} {
		if _, err := safeEntryName(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if n, err := safeEntryName("data/integrations/x"); err != nil || n != "data/integrations/x" {
		t.Errorf("good name: %q %v", n, err)
	}
}

func TestBackupKeychainKey(t *testing.T) {
	old := detectKeychain
	defer func() { detectKeychain = old }()
	key := bytes.Repeat([]byte{7}, vault.KeySize)
	kc := memKeychain{vault.DefaultKeychainService + "/" + vault.DefaultKeychainAccount: base64.StdEncoding.EncodeToString(key)}
	detectKeychain = func() vault.Keychain { return kc }

	cfgPath, cfg := newBackupFixture(t, "keychain")
	if err := os.WriteFile(filepath.Join(cfg.Daemon.DataDir, vault.KeySourceMarkerFile), []byte("keychain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.tar.gz")
	out, err := runCLI(t, "", "--config", cfgPath, "backup", "--out", plain)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "WARNING: the vault master key is in the OS keychain and is NOT in this backup") {
		t.Errorf("missing keychain warning:\n%s", out)
	}
	ents := archiveEntries(t, plain)
	if _, ok := ents["data/master.key"]; ok {
		t.Error("key in archive without --include-keychain-key")
	}

	withKey := filepath.Join(dir, "withkey.tar.gz")
	if out, err := runCLI(t, "", "--config", cfgPath, "backup", "--out", withKey, "--include-keychain-key"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ents = archiveEntries(t, withKey)
	if !bytes.Equal(ents["data/master.key"], key) {
		t.Errorf("exported key = %x", ents["data/master.key"])
	}
	var m backupManifest
	json.Unmarshal(ents["bosun-backup.json"], &m)
	if m.MasterKey != keyExported {
		t.Errorf("manifest key state = %s", m.MasterKey)
	}
	if fi, _ := os.Stat(withKey); fi.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %v", fi.Mode())
	}
}
