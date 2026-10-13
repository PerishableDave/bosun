package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeKeychain is an in-memory Keychain.
type fakeKeychain struct {
	mu      sync.Mutex
	items   map[string]string
	getErr  error // returned by Get when set
	setErr  error
	sets    int
	corrupt bool // Set stores something else
}

func newFakeKeychain() *fakeKeychain { return &fakeKeychain{items: map[string]string{}} }

func (f *fakeKeychain) Name() string { return "fake keychain" }

func (f *fakeKeychain) Get(_ context.Context, svc, acct string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.items[svc+"/"+acct]
	if !ok {
		return "", ErrKeychainItemNotFound
	}
	return v, nil
}

func (f *fakeKeychain) Set(_ context.Context, svc, acct, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.sets++
	if f.corrupt {
		secret = "garbage"
	}
	f.items[svc+"/"+acct] = secret
	return nil
}

func (f *fakeKeychain) Delete(_ context.Context, svc, acct string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[svc+"/"+acct]; !ok {
		return ErrKeychainItemNotFound
	}
	delete(f.items, svc+"/"+acct)
	return nil
}

func TestKeychainKeySourceGeneratesAndCaches(t *testing.T) {
	ctx := context.Background()
	kc := newFakeKeychain()
	ks := &KeychainKeySource{Keychain: kc}
	k1, err := ks.Key(ctx)
	if err != nil || len(k1) != KeySize {
		t.Fatalf("Key = %x, %v", k1, err)
	}
	k2, _ := ks.Key(ctx)
	if !bytes.Equal(k1, k2) || kc.sets != 1 {
		t.Fatalf("key not cached: sets=%d", kc.sets)
	}
	// A fresh source reads the stored key.
	k3, err := (&KeychainKeySource{Keychain: kc}).Key(ctx)
	if err != nil || !bytes.Equal(k1, k3) || kc.sets != 1 {
		t.Fatalf("reload: %x, %v, sets=%d", k3, err, kc.sets)
	}
}

func TestKeychainKeySourceImportsFileAndLeavesIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fk := NewFileKeySource(dir)
	want, err := fk.Key(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kc := newFakeKeychain()
	got, err := (&KeychainKeySource{Keychain: kc, ImportFile: fk.Path}).Key(ctx)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("import: %x, %v (want %x)", got, err, want)
	}
	if _, err := os.Stat(fk.Path); err != nil {
		t.Errorf("key file removed: %v", err)
	}
}

func TestKeychainKeySourceRejectsBadData(t *testing.T) {
	ctx := context.Background()
	kc := newFakeKeychain()
	kc.items["bosun/master-key"] = "not base64!"
	if _, err := (&KeychainKeySource{Keychain: kc}).Key(ctx); err == nil {
		t.Error("accepted non-base64 key")
	}
	kc.items["bosun/master-key"] = "AAAA"
	if _, err := (&KeychainKeySource{Keychain: kc}).Key(ctx); err == nil {
		t.Error("accepted short key")
	}
	// A Set that does not round-trip is an error.
	kc2 := newFakeKeychain()
	kc2.corrupt = true
	if _, err := (&KeychainKeySource{Keychain: kc2}).Key(ctx); err == nil {
		t.Error("accepted key that did not round-trip")
	}
}

func TestSelectKeySource(t *testing.T) {
	ctx := context.Background()
	none := func() Keychain { return nil }

	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		kc := newFakeKeychain()
		ks, desc, err := SelectKeySource(ctx, KeySourceOptions{Mode: "file", DataDir: dir, Detect: func() Keychain { return kc }})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ks.(*FileKeySource); !ok || !strings.Contains(desc, "master.key") {
			t.Errorf("got %T %q", ks, desc)
		}
		if len(kc.items) != 0 {
			t.Error("file mode touched the keychain")
		}
	})
	t.Run("auto without keychain", func(t *testing.T) {
		dir := t.TempDir()
		ks, desc, err := SelectKeySource(ctx, KeySourceOptions{DataDir: dir, Detect: none})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ks.(*FileKeySource); !ok || !strings.Contains(desc, "no OS keychain") {
			t.Errorf("got %T %q", ks, desc)
		}
	})
	t.Run("keychain required but missing", func(t *testing.T) {
		if _, _, err := SelectKeySource(ctx, KeySourceOptions{Mode: "keychain", DataDir: t.TempDir(), Detect: none}); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("bad mode", func(t *testing.T) {
		if _, _, err := SelectKeySource(ctx, KeySourceOptions{Mode: "tpm", DataDir: t.TempDir(), Detect: none}); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("auto migrates file into keychain", func(t *testing.T) {
		dir := t.TempDir()
		want, err := NewFileKeySource(dir).Key(ctx)
		if err != nil {
			t.Fatal(err)
		}
		kc := newFakeKeychain()
		ks, desc, err := SelectKeySource(ctx, KeySourceOptions{DataDir: dir, Detect: func() Keychain { return kc }})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ks.(*KeychainKeySource); !ok || !strings.Contains(desc, "fake keychain") {
			t.Fatalf("got %T %q", ks, desc)
		}
		got, _ := ks.Key(ctx)
		if !bytes.Equal(got, want) {
			t.Error("migrated key differs from file key")
		}
		if _, err := os.Stat(filepath.Join(dir, MasterKeyFile)); err != nil {
			t.Error("key file was removed")
		}
		if b, _ := os.ReadFile(filepath.Join(dir, KeySourceMarkerFile)); strings.TrimSpace(string(b)) != "keychain" {
			t.Errorf("marker = %q", b)
		}

		// Later the keychain breaks: auto mode must not fall back to the
		// file silently, because the marker says the key lives there.
		kc.getErr = errors.New("user interaction is not allowed")
		if _, _, err := SelectKeySource(ctx, KeySourceOptions{DataDir: dir, Detect: func() Keychain { return kc }}); err == nil {
			t.Error("auto fell back to file despite marker")
		}
		if _, _, err := SelectKeySource(ctx, KeySourceOptions{DataDir: dir, Detect: none}); err == nil {
			t.Error("auto fell back to file with no keychain despite marker")
		}
	})
	t.Run("auto falls back when keychain unreachable", func(t *testing.T) {
		dir := t.TempDir()
		kc := newFakeKeychain()
		kc.getErr = errors.New("no D-Bus session")
		ks, desc, err := SelectKeySource(ctx, KeySourceOptions{DataDir: dir, Detect: func() Keychain { return kc }})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ks.(*FileKeySource); !ok || !strings.Contains(desc, "keychain unavailable") {
			t.Errorf("got %T %q", ks, desc)
		}
		if _, _, err := SelectKeySource(ctx, KeySourceOptions{Mode: "keychain", DataDir: dir, Detect: func() Keychain { return kc }}); err == nil {
			t.Error("keychain mode accepted unreachable keychain")
		}
	})
}

// fakeRunner records commands and answers from a script.
type fakeRunner struct {
	calls []string
	stdin []string
	reply func(name string, args []string, stdin []byte) ([]byte, error)
}

func (f *fakeRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	f.stdin = append(f.stdin, string(stdin))
	return f.reply(name, args, stdin)
}

func TestSecurityCLI(t *testing.T) {
	ctx := context.Background()
	stored := ""
	r := &fakeRunner{reply: func(name string, args []string, stdin []byte) ([]byte, error) {
		switch args[0] {
		case "find-generic-password":
			if stored == "" {
				return nil, &ExitError{Code: 44, Stderr: "The specified item could not be found in the keychain."}
			}
			return []byte(stored + "\n"), nil
		case "-i":
			f := strings.Fields(string(stdin))
			stored = f[len(f)-1]
			return nil, nil
		case "delete-generic-password":
			if stored == "" {
				return nil, &ExitError{Code: 44}
			}
			stored = ""
			return nil, nil
		}
		return nil, &ExitError{Code: 2, Stderr: "bad"}
	}}
	s := &SecurityCLI{Runner: r}
	if _, err := s.Get(ctx, "bosun", "master-key"); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Fatalf("Get empty = %v", err)
	}
	if err := s.Set(ctx, "bosun", "master-key", "c2VjcmV0+/=="); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "c2VjcmV0") {
			t.Errorf("secret on argv: %q", c)
		}
	}
	if got, err := s.Get(ctx, "bosun", "master-key"); err != nil || got != "c2VjcmV0+/==" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Set(ctx, "bosun", "master-key", "has space"); err == nil {
		t.Error("unsafe secret accepted")
	}
	if err := s.Delete(ctx, "bosun", "master-key"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "bosun", "master-key"); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Errorf("Delete absent = %v", err)
	}
}

func TestSecretTool(t *testing.T) {
	ctx := context.Background()
	stored := ""
	r := &fakeRunner{reply: func(name string, args []string, stdin []byte) ([]byte, error) {
		switch args[0] {
		case "lookup":
			if stored == "" {
				return nil, &ExitError{Code: 1}
			}
			return []byte(stored), nil
		case "store":
			stored = string(stdin)
			return nil, nil
		case "clear":
			stored = ""
			return nil, nil
		}
		return nil, &ExitError{Code: 2}
	}}
	s := &SecretTool{Runner: r}
	if _, err := s.Get(ctx, "bosun", "master-key"); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Fatalf("Get empty = %v", err)
	}
	if err := s.Set(ctx, "bosun", "master-key", "secret value"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "secret value") {
			t.Errorf("secret on argv: %q", c)
		}
	}
	if got, err := s.Get(ctx, "bosun", "master-key"); err != nil || got != "secret value" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete(ctx, "bosun", "master-key"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "bosun", "master-key"); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Errorf("Delete absent = %v", err)
	}
	// A D-Bus failure is not "not found".
	r.reply = func(string, []string, []byte) ([]byte, error) {
		return nil, &ExitError{Code: 1, Stderr: "Cannot autolaunch D-Bus without X11 $DISPLAY"}
	}
	if _, err := s.Get(ctx, "bosun", "master-key"); err == nil || errors.Is(err, ErrKeychainItemNotFound) {
		t.Errorf("Get with D-Bus failure = %v", err)
	}
}

// TestRealKeychain exercises the platform keychain with a throwaway item.
// It is opt-in: set BOSUN_TEST_KEYCHAIN=1 (macOS may show an access prompt).
func TestRealKeychain(t *testing.T) {
	if os.Getenv("BOSUN_TEST_KEYCHAIN") != "1" {
		t.Skip("set BOSUN_TEST_KEYCHAIN=1 to test the real OS keychain")
	}
	kc := DetectKeychain(ExecRunner{})
	if kc == nil {
		t.Skip("no OS keychain tool on this platform")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	svc := "bosun-test-" + hex.EncodeToString(b)
	t.Cleanup(func() { _ = kc.Delete(context.Background(), svc, DefaultKeychainAccount) })

	ks := &KeychainKeySource{Keychain: kc, Service: svc}
	k1, err := ks.Key(ctx)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := (&KeychainKeySource{Keychain: kc, Service: svc}).Key(ctx)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("reload = %x, %v", k2, err)
	}
	if err := kc.Delete(ctx, svc, DefaultKeychainAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := kc.Get(ctx, svc, DefaultKeychainAccount); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Errorf("Get after delete = %v", err)
	}
}
