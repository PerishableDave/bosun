package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/store"
)

type fx struct {
	st   *store.Store
	v    *Vault
	a, b int64
	dir  string
}

func setup(t *testing.T) *fx {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := st.CreateUser(ctx, "alice", "h", "member")
	b, _ := st.CreateUser(ctx, "bob", "h", "member")
	return &fx{st: st, v: New(st, NewFileKeySource(dir)), a: a.ID, b: b.ID, dir: dir}
}

func ptr(v int64) *int64 { return &v }

func TestPutGetDeleteList(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	cred := integration.Credential{Kind: "apikey", Secret: "s3cret", Fields: map[string]string{"user": "alice"}, Expiry: exp}
	if err := f.v.Put(ctx, ptr(f.a), "email", "", cred); err != nil {
		t.Fatal(err)
	}
	got, err := f.v.Get(ctx, ptr(f.a), "email", "apikey")
	if err != nil || got.Secret != "s3cret" || got.Fields["user"] != "alice" || !got.Expiry.Equal(exp) || got.Kind != "apikey" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	// Ciphertext at rest does not contain the secret.
	row, _ := f.st.GetCredential(ctx, ptr(f.a), "email", "apikey")
	if bytes.Contains(row.Ciphertext, []byte("s3cret")) {
		t.Error("secret stored in plaintext")
	}
	// Replace.
	if err := f.v.Put(ctx, ptr(f.a), "email", "apikey", integration.Credential{Secret: "new"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.v.Get(ctx, ptr(f.a), "email", "apikey"); got.Secret != "new" {
		t.Errorf("replace: %+v", got)
	}
	if err := f.v.Put(ctx, nil, "websearch", "apikey", integration.Credential{Secret: "shared"}); err != nil {
		t.Fatal(err)
	}
	infos, err := f.v.List(ctx, ptr(f.a), "")
	if err != nil || len(infos) != 1 || infos[0].Integration != "email" {
		t.Errorf("List alice = %+v, %v", infos, err)
	}
	if infos, _ := f.v.List(ctx, nil, ""); len(infos) != 1 || infos[0].Integration != "websearch" || infos[0].UserID != nil {
		t.Errorf("List shared = %+v", infos)
	}
	if err := f.v.Delete(ctx, ptr(f.a), "email", "apikey"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.v.Get(ctx, ptr(f.a), "email", "apikey"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if err := f.v.Delete(ctx, ptr(f.a), "email", "apikey"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
}

func TestPerUserSourceIsolation(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if err := f.v.Put(ctx, ptr(f.b), "email", "password", integration.Credential{Secret: "bob-secret"}); err != nil {
		t.Fatal(err)
	}
	src := f.v.SourceFor("email", integration.ScopePerUser)

	// Alice (as set by the dispatcher) cannot read Bob's credential.
	if c, err := src.Get(integration.WithUserID(ctx, f.a), "password"); !errors.Is(err, integration.ErrNoCredential) || c.Secret != "" {
		t.Fatalf("alice read bob's credential: %+v, %v", c, err)
	}
	// No user in ctx → no credential.
	if _, err := src.Get(ctx, "password"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("no user: %v", err)
	}
	if _, err := src.Get(integration.WithUserID(ctx, 0), "password"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("user 0: %v", err)
	}
	// Bob can.
	if c, err := src.Get(integration.WithUserID(ctx, f.b), "password"); err != nil || c.Secret != "bob-secret" {
		t.Errorf("bob: %+v %v", c, err)
	}
	// Another integration's source cannot read it either.
	other := f.v.SourceFor("calendar", integration.ScopePerUser)
	if _, err := other.Get(integration.WithUserID(ctx, f.b), "password"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("cross-integration: %v", err)
	}
	// A per-user source never falls back to a shared credential.
	if err := f.v.Put(ctx, nil, "email", "password", integration.Credential{Secret: "shared"}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Get(integration.WithUserID(ctx, f.a), "password"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("per-user fell back to shared: %v", err)
	}
}

func TestSourceKinds(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	for _, k := range []string{"ics:work", "password", "ics:family"} {
		if err := f.v.Put(ctx, ptr(f.a), "calendar", k, integration.Credential{Secret: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.v.Put(ctx, ptr(f.b), "calendar", "ics:bob", integration.Credential{Secret: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := f.v.Put(ctx, ptr(f.a), "email", "password", integration.Credential{Secret: "x"}); err != nil {
		t.Fatal(err)
	}
	src := f.v.SourceFor("calendar", integration.ScopePerUser).(integration.CredentialKindLister)
	got, err := src.Kinds(integration.WithUserID(ctx, f.a))
	if err != nil || strings.Join(got, ",") != "ics:family,ics:work,password" {
		t.Errorf("alice kinds = %v, %v", got, err)
	}
	if got, _ := src.Kinds(integration.WithUserID(ctx, f.b)); strings.Join(got, ",") != "ics:bob" {
		t.Errorf("bob kinds = %v", got)
	}
	if _, err := src.Kinds(ctx); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("no user: %v", err)
	}
}

func TestSharedSource(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if err := f.v.Put(ctx, nil, "websearch", "apikey", integration.Credential{Secret: "house"}); err != nil {
		t.Fatal(err)
	}
	if err := f.v.Put(ctx, ptr(f.a), "websearch", "apikey", integration.Credential{Secret: "alice-own"}); err != nil {
		t.Fatal(err)
	}
	src := f.v.SourceFor("websearch", integration.ScopeShared)
	for _, c := range []context.Context{ctx, integration.WithUserID(ctx, f.a), integration.WithUserID(ctx, f.b)} {
		if got, err := src.Get(c, "apikey"); err != nil || got.Secret != "house" {
			t.Errorf("shared Get = %+v, %v", got, err)
		}
	}
	if _, err := src.Get(ctx, "oauth2:google"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("missing kind: %v", err)
	}
	if _, err := f.v.SourceFor("websearch", "bogus").Get(ctx, "apikey"); !errors.Is(err, integration.ErrNoCredential) {
		t.Errorf("bogus scope: %v", err)
	}
}

func TestTamperedRowFailsDecrypt(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if err := f.v.Put(ctx, ptr(f.b), "email", "password", integration.Credential{Secret: "bob-secret"}); err != nil {
		t.Fatal(err)
	}
	// Copy Bob's ciphertext into Alice's row: AAD binding must reject it.
	row, _ := f.st.GetCredential(ctx, ptr(f.b), "email", "password")
	if _, err := f.st.UpsertCredential(ctx, store.Credential{UserID: ptr(f.a), Integration: "email", Kind: "password",
		Nonce: row.Nonce, Ciphertext: row.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if c, err := f.v.Get(ctx, ptr(f.a), "email", "password"); err == nil || c.Secret != "" {
		t.Errorf("moved ciphertext decrypted: %+v", c)
	}
	// Flipped bit.
	ct := append([]byte(nil), row.Ciphertext...)
	ct[0] ^= 1
	f.st.UpsertCredential(ctx, store.Credential{UserID: ptr(f.b), Integration: "email", Kind: "password", Nonce: row.Nonce, Ciphertext: ct})
	if _, err := f.v.Get(ctx, ptr(f.b), "email", "password"); err == nil {
		t.Error("tampered ciphertext decrypted")
	}
}

func TestWrongKey(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if err := f.v.Put(ctx, ptr(f.a), "email", "apikey", integration.Credential{Secret: "x"}); err != nil {
		t.Fatal(err)
	}
	other := New(f.st, StaticKeySource(bytes.Repeat([]byte{7}, KeySize)))
	if _, err := other.Get(ctx, ptr(f.a), "email", "apikey"); err == nil {
		t.Error("decrypted with wrong key")
	}
}

func TestFileKeySource(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	ks := NewFileKeySource(dir)
	k1, err := ks.Key(ctx)
	if err != nil || len(k1) != KeySize {
		t.Fatalf("Key = %d bytes, %v", len(k1), err)
	}
	st, err := os.Stat(ks.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode().Perm(), err)
	}
	// A fresh source reads the same key.
	k2, err := NewFileKeySource(dir).Key(ctx)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Errorf("reload mismatch: %v", err)
	}
	// Insecure permissions are refused.
	os.Chmod(ks.Path, 0o644)
	if _, err := NewFileKeySource(dir).Key(ctx); err == nil {
		t.Error("accepted world-readable key")
	}
	// Wrong length is refused.
	os.WriteFile(ks.Path, []byte("short"), 0o600)
	os.Chmod(ks.Path, 0o600)
	if _, err := NewFileKeySource(dir).Key(ctx); err == nil {
		t.Error("accepted short key")
	}
}

func TestPutValidation(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if err := f.v.Put(ctx, ptr(0), "email", "apikey", integration.Credential{Secret: "x"}); err == nil {
		t.Error("user 0 accepted")
	}
	if err := f.v.Put(ctx, ptr(f.a), "", "apikey", integration.Credential{Secret: "x"}); err == nil {
		t.Error("empty integration accepted")
	}
	if err := f.v.Put(ctx, ptr(f.a), "email", "", integration.Credential{Secret: "x"}); err == nil {
		t.Error("empty kind accepted")
	}
}
